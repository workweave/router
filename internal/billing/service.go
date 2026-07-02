package billing

import (
	"context"
	"math"

	"workweave/router/internal/observability"
	"workweave/router/internal/router/catalog"
)

// hasOverrideContextKeyT is the request-context type for the override
// flag. Defined in the billing package so middleware (adapter) and
// proxy (inner-ring) can both reference it without a layering
// violation.
type hasOverrideContextKeyT struct{}

// HasOverrideContextKey is the request-context key used by
// middleware.WithBalanceCheck to flag override-pass-through requests
// for the proxy's debit hook. Bool value.
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

// EntryTypeInference is the canonical entry_type value for per-request
// debits (and override pass-throughs). Keep in sync with the CHECK
// constraint on router.organization_credit_ledger.entry_type.
const EntryTypeInference = "inference"

// MinBalanceMicros: new requests 402 when balance <= this (USD micros).
// 0 matches OpenAI/Anthropic prepaid semantics — block at zero, let
// in-flight debits settle (post-request balance can dip by one req cost).
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

// AutopayNotifier signals the control plane that an org's balance just crossed
// below its autopay threshold and a recharge should fire. Implemented by a
// Pub/Sub adapter in internal/pubsub; left nil when autopay signalling isn't
// wired (selfhosted, or the autopay topic env is unset), which disables the
// crossing check entirely.
type AutopayNotifier interface {
	NotifyRechargeNeeded(organizationID string)
}

// WithAutopayNotifier attaches the autopay recharge signaller and returns the
// service for chaining. Wired only in managed mode when the autopay topic is
// configured.
func (s *Service) WithAutopayNotifier(n AutopayNotifier) *Service {
	s.autopay = n
	return s
}

// CheckResult is the outcome of a preflight balance check. The middleware
// uses HasOverride to skip the threshold comparison and to flag the request
// context so the debit hook writes a delta=0 ledger row.
type CheckResult struct {
	HasOverride   bool
	BalanceMicros int64
}

// APIKeySpendCapResult is the outcome of a preflight per-key spend-cap check.
// Found is false when the key was deleted mid-request; CapMicros is nil for an
// uncapped key. The middleware blocks when Found && CapMicros != nil &&
// SpentMicros >= *CapMicros.
type APIKeySpendCapResult struct {
	Found       bool
	SpentMicros int64
	CapMicros   *int64
}

// CheckAPIKeySpendCap reads the key's cap and spend-to-date fresh from the repo
// (not the auth cache) so a hot cached key cannot overrun its cap within the
// cache TTL. Returns Found=false when the key no longer exists.
func (s *Service) CheckAPIKeySpendCap(ctx context.Context, apiKeyID string) (APIKeySpendCapResult, error) {
	spent, cap, found, err := s.repo.GetAPIKeySpend(ctx, apiKeyID)
	if err != nil {
		return APIKeySpendCapResult{}, err
	}
	return APIKeySpendCapResult{Found: found, SpentMicros: spent, CapMicros: cap}, nil
}

// CheckBalance reads the override flag and balance row. Returns
// HasOverride=true and short-circuits the balance read when an override is
// active. Caller (middleware) compares BalanceMicros against the
// configured minimum threshold.
//
// Errors:
//   - ErrBalanceRowMissing: no balance row exists for the org. Middleware
//     should treat this as 402 with a distinct log line; the org needs to
//     be backfilled before any inference can succeed.
//   - Other errors propagate from the Repo.
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

// DebitInferenceParams is the input to DebitForInference. Token counts and
// the two pricing rows come from the proxy's usage extractor; orgID +
// requestID + model + provider come from the request context.
//
// HasOverride is set by the caller (proxy) from the per-request context
// flag that middleware stamped on. Plumbing it through here keeps the
// override read out of the hot path — middleware already did it.
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
	// SubscriptionServed is true when the turn was served on the customer's own
	// Anthropic or Codex subscription token. Weave charges nothing for these —
	// the customer's plan already paid for the tokens.
	SubscriptionServed bool
	// APIKeyID, when non-empty, attributes the debit to the api key that
	// authenticated the request so its lifetime spend is tracked for cap
	// enforcement. Empty leaves per-key spend untouched.
	APIKeyID string
}

// DebitForInference computes the raw upstream cost and writes one ledger
// row. For Weave-fronted usage there is no markup math — margin is collected
// at top-up by the backend and the router debits at cost. Two cases debit 0:
// an override pass-through, and a turn served on the customer's own
// subscription (Anthropic or Codex) — their plan already covers the tokens, so
// Weave charges nothing for routing them. NotionalCostMicros always records
// the full would-be cost regardless, as a shadow trail.
//
// Returns the post-debit balance (0 on override since balance is
// unchanged but the ledger row was recorded with the current balance).
func (s *Service) DebitForInference(ctx context.Context, p DebitInferenceParams) (int64, error) {
	notional := computeNotionalMicros(p)
	delta := -notional
	switch {
	case p.HasOverride, p.SubscriptionServed:
		// Override pass-through, or a turn the customer's own subscription
		// already paid for — debit nothing. The notional cost is still recorded
		// below as a shadow billing trail.
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

// maybeSignalRecharge fires exactly one autopay recharge signal on the debit
// that takes the org's balance from at-or-above its configured threshold to
// below it — the single downward crossing. It no-ops when autopay signalling
// isn't wired, the debit moved nothing (override or subscription-served), or
// the org hasn't enabled autopay. The signal is the primary autopay trigger;
// the control-plane reconciliation sweep backstops a dropped signal, so a
// config read error is logged and dropped rather than failing the
// already-served request.
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

// computeNotionalMicros returns the would-be charge in USD micros. Always
// populated, regardless of override status, so the ledger preserves a
// shadow billing trail.
func computeNotionalMicros(p DebitInferenceParams) int64 {
	inUSD := catalog.EffectiveInputCost(p.InputTokens, p.CacheCreation, p.CacheRead, p.Pricing.InputUSDPer1M, p.Pricing, p.Provider)
	outUSD := catalog.EffectiveOutputCost(p.OutputTokens, p.Pricing.OutputUSDPer1M)
	total := inUSD + outUSD
	if math.IsNaN(total) || math.IsInf(total, 0) || total < 0 {
		return 0
	}
	return int64(math.Round(total * 1_000_000))
}
