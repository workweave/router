package postgres

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"workweave/router/internal/billing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReserveSpendCaps_SingleGoComputedMonthAcrossEnsureBumpInsert freezes the
// Go reservation clock on a UTC month that differs from Postgres NOW()'s
// month. The PG-NOW month row is already at its cap (no headroom); the
// Go-computed month row has headroom. After the fix, Ensure/Bump/Insert all
// target the Go month, so the reserve succeeds and the reservation's recorded
// month matches the bumped spend row. Pre-fix (SQL NOW() in Ensure/Bump),
// the bump would hit the capped PG-NOW month and either refuse or strand
// reserved_usd_micros on a month the reservation record does not name.
func TestReserveSpendCaps_SingleGoComputedMonthAcrossEnsureBumpInsert(t *testing.T) {
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
	require.NoError(t, pool.Ping(ctx))

	var pgMonth time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT DATE_TRUNC('month', NOW() AT TIME ZONE 'utc')::date`).Scan(&pgMonth))
	pgMonth = time.Date(pgMonth.Year(), pgMonth.Month(), 1, 0, 0, 0, 0, time.UTC)
	goMonth := pgMonth.AddDate(0, -1, 0)
	require.NotEqual(t, pgMonth, goMonth)

	prev := utcNow
	utcNow = func() time.Time { return goMonth.Add(12 * time.Hour) }
	t.Cleanup(func() { utcNow = prev })

	orgID := fmt.Sprintf("month-skew-793-%d", time.Now().UnixNano())
	const (
		limitMicros int64 = 10_000_000 // $10
		reserveR    int64 = 1_000_000  // $1
	)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM spend_reservations WHERE scope_id = $1`, orgID)
		_, _ = pool.Exec(ctx, `DELETE FROM organization_monthly_spend WHERE organization_id = $1`, orgID)
		_, _ = pool.Exec(ctx, `DELETE FROM organization_spend_limits WHERE organization_id = $1`, orgID)
	})

	_, err = pool.Exec(ctx, `
		INSERT INTO organization_spend_limits (organization_id, org_monthly_limit_usd_micros)
		VALUES ($1, $2)`, orgID, limitMicros)
	require.NoError(t, err)
	// PG-NOW month: fully spent — an independent SQL NOW() bump would fail here.
	_, err = pool.Exec(ctx, `
		INSERT INTO organization_monthly_spend (organization_id, month, spent_usd_micros, reserved_usd_micros)
		VALUES ($1, $2, $3, 0)`, orgID, pgMonth, limitMicros)
	require.NoError(t, err)
	// Go-computed month: empty headroom for R.
	_, err = pool.Exec(ctx, `
		INSERT INTO organization_monthly_spend (organization_id, month, spent_usd_micros, reserved_usd_micros)
		VALUES ($1, $2, 0, 0)`, orgID, goMonth)
	require.NoError(t, err)

	repo := NewBillingRepo(pool)
	ids, err := repo.ReserveSpendCaps(ctx, billing.ReserveSpendCapsParams{
		OrganizationID:  orgID,
		RouterRequestID: "month-skew-req",
		AmountUsdMicros: reserveR,
		TTL:             15 * time.Minute,
		SkipKey:         true,
		SkipUser:        true,
	})
	require.NoError(t, err)
	require.Len(t, ids, 1)

	var (
		resMonth   time.Time
		resAmount  int64
		goReserved int64
		pgReserved int64
	)
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT month, amount_usd_micros FROM spend_reservations WHERE id = $1`, ids[0]).
		Scan(&resMonth, &resAmount))
	resMonth = time.Date(resMonth.Year(), resMonth.Month(), 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT reserved_usd_micros FROM organization_monthly_spend
		WHERE organization_id = $1 AND month = $2`, orgID, goMonth).Scan(&goReserved))
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT reserved_usd_micros FROM organization_monthly_spend
		WHERE organization_id = $1 AND month = $2`, orgID, pgMonth).Scan(&pgReserved))

	assert.Equal(t, goMonth, resMonth, "reservation must record the Go-computed month")
	assert.Equal(t, reserveR, resAmount)
	assert.Equal(t, reserveR, goReserved, "bump must land on the same Go-computed month as the reservation")
	assert.Equal(t, int64(0), pgReserved, "PG-NOW month must stay untouched (would be bumped pre-fix)")
	assert.Equal(t, goMonth, utcMonthDate().Time.UTC(), "utcMonthDate must match the frozen Go clock")
}
