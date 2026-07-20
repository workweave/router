package postgres_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"workweave/router/internal/billing"
	"workweave/router/internal/postgres"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// TestSweepExpiredSpendReservations_DecrementsReserved seeds an already-
// expired org-month reservation, runs one sweep cycle, and asserts the row
// is gone and denormalized reserved_usd_micros is decremented.
func TestSweepExpiredSpendReservations_DecrementsReserved(t *testing.T) {
	dsn := os.Getenv("ROUTER_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("ROUTER_TEST_DATABASE_URL / DATABASE_URL not set")
	}

	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET search_path TO router, public")
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	orgID := fmt.Sprintf("sweep-793-%d", time.Now().UnixNano())
	const amount int64 = 1_000_000
	resID := uuid.New()

	_, err = pool.Exec(ctx, `
		INSERT INTO organization_spend_limits (organization_id, org_monthly_limit_usd_micros)
		VALUES ($1, $2)`, orgID, int64(10_000_000))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO organization_monthly_spend (organization_id, month, spent_usd_micros, reserved_usd_micros)
		VALUES ($1, DATE_TRUNC('month', NOW() AT TIME ZONE 'utc')::date, 0, $2)`, orgID, amount)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO spend_reservations (id, scope_kind, scope_id, month, amount_usd_micros, expires_at)
		VALUES ($1, 'org_month', $2, DATE_TRUNC('month', NOW() AT TIME ZONE 'utc')::date, $3, NOW() - INTERVAL '1 minute')`,
		resID, orgID, amount)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM spend_reservations WHERE scope_id = $1`, orgID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM organization_monthly_spend WHERE organization_id = $1`, orgID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM organization_spend_limits WHERE organization_id = $1`, orgID)
	})

	svc := billing.NewService(postgres.NewBillingRepo(pool))
	released, err := svc.SweepExpiredReservations(ctx, time.Now().UTC())
	require.NoError(t, err)
	require.GreaterOrEqual(t, released, 1, "sweeper must consume at least the seeded expired row")

	var stillThere int
	err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM spend_reservations WHERE id = $1`, resID).Scan(&stillThere)
	require.NoError(t, err)
	require.Equal(t, 0, stillThere, "expired reservation row must be deleted")

	spent, reserved, _, err := postgres.NewBillingRepo(pool).GetOrgMonthlySpendAndLimit(ctx, orgID)
	require.NoError(t, err)
	require.Equal(t, int64(0), spent)
	require.Equal(t, int64(0), reserved, "denormalized reserved_usd_micros must drop by the swept amount")
}
