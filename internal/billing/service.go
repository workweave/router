package billing

import (
	"context"
	"math"

	"workweave/router/internal/observability"
	"workweave/router/internal/router/catalog"
)

// hasOverrideContextKeyT lives in billing (not middleware/proxy) so both
// sides can reference it without a layering violation.
type hasOverrideContextKeyT struct{}

// HasOverrideContextKey flags override-pass-through requests for the
// proxy's debit hook. Set by middleware.WithBalanceCheck. Bool value.
var HasOverrideContextKey = hasOverrideContextKeyT{}

// HasOverrideFromContext returns true when WithBalanceCheck flagged the
// current request as a billing-override pass-through.
func HasOverrideFromContext(ctx context.Context) bool {
	v := ctx.Value(HasOverrideContextKey)
	if v == nil {
		return false
	}
	b, _ := v.(bool)
	return b
}

// EntryTypeInference is the canonical entry_type for per-request debits.
// Keep in sync with the CHECK constraint on
// router.organization_credit_ledger.entry_type.
const EntryTypeInference = "inference"

// MinBalanceMicros: requests 402 when balance <= this. 0 matches
// OpenAI/Anthropic prepaid semantics — block at zero, let in-flight
// debits settle.
const MinBalanceMicros int64 = 0

// Service orchestrates balance reads and debits. No I/O of its own — all
// persistence flows through the Repo interface.
type Service struct {
	repo    Repo
	autopay AutopayNotifier
}

// NewService constructs a billing service. The Repo is required; nil panics
// at request time, so the composition root must guard against it.
func NewService(repo Repo) *Service {
	return &Service{repo: repo}
}

// AutopayNotifier signals the control plane that an org's balance just
// crossed below its autopay threshold. Implemented by a Pub/Sub adapter in
// internal/pubsub; nil disables the crossing check (selfhosted, or topic
// env unset).
type AutopayNotifier interface {
	NotifyRechargeNeeded(organizationID string)
}

// WithAutopayNotifier attaches the autopay recharge signaller and returns
// the service for chaining. Wired only in managed mode.
func (s *Service) WithAutopayNotifier(n AutopayNotifier) *Service {
	s.autopay = n
	return s
}

// CheckResult is the outcome of a preflight balance check. HasOverride
// tells the middleware to skip the threshold comparison and flag the
// request context so the debit hook writes a delta=0 ledger row.
type CheckResult struct {
	HasOverride   bool
	BalanceMicros int64
}

// APIKeySpendCapResult is the outcome of a preflight per-key spend-cap
// check. Found is false if the key was deleted mid-request; CapMicros is
// nil for an uncapped key. Middleware blocks when Found && CapMicros !=
// nil && SpentMicros >= *CapMicros.
type APIKeySpendCapResult struct {
	Found       bool
	SpentMicros int64
	CapMicros   *int64
}

// CheckAPIKeySpendCap reads fresh from the repo (not the auth cache) so a
// hot cached key can't overrun its cap within the cache TTL.
func (s *Service) CheckAPIKeySpendCap(ctx context.Context, apiKeyID string) (APIKeySpendCapResult, error) {
	spent, cap, found, err := s.repo.GetAPIKeySpend(ctx, apiKeyID)
	if err != nil {
		return APIKeySpendCapResult{}, err
	}
	return APIKeySpendCapResult{Found: found, SpentMicros: spent, CapMicros: cap}, nil
}

// CheckBalance short-circuits the balance read when an override is active
// (returns HasOverride=true). Otherwise the caller compares BalanceMicros
// against MinBalanceMicros.
//
// ErrBalanceRowMissing means no balance row exists for the org — treat as
// 402; the org needs to be backfilled before inference can succeed.
func (s *Service) CheckBalance(ctx context.Context, orgID string) (CheckResult, error) {
	override, err := s.repo.HasActiveOverride(ctx, orgID)
	if err != nil {
		return CheckResult{}, err
	}
	if override {
		return CheckResult{HasOverride: true}, nil
	}
	balance, err := s.repo.GetBalance(ctx, orgID)
	if err != nil {
		return CheckResult{}, err
	}
	return CheckResult{BalanceMicros: balance}, nil
}

// DebitInferenceParams is the input to DebitForInference. Token counts
// and pricing come from the proxy's usage extractor; HasOverride is
// carried from the context flag middleware already stamped, so the
// override read doesn't happen twice.
type DebitInferenceParams struct {
	OrganizationID  string
	RouterRequestID string
	Model           string
	Provider        string
	InputTokens     int
	OutputTokens    int
	CacheCreation   int
	CacheRead       int
	Pricing         catalog.Pricing
	HasOverride     bool
	// SubscriptionServed: turn ran on the customer's own Anthropic/Codex
	// subscription token, so Weave charges nothing.
	SubscriptionServed bool
	// APIKeyID attributes the debit to the authenticating key for
	// spend-cap tracking; empty leaves per-key spend untouched.
	APIKeyID string
}

// DebitForInference writes one ledger row at cost — no markup math here;
// margin is collected at top-up by the backend. Debits 0 for an override
// pass-through or subscription-served turn (already paid for), but always
// records NotionalCostMicros as a shadow trail.
//
// Returns the post-debit balance (0 on override, since balance doesn't
// change).
func (s *Service) DebitForInference(ctx context.Context, p DebitInferenceParams) (int64, error) {
	notional := computeNotionalMicros(p)
	delta := -notional
	switch {
	case p.HasOverride, p.SubscriptionServed:
		// Already paid for — debit nothing, but still record notional cost below.
		delta = 0
	}
	balanceAfter, err := s.repo.DebitInference(ctx, DebitParams{
		OrganizationID:     p.OrganizationID,
		DeltaUsdMicros:     delta,
		NotionalCostMicros: notional,
		EntryType:          EntryTypeInference,
		RouterRequestID:    p.RouterRequestID,
		RouterModel:        p.Model,
		APIKeyID:           p.APIKeyID,
	})
	if err != nil {
		return balanceAfter, err
	}
	s.maybeSignalRecharge(ctx, p.OrganizationID, delta, balanceAfter)
	return balanceAfter, nil
}

// maybeSignalRecharge fires once, on the debit that crosses the org's
// balance from at-or-above its autopay threshold to below it. No-ops if
// autopay isn't wired, the debit moved nothing, or autopay is disabled.
// A config-read error is logged and dropped (not returned) since the
// control-plane reconciliation sweep backstops a missed signal.
func (s *Service) maybeSignalRecharge(ctx context.Context, orgID string, delta, balanceAfter int64) {
	if s.autopay == nil || delta >= 0 {
		return
	}
	enabled, threshold, err := s.repo.GetAutopayConfig(ctx, orgID)
	if err != nil {
		observability.Get().Warn("Autopay crossing check skipped: config read failed",
			"organization_id", orgID, "err", err)
		return
	}
	if !enabled {
		return
	}
	// delta < 0, so the pre-debit balance is strictly greater than balanceAfter.
	balanceBefore := balanceAfter - delta
	if balanceBefore >= threshold && balanceAfter < threshold {
		s.autopay.NotifyRechargeNeeded(orgID)
	}
}

// computeNotionalMicros returns the would-be charge in USD micros,
// regardless of override status, for the shadow billing trail.
func computeNotionalMicros(p DebitInferenceParams) int64 {
	inUSD := catalog.EffectiveInputCost(p.InputTokens, p.CacheCreation, p.CacheRead, p.Pricing.InputUSDPer1M, p.Pricing, p.Provider)
	outUSD := catalog.EffectiveOutputCost(p.OutputTokens, p.Pricing.OutputUSDPer1M)
	total := inUSD + outUSD
	if math.IsNaN(total) || math.IsInf(total, 0) || total < 0 {
		return 0
	}
	return int64(math.Round(total * 1_000_000))
}
