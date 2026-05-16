package billing_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"workweave/router/internal/billing"
	"workweave/router/internal/providers"
	"workweave/router/internal/router/pricing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRepo is an in-memory billing.Repo for testing the Service without
// hitting Postgres. Atomic fields keep the concurrent-debit test honest.
type fakeRepo struct {
	mu               sync.Mutex
	balanceMicros    int64
	hasOverride      bool
	balanceErr       error
	overrideErr      error
	debitErr         error
	ledgerCalls      []billing.DebitParams
	balanceRowExists bool
	debitCalls       atomic.Int32
}

func (r *fakeRepo) GetBalance(_ context.Context, _ string) (int64, error) {
	if r.balanceErr != nil {
		return 0, r.balanceErr
	}
	if !r.balanceRowExists {
		return 0, billing.ErrBalanceRowMissing
	}
	return r.balanceMicros, nil
}

func (r *fakeRepo) HasActiveOverride(_ context.Context, _ string) (bool, error) {
	if r.overrideErr != nil {
		return false, r.overrideErr
	}
	return r.hasOverride, nil
}

func (r *fakeRepo) DebitInference(_ context.Context, p billing.DebitParams) (int64, error) {
	r.debitCalls.Add(1)
	if r.debitErr != nil {
		return 0, r.debitErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.balanceRowExists {
		return 0, billing.ErrBalanceRowMissing
	}
	r.balanceMicros += p.DeltaUsdMicros
	r.ledgerCalls = append(r.ledgerCalls, p)
	return r.balanceMicros, nil
}

func (r *fakeRepo) BillingTablesExist(_ context.Context) (bool, error) { return true, nil }

func TestCheckBalance_Override(t *testing.T) {
	repo := &fakeRepo{hasOverride: true, balanceRowExists: true, balanceMicros: 5_000_000}
	svc := billing.NewService(repo)
	res, err := svc.CheckBalance(context.Background(), "org_x")
	require.NoError(t, err)
	assert.True(t, res.HasOverride)
	// When override is active the service must not bother reading the
	// balance — the middleware doesn't need it and the row may be missing.
	assert.Equal(t, int64(0), res.BalanceMicros, "balance must be skipped on override path")
}

func TestCheckBalance_Healthy(t *testing.T) {
	repo := &fakeRepo{balanceRowExists: true, balanceMicros: 9_500_000}
	svc := billing.NewService(repo)
	res, err := svc.CheckBalance(context.Background(), "org_x")
	require.NoError(t, err)
	assert.False(t, res.HasOverride)
	assert.Equal(t, int64(9_500_000), res.BalanceMicros)
}

func TestCheckBalance_MissingRowPropagates(t *testing.T) {
	repo := &fakeRepo{balanceRowExists: false}
	svc := billing.NewService(repo)
	_, err := svc.CheckBalance(context.Background(), "org_x")
	assert.ErrorIs(t, err, billing.ErrBalanceRowMissing)
}

func TestDebitForInference_MatchesExportedCostMath(t *testing.T) {
	// The debit hook must compute the same notional cost as the OTel
	// emitter and telemetry writer — they all go through the exported
	// pricing functions now. If this drifts the customer sees a billed
	// amount different from the dashboard cost.
	repo := &fakeRepo{balanceRowExists: true, balanceMicros: 10_000_000}
	svc := billing.NewService(repo)
	p := pricing.Pricing{InputUSDPer1M: 3.00, OutputUSDPer1M: 15.00, CacheReadMultiplier: 0.10}
	balance, err := svc.DebitForInference(context.Background(), billing.DebitInferenceParams{
		OrganizationID:  "org_x",
		RouterRequestID: "req_abc",
		Model:           "claude-sonnet-4-5",
		Provider:        providers.ProviderAnthropic,
		InputTokens:     1_000_000,
		OutputTokens:    250_000,
		Pricing:         p,
	})
	require.NoError(t, err)

	// Expected: 1_000_000 fresh * $3/M + 250_000 * $15/M = 3.00 + 3.75 = 6.75
	expectedMicros := int64(6_750_000)
	assert.Equal(t, int64(10_000_000-expectedMicros), balance, "balance reflects raw upstream cost")
	require.Len(t, repo.ledgerCalls, 1)
	assert.Equal(t, -expectedMicros, repo.ledgerCalls[0].DeltaUsdMicros, "delta is the negative charge")
	assert.Equal(t, expectedMicros, repo.ledgerCalls[0].NotionalCostMicros, "notional matches the charge")
	assert.Equal(t, billing.EntryTypeInference, repo.ledgerCalls[0].EntryType)
	assert.Equal(t, "req_abc", repo.ledgerCalls[0].RouterRequestID)
	assert.Equal(t, "claude-sonnet-4-5", repo.ledgerCalls[0].RouterModel)
}

func TestDebitForInference_OverrideWritesZeroDeltaWithNotional(t *testing.T) {
	// Override path: ledger row must record the would-be charge in
	// notional_cost_micros while leaving the balance untouched. This is
	// the shadow billing trail the plan requires for capacity planning.
	repo := &fakeRepo{balanceRowExists: true, balanceMicros: 0}
	svc := billing.NewService(repo)
	p := pricing.Pricing{InputUSDPer1M: 3.00, OutputUSDPer1M: 15.00, CacheReadMultiplier: 0.10}
	balance, err := svc.DebitForInference(context.Background(), billing.DebitInferenceParams{
		OrganizationID: "org_internal",
		Model:          "claude-sonnet-4-5",
		Provider:       providers.ProviderAnthropic,
		InputTokens:    1_000_000,
		OutputTokens:   250_000,
		Pricing:        p,
		HasOverride:    true,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(0), balance, "override leaves balance unchanged")
	require.Len(t, repo.ledgerCalls, 1)
	assert.Equal(t, int64(0), repo.ledgerCalls[0].DeltaUsdMicros, "override delta must be zero")
	assert.Equal(t, int64(6_750_000), repo.ledgerCalls[0].NotionalCostMicros, "notional records would-be charge")
}

func TestDebitForInference_BalanceCanGoNegative(t *testing.T) {
	// Concurrent-debit semantics: when two requests pass preflight with a
	// thin balance and both debit, the second goes negative. The Service
	// must accept this — no balance>=amount guard. The middleware's
	// min-balance threshold bounds the typical dip.
	repo := &fakeRepo{balanceRowExists: true, balanceMicros: 500_000} // $0.50
	svc := billing.NewService(repo)
	p := pricing.Pricing{InputUSDPer1M: 3.00, OutputUSDPer1M: 15.00, CacheReadMultiplier: 0.10}

	for range 2 {
		_, err := svc.DebitForInference(context.Background(), billing.DebitInferenceParams{
			OrganizationID: "org_x",
			InputTokens:    1_000_000,
			OutputTokens:   250_000,
			Pricing:        p,
			Provider:       providers.ProviderAnthropic,
		})
		require.NoError(t, err)
	}

	// 500_000 - 6_750_000*2 = -13_000_000
	assert.Equal(t, int64(-13_000_000), repo.balanceMicros)
	assert.Len(t, repo.ledgerCalls, 2, "both debits recorded; nothing dropped")
}

func TestDebitForInference_ZeroTokensYieldsZeroCharge(t *testing.T) {
	// A real failure mode: upstream returns 0-token usage (timeouts, 5xx
	// before any tokens were produced). Notional must be 0 and balance
	// unchanged — billing the customer for "0 tokens worth of cost" would
	// be confusing.
	repo := &fakeRepo{balanceRowExists: true, balanceMicros: 5_000_000}
	svc := billing.NewService(repo)
	p := pricing.Pricing{InputUSDPer1M: 3.00, OutputUSDPer1M: 15.00, CacheReadMultiplier: 0.10}
	_, err := svc.DebitForInference(context.Background(), billing.DebitInferenceParams{
		OrganizationID: "org_x",
		Pricing:        p,
		Provider:       providers.ProviderAnthropic,
	})
	require.NoError(t, err)
	require.Len(t, repo.ledgerCalls, 1)
	assert.Equal(t, int64(0), repo.ledgerCalls[0].DeltaUsdMicros)
	assert.Equal(t, int64(0), repo.ledgerCalls[0].NotionalCostMicros)
}

func TestDebitForInference_RepoErrorPropagates(t *testing.T) {
	repo := &fakeRepo{balanceRowExists: true, debitErr: errors.New("conn refused")}
	svc := billing.NewService(repo)
	_, err := svc.DebitForInference(context.Background(), billing.DebitInferenceParams{
		OrganizationID: "org_x",
		Pricing:        pricing.Pricing{InputUSDPer1M: 3.00, OutputUSDPer1M: 15.00},
	})
	assert.Error(t, err)
}

func TestHasOverrideFromContext_Default(t *testing.T) {
	assert.False(t, billing.HasOverrideFromContext(context.Background()))
}

func TestHasOverrideFromContext_True(t *testing.T) {
	ctx := context.WithValue(context.Background(), billing.HasOverrideContextKey, true)
	assert.True(t, billing.HasOverrideFromContext(ctx))
}
