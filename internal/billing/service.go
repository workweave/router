package billing

import (
	"context"
	"math"

	"workweave/router/internal/router/pricing"
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

// Service orchestrates balance reads and debits. No I/O of its own — all
// persistence flows through the Repo interface.
type Service struct {
	repo Repo
}

// NewService constructs a billing service. The Repo is required; nil panics
// at request time, so the composition root must guard against it.
func NewService(repo Repo) *Service {
	return &Service{repo: repo}
}

// CheckResult is the outcome of a preflight balance check. The middleware
// uses HasOverride to skip the threshold comparison and to flag the request
// context so the debit hook writes a delta=0 ledger row.
type CheckResult struct {
	HasOverride   bool
	BalanceMicros int64
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
	Pricing         pricing.Pricing
	HasOverride     bool
}

// DebitForInference computes the raw upstream cost and writes one ledger
// row. No markup math — margin is collected at top-up by the backend; the
// router debits at cost.
//
// Returns the post-debit balance (0 on override since balance is
// unchanged but the ledger row was recorded with the current balance).
func (s *Service) DebitForInference(ctx context.Context, p DebitInferenceParams) (int64, error) {
	notional := computeNotionalMicros(p)
	delta := -notional
	if p.HasOverride {
		delta = 0
	}
	return s.repo.DebitInference(ctx, DebitParams{
		OrganizationID:     p.OrganizationID,
		DeltaUsdMicros:     delta,
		NotionalCostMicros: notional,
		EntryType:          EntryTypeInference,
		RouterRequestID:    p.RouterRequestID,
		RouterModel:        p.Model,
	})
}

// computeNotionalMicros returns the would-be charge in USD micros. Always
// populated, regardless of override status, so the ledger preserves a
// shadow billing trail.
func computeNotionalMicros(p DebitInferenceParams) int64 {
	inUSD := pricing.EffectiveInputCost(p.InputTokens, p.CacheCreation, p.CacheRead, p.Pricing.InputUSDPer1M, p.Pricing, p.Provider)
	outUSD := pricing.EffectiveOutputCost(p.OutputTokens, p.Pricing.OutputUSDPer1M)
	total := inUSD + outUSD
	if math.IsNaN(total) || math.IsInf(total, 0) || total < 0 {
		return 0
	}
	return int64(math.Round(total * 1_000_000))
}
