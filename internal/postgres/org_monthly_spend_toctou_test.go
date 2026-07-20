package postgres_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workweave/router/internal/billing"
	"workweave/router/internal/postgres"
	"workweave/router/internal/router/catalog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// TestOrgMonthlySpend_ConcurrentCheckThenDebit_BoundedOvershoot is the
// permanent #793 regression: N concurrent CheckOrgMonthlySpend + DebitForInference
// must not multiply past the hard monthly cap unboundedly.
//
// Fixture matches the issue reproduction: $1 limit, $0.90 starting spend,
// $6.75 notional per debit (1M in @ $3/MTok + 250K out @ $15/MTok), N=20.
// Soft-overshoot bound after the reserve-then-settle fix is roughly one
// under-reserved turn (or zero when remaining headroom < R); before the fix
// this assertion fails with ~$100+ overshoot.
//
// Gated on ROUTER_TEST_DATABASE_URL (falls back to DATABASE_URL). Skips when
// neither is set so unit CI without Postgres stays green. Requires a migrated
// router schema (docker compose postgres on :5433 is the local fixture).
func TestOrgMonthlySpend_ConcurrentCheckThenDebit_BoundedOvershoot(t *testing.T) {
	dsn := os.Getenv("ROUTER_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("ROUTER_TEST_DATABASE_URL / DATABASE_URL not set; need live Postgres for #793 concurrency regression")
	}

	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET search_path TO router, public")
		return err
	}
	cfg.MaxConns = 20
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	require.NoError(t, pool.Ping(ctx))

	orgID := fmt.Sprintf("toctou-793-%d", time.Now().UnixNano())
	const (
		limitMicros   int64 = 1_000_000 // $1.00
		startSpent    int64 = 900_000   // $0.90
		inputTokens         = 1_000_000
		outputTokens        = 250_000
		concurrency         = 20
	)
	pricing := catalog.Pricing{InputUSDPer1M: 3, OutputUSDPer1M: 15}
	perCallMicros := catalog.USDToMicros(
		catalog.EffectiveInputCost(inputTokens, 0, 0, pricing.InputUSDPer1M, pricing, "") +
			catalog.EffectiveOutputCost(outputTokens, pricing.OutputUSDPer1M),
	)
	require.Equal(t, int64(6_750_000), perCallMicros, "fixture must stay $6.75 for continuity with #793")

	_, err = pool.Exec(ctx, `
		INSERT INTO organization_spend_limits (organization_id, org_monthly_limit_usd_micros)
		VALUES ($1, $2)`, orgID, limitMicros)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO organization_monthly_spend (organization_id, month, spent_usd_micros)
		VALUES ($1, DATE_TRUNC('month', NOW() AT TIME ZONE 'utc')::date, $2)`, orgID, startSpent)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO organization_credit_balance (organization_id, balance_usd_micros)
		VALUES ($1, $2)`, orgID, int64(1_000_000_000)) // $1000 — not under test
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM organization_credit_ledger WHERE organization_id = $1`, orgID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM organization_credit_balance WHERE organization_id = $1`, orgID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM organization_monthly_spend WHERE organization_id = $1`, orgID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM organization_spend_limits WHERE organization_id = $1`, orgID)
	})

	svc := billing.NewService(postgres.NewBillingRepo(pool))

	var (
		wg       sync.WaitGroup
		debited  atomic.Int32
		rejected atomic.Int32
	)
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		i := i
		go func() {
			defer wg.Done()
			reqCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			res, err := svc.CheckOrgMonthlySpend(reqCtx, orgID)
			if err != nil {
				t.Errorf("CheckOrgMonthlySpend: %v", err)
				return
			}
			if res.LimitReached() {
				rejected.Add(1)
				return
			}
			_, err = svc.DebitForInference(reqCtx, billing.DebitInferenceParams{
				OrganizationID:  orgID,
				RouterRequestID: fmt.Sprintf("%s-%d", orgID, i),
				Model:           "toctou-fixture",
				InputTokens:     inputTokens,
				OutputTokens:    outputTokens,
				Pricing:         pricing,
			})
			if err != nil {
				t.Errorf("DebitForInference: %v", err)
				return
			}
			debited.Add(1)
		}()
	}
	wg.Wait()

	final, _, err := postgres.NewBillingRepo(pool).GetOrgMonthlySpendAndLimit(ctx, orgID)
	require.NoError(t, err)

	// Soft overshoot bound: at most one in-flight turn past the limit (the
	// product promise before concurrency multiplied it). Equivalent to
	// start + N*cost when every check races — that path must fail this assert.
	maxAllowed := limitMicros + perCallMicros
	t.Logf("org=%s debited=%d rejected=%d final_spent_usd_micros=%d overshoot_usd_micros=%d max_allowed=%d",
		orgID, debited.Load(), rejected.Load(), final, final-limitMicros, maxAllowed)

	require.LessOrEqual(t, final, maxAllowed,
		"org monthly spend TOCTOU (#793): final=%d ($%.2f) exceeds limit+one_turn=%d ($%.2f); overshoot=$%.2f from %d concurrent debits",
		final, float64(final)/1e6, maxAllowed, float64(maxAllowed)/1e6, float64(final-limitMicros)/1e6, debited.Load())
}
