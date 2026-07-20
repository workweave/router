// Command spend_reservation_check exercises spend reservations against the
// docker-compose Postgres fixture without making database access part of
// `go test ./...`.
//
// It is gated on ROUTER_TEST_DATABASE_URL and is a no-op without it.
//
// Usage (from the repo root):
//
//	ROUTER_TEST_DATABASE_URL="postgres://router:router@localhost:5432/router?search_path=router" \
//	    go run ./scripts/spend_reservation_check
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"workweave/router/internal/billing"
	"workweave/router/internal/postgres"
	"workweave/router/internal/router/catalog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	reservationLimitMicros  int64 = 1_000_000
	reservationAmountMicros int64 = 1_000_000
)

func main() {
	dsn := os.Getenv("ROUTER_TEST_DATABASE_URL")
	if dsn == "" {
		slog.Info("ROUTER_TEST_DATABASE_URL not set; skipping live-DB spend reservation check")
		return
	}

	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		fail("parse database URL", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET search_path TO router, public")
		return err
	}
	cfg.MaxConns = 20
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		fail("connect to database", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		fail("ping database", err)
	}

	checks := []struct {
		name string
		run  func(context.Context, *pgxpool.Pool) error
	}{
		{"concurrent arm and settle", checkConcurrentArmAndSettle},
		{"Go-computed month consistency", checkGoComputedMonth},
		{"expired reservation sweep", checkExpiredSweep},
	}
	for _, check := range checks {
		if err := check.run(ctx, pool); err != nil {
			fail(check.name, err)
		}
		slog.Info("spend reservation check passed", "check", check.name)
	}
}

func fail(step string, err error) {
	slog.Error("spend reservation check failed", "step", step, "err", err)
	os.Exit(1)
}

func checkConcurrentArmAndSettle(ctx context.Context, pool *pgxpool.Pool) error {
	orgID := fmt.Sprintf("spend-check-toctou-%d", time.Now().UnixNano())
	defer cleanupOrg(ctx, pool, orgID)

	const (
		startSpent   int64 = 900_000
		inputTokens        = 1_000_000
		outputTokens       = 250_000
		concurrency        = 20
	)
	pricing := catalog.Pricing{InputUSDPer1M: 3, OutputUSDPer1M: 15}
	perCallMicros := catalog.USDToMicros(
		catalog.EffectiveInputCost(inputTokens, 0, 0, pricing.InputUSDPer1M, pricing, "") +
			catalog.EffectiveOutputCost(outputTokens, pricing.OutputUSDPer1M),
	)
	if perCallMicros != 6_750_000 {
		return fmt.Errorf("fixture cost changed: got %d, want 6750000", perCallMicros)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO organization_spend_limits (organization_id, org_monthly_limit_usd_micros)
		VALUES ($1, $2)`, orgID, reservationLimitMicros); err != nil {
		return fmt.Errorf("insert spend limit: %w", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO organization_monthly_spend (organization_id, month, spent_usd_micros)
		VALUES ($1, DATE_TRUNC('month', NOW() AT TIME ZONE 'utc')::date, $2)`,
		orgID, startSpent); err != nil {
		return fmt.Errorf("insert monthly spend: %w", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO organization_credit_balance (organization_id, balance_usd_micros)
		VALUES ($1, $2)`, orgID, int64(1_000_000_000)); err != nil {
		return fmt.Errorf("insert credit balance: %w", err)
	}

	svc := billing.NewService(postgres.NewBillingRepo(pool)).
		WithReservationConfig(billing.DefaultReserveAmountMicros, billing.DefaultReserveTTL)
	var wg sync.WaitGroup
	var debited, rejected atomic.Int32
	errs := make(chan error, concurrency)
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		i := i
		go func() {
			defer wg.Done()
			reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			reqID := fmt.Sprintf("%s-%d", orgID, i)
			reqCtx, release, err := svc.ArmSpendReservations(reqCtx, orgID, "", "", reqID)
			if err != nil {
				rejected.Add(1)
				return
			}
			defer release()
			_, err = svc.DebitForInference(reqCtx, billing.DebitInferenceParams{
				OrganizationID:  orgID,
				RouterRequestID: reqID,
				Model:           "spend-reservation-fixture",
				InputTokens:     inputTokens,
				OutputTokens:    outputTokens,
				Pricing:         pricing,
				ReservationIDs:  billing.SpendHoldFrom(reqCtx).IDs,
			})
			if err != nil {
				errs <- fmt.Errorf("debit: %w", err)
				return
			}
			billing.SpendHoldFrom(reqCtx).MarkSettled()
			debited.Add(1)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		return err
	}

	spent, reserved, _, err := postgres.NewBillingRepo(pool).GetOrgMonthlySpendAndLimit(ctx, orgID)
	if err != nil {
		return fmt.Errorf("read final monthly spend: %w", err)
	}
	slog.Info("concurrent reservation fixture complete", "organization_id", orgID,
		"debited", debited.Load(), "rejected", rejected.Load(), "spent_usd_micros", spent,
		"reserved_usd_micros", reserved)
	if spent > reservationLimitMicros+perCallMicros {
		return fmt.Errorf("monthly spend overshot cap by more than one turn: spent=%d max=%d",
			spent, reservationLimitMicros+perCallMicros)
	}
	if reserved != 0 {
		return fmt.Errorf("reservations remain after settle: %d", reserved)
	}
	return nil
}

func checkGoComputedMonth(ctx context.Context, pool *pgxpool.Pool) error {
	orgID := fmt.Sprintf("spend-check-month-%d", time.Now().UnixNano())
	defer cleanupOrg(ctx, pool, orgID)
	now := time.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		INSERT INTO organization_spend_limits (organization_id, org_monthly_limit_usd_micros)
		VALUES ($1, $2)`, orgID, int64(10_000_000)); err != nil {
		return fmt.Errorf("insert spend limit: %w", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO organization_monthly_spend (organization_id, month, spent_usd_micros, reserved_usd_micros)
		VALUES ($1, $2, 0, 0)`, orgID, month); err != nil {
		return fmt.Errorf("insert monthly spend: %w", err)
	}
	ids, err := postgres.NewBillingRepo(pool).ReserveSpendCaps(ctx, billing.ReserveSpendCapsParams{
		OrganizationID: orgID, RouterRequestID: orgID, AmountUsdMicros: reservationAmountMicros,
		TTL: 15 * time.Minute, SkipKey: true, SkipUser: true,
	})
	if err != nil {
		return fmt.Errorf("reserve spend caps: %w", err)
	}
	if len(ids) != 1 {
		return fmt.Errorf("want one reservation, got %d", len(ids))
	}
	var reservationMonth time.Time
	if err := pool.QueryRow(ctx, `SELECT month FROM spend_reservations WHERE id = $1`, ids[0]).Scan(&reservationMonth); err != nil {
		return fmt.Errorf("read reservation month: %w", err)
	}
	reservationMonth = reservationMonth.UTC()
	reservationMonth = time.Date(reservationMonth.Year(), reservationMonth.Month(), 1, 0, 0, 0, 0, time.UTC)
	var reserved int64
	if err := pool.QueryRow(ctx, `
		SELECT reserved_usd_micros FROM organization_monthly_spend
		WHERE organization_id = $1 AND month = $2`, orgID, month).Scan(&reserved); err != nil {
		return fmt.Errorf("read reserved month amount: %w", err)
	}
	if !reservationMonth.Equal(month) {
		return fmt.Errorf("reservation month=%s, Go-computed month=%s", reservationMonth, month)
	}
	if reserved != reservationAmountMicros {
		return fmt.Errorf("reserved amount=%d, want %d", reserved, reservationAmountMicros)
	}
	return nil
}

func checkExpiredSweep(ctx context.Context, pool *pgxpool.Pool) error {
	orgID := fmt.Sprintf("spend-check-sweep-%d", time.Now().UnixNano())
	defer cleanupOrg(ctx, pool, orgID)
	const amount int64 = 1_000_000
	reservationID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO organization_spend_limits (organization_id, org_monthly_limit_usd_micros)
		VALUES ($1, $2)`, orgID, int64(10_000_000)); err != nil {
		return fmt.Errorf("insert spend limit: %w", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO organization_monthly_spend (organization_id, month, spent_usd_micros, reserved_usd_micros)
		VALUES ($1, DATE_TRUNC('month', NOW() AT TIME ZONE 'utc')::date, 0, $2)`,
		orgID, amount); err != nil {
		return fmt.Errorf("insert monthly spend: %w", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO spend_reservations (id, scope_kind, scope_id, month, amount_usd_micros, expires_at)
		VALUES ($1, 'org_month', $2, DATE_TRUNC('month', NOW() AT TIME ZONE 'utc')::date, $3, NOW() - INTERVAL '1 minute')`,
		reservationID, orgID, amount); err != nil {
		return fmt.Errorf("insert expired reservation: %w", err)
	}
	released, err := billing.NewService(postgres.NewBillingRepo(pool)).SweepExpiredReservations(ctx, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("sweep expired reservations: %w", err)
	}
	if released < 1 {
		return fmt.Errorf("sweeper released %d reservations", released)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM spend_reservations WHERE id = $1`, reservationID).Scan(&count); err != nil {
		return fmt.Errorf("check swept reservation: %w", err)
	}
	if count != 0 {
		return errors.New("expired reservation row remains")
	}
	var reserved int64
	if err := pool.QueryRow(ctx, `
		SELECT reserved_usd_micros FROM organization_monthly_spend
		WHERE organization_id = $1 AND month = DATE_TRUNC('month', NOW() AT TIME ZONE 'utc')::date`,
		orgID).Scan(&reserved); err != nil {
		return fmt.Errorf("read swept monthly spend: %w", err)
	}
	if reserved != 0 {
		return fmt.Errorf("reserved amount after sweep=%d", reserved)
	}
	return nil
}

func cleanupOrg(ctx context.Context, pool *pgxpool.Pool, orgID string) {
	cleanupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, _ = pool.Exec(cleanupCtx, `DELETE FROM spend_reservations WHERE scope_id = $1`, orgID)
	_, _ = pool.Exec(cleanupCtx, `DELETE FROM organization_credit_ledger WHERE organization_id = $1`, orgID)
	_, _ = pool.Exec(cleanupCtx, `DELETE FROM organization_credit_balance WHERE organization_id = $1`, orgID)
	_, _ = pool.Exec(cleanupCtx, `DELETE FROM organization_monthly_spend WHERE organization_id = $1`, orgID)
	_, _ = pool.Exec(cleanupCtx, `DELETE FROM organization_spend_limits WHERE organization_id = $1`, orgID)
}
