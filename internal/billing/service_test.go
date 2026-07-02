package billing_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"workweave/router/internal/billing"
	"workweave/router/internal/providers"
	"workweave/router/internal/router/catalog"

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
	autopayEnabled   bool
	autopayThreshold int64
	autopayErr       error
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

func (r *fakeRepo) GetAPIKeySpend(_ context.Context, _ string) (int64, *int64, bool, error) {
	return 0, nil, false, nil
}

func (r *fakeRepo) GetAutopayConfig(_ context.Context, _ string) (bool, int64, error) {
	if r.autopayErr != nil {
		return false, 0, r.autopayErr
	}
	return r.autopayEnabled, r.autopayThreshold, nil
}

// fakeAutopayNotifier records the org ids the service asked to recharge.
type fakeAutopayNotifier struct {
	mu       sync.Mutex
	notified []string
}

func (n *fakeAutopayNotifier) NotifyRechargeNeeded(organizationID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.notified = append(n.notified, organizationID)
}

func (n *fakeAutopayNotifier) calls() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.notified...)
}

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
	p := catalog.Pricing{InputUSDPer1M: 3.00, OutputUSDPer1M: 15.00, CacheReadMultiplier: 0.10}
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
	p := catalog.Pricing{InputUSDPer1M: 3.00, OutputUSDPer1M: 15.00, CacheReadMultiplier: 0.10}
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

func TestDebitForInference_SubscriptionDebitsNothing(t *testing.T) {
	// Served on the customer's own subscription: their plan already covers the
	// tokens, so Weave charges nothing — the ledger debits 0 while the notional
	// row still records the full would-be cost as a shadow trail.
	repo := &fakeRepo{balanceRowExists: true, balanceMicros: 10_000_000}
	svc := billing.NewService(repo)
	p := catalog.Pricing{InputUSDPer1M: 3.00, OutputUSDPer1M: 15.00, CacheReadMultiplier: 0.10}
	balance, err := svc.DebitForInference(context.Background(), billing.DebitInferenceParams{
		OrganizationID:     "org_sub",
		RouterRequestID:    "req_sub",
		Model:              "claude-opus-4-8",
		Provider:           providers.ProviderAnthropic,
		InputTokens:        1_000_000,
		OutputTokens:       250_000,
		Pricing:            p,
		SubscriptionServed: true,
	})
	require.NoError(t, err)

	assert.Equal(t, int64(10_000_000), balance, "subscription turns debit nothing")
	require.Len(t, repo.ledgerCalls, 1)
	assert.Equal(t, int64(0), repo.ledgerCalls[0].DeltaUsdMicros, "delta is zero — the customer's plan covers the tokens")
	assert.Equal(t, int64(6_750_000), repo.ledgerCalls[0].NotionalCostMicros, "notional still records the full would-be cost")
}

func TestDebitForInference_OverrideBeatsSubscription(t *testing.T) {
	// A comped/override org pays nothing even when the turn was subscription
	// served — override wins the precedence.
	repo := &fakeRepo{balanceRowExists: true, balanceMicros: 5_000_000}
	svc := billing.NewService(repo)
	p := catalog.Pricing{InputUSDPer1M: 3.00, OutputUSDPer1M: 15.00, CacheReadMultiplier: 0.10}
	balance, err := svc.DebitForInference(context.Background(), billing.DebitInferenceParams{
		OrganizationID:     "org_internal",
		Model:              "claude-opus-4-8",
		Provider:           providers.ProviderAnthropic,
		InputTokens:        1_000_000,
		OutputTokens:       250_000,
		Pricing:            p,
		HasOverride:        true,
		SubscriptionServed: true,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(5_000_000), balance, "override leaves balance unchanged even on a subscription turn")
	require.Len(t, repo.ledgerCalls, 1)
	assert.Equal(t, int64(0), repo.ledgerCalls[0].DeltaUsdMicros, "override delta is zero, beating the subscription fee")
	assert.Equal(t, int64(6_750_000), repo.ledgerCalls[0].NotionalCostMicros)
}

func TestDebitForInference_BalanceCanGoNegative(t *testing.T) {
	// Concurrent-debit semantics: when two requests pass preflight with a
	// thin balance and both debit, the second goes negative. The Service
	// must accept this — no balance>=amount guard. The middleware's
	// min-balance threshold bounds the typical dip.
	repo := &fakeRepo{balanceRowExists: true, balanceMicros: 500_000} // $0.50
	svc := billing.NewService(repo)
	p := catalog.Pricing{InputUSDPer1M: 3.00, OutputUSDPer1M: 15.00, CacheReadMultiplier: 0.10}

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
	p := catalog.Pricing{InputUSDPer1M: 3.00, OutputUSDPer1M: 15.00, CacheReadMultiplier: 0.10}
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
		Pricing:        catalog.Pricing{InputUSDPer1M: 3.00, OutputUSDPer1M: 15.00},
	})
	assert.Error(t, err)
}

func TestDebitForInference_AttributesAPIKey(t *testing.T) {
	// The api_key_id and the negative delta both flow to the repo so the CTE
	// can bump the key's lifetime spent counter by the debit magnitude. On a
	// real debit spent should grow by exactly the notional charge (= -delta).
	repo := &fakeRepo{balanceRowExists: true, balanceMicros: 10_000_000}
	svc := billing.NewService(repo)
	p := catalog.Pricing{InputUSDPer1M: 3.00, OutputUSDPer1M: 15.00, CacheReadMultiplier: 0.10}
	_, err := svc.DebitForInference(context.Background(), billing.DebitInferenceParams{
		OrganizationID:  "org_x",
		RouterRequestID: "req_key",
		Model:           "claude-sonnet-4-5",
		Provider:        providers.ProviderAnthropic,
		InputTokens:     1_000_000,
		OutputTokens:    250_000,
		Pricing:         p,
		APIKeyID:        "key-123",
	})
	require.NoError(t, err)
	require.Len(t, repo.ledgerCalls, 1)
	assert.Equal(t, "key-123", repo.ledgerCalls[0].APIKeyID, "api key id flows to the repo for per-key spend")
	// spent grows by -delta: a real debit's delta is negative, so the key's
	// lifetime spend rises by the full notional charge.
	assert.Equal(t, int64(6_750_000), -repo.ledgerCalls[0].DeltaUsdMicros, "per-key spend increment equals the charge")
}

func TestDebitForInference_SubscriptionLeavesKeySpendFlat(t *testing.T) {
	// A subscription-served turn debits 0, so the key's spend must not move —
	// the customer's own plan paid, not their Weave key budget.
	repo := &fakeRepo{balanceRowExists: true, balanceMicros: 10_000_000}
	svc := billing.NewService(repo)
	p := catalog.Pricing{InputUSDPer1M: 3.00, OutputUSDPer1M: 15.00, CacheReadMultiplier: 0.10}
	_, err := svc.DebitForInference(context.Background(), billing.DebitInferenceParams{
		OrganizationID:     "org_sub",
		Model:              "claude-opus-4-8",
		Provider:           providers.ProviderAnthropic,
		InputTokens:        1_000_000,
		OutputTokens:       250_000,
		Pricing:            p,
		APIKeyID:           "key-sub",
		SubscriptionServed: true,
	})
	require.NoError(t, err)
	require.Len(t, repo.ledgerCalls, 1)
	assert.Equal(t, "key-sub", repo.ledgerCalls[0].APIKeyID)
	assert.Equal(t, int64(0), repo.ledgerCalls[0].DeltaUsdMicros, "subscription turn debits 0, so key spend stays flat")
}

func TestDebitForInference_AutopaySignalsOnDownwardCrossing(t *testing.T) {
	// Each debit is $6.75 (1M input + 250k output at $3/$15 per 1M).
	p := catalog.Pricing{InputUSDPer1M: 3.00, OutputUSDPer1M: 15.00, CacheReadMultiplier: 0.10}
	debit := func(svc *billing.Service) error {
		_, err := svc.DebitForInference(context.Background(), billing.DebitInferenceParams{
			OrganizationID: "org_x",
			InputTokens:    1_000_000,
			OutputTokens:   250_000,
			Pricing:        p,
			Provider:       providers.ProviderAnthropic,
		})
		return err
	}

	t.Run("fires once when the debit crosses below the threshold", func(t *testing.T) {
		// $10.00 balance, $5.00 threshold; one $6.75 debit lands at $3.25 (< $5).
		repo := &fakeRepo{balanceRowExists: true, balanceMicros: 10_000_000, autopayEnabled: true, autopayThreshold: 5_000_000}
		notifier := &fakeAutopayNotifier{}
		svc := billing.NewService(repo).WithAutopayNotifier(notifier)
		require.NoError(t, debit(svc))
		assert.Equal(t, []string{"org_x"}, notifier.calls(), "the crossing debit signals exactly once")
	})

	t.Run("does not fire when already below the threshold (below to below)", func(t *testing.T) {
		// $3.25 balance is already under the $5 threshold: the next debit is not a crossing.
		repo := &fakeRepo{balanceRowExists: true, balanceMicros: 3_250_000, autopayEnabled: true, autopayThreshold: 5_000_000}
		notifier := &fakeAutopayNotifier{}
		svc := billing.NewService(repo).WithAutopayNotifier(notifier)
		require.NoError(t, debit(svc))
		assert.Empty(t, notifier.calls(), "below→below must not re-fire; that's the transition guard")
	})

	t.Run("does not fire when the debit stays above the threshold", func(t *testing.T) {
		// $100.00 → $93.25, still comfortably above $5.
		repo := &fakeRepo{balanceRowExists: true, balanceMicros: 100_000_000, autopayEnabled: true, autopayThreshold: 5_000_000}
		notifier := &fakeAutopayNotifier{}
		svc := billing.NewService(repo).WithAutopayNotifier(notifier)
		require.NoError(t, debit(svc))
		assert.Empty(t, notifier.calls())
	})

	t.Run("does not fire when autopay is disabled", func(t *testing.T) {
		repo := &fakeRepo{balanceRowExists: true, balanceMicros: 10_000_000, autopayEnabled: false, autopayThreshold: 5_000_000}
		notifier := &fakeAutopayNotifier{}
		svc := billing.NewService(repo).WithAutopayNotifier(notifier)
		require.NoError(t, debit(svc))
		assert.Empty(t, notifier.calls())
	})

	t.Run("no notifier wired is a no-op", func(t *testing.T) {
		repo := &fakeRepo{balanceRowExists: true, balanceMicros: 10_000_000, autopayEnabled: true, autopayThreshold: 5_000_000}
		svc := billing.NewService(repo) // autopay signalling not wired
		require.NoError(t, debit(svc))
	})
}

func TestHasOverrideFromContext_Default(t *testing.T) {
	assert.False(t, billing.HasOverrideFromContext(context.Background()))
}

func TestHasOverrideFromContext_True(t *testing.T) {
	ctx := context.WithValue(context.Background(), billing.HasOverrideContextKey, true)
	assert.True(t, billing.HasOverrideFromContext(ctx))
}
