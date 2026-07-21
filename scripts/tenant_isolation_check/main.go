// Command tenant_isolation_check exercises the tenant-ownership invariants
// against a live Postgres database using the real repository implementations.
//
// It is a separate main package so go test ./... never touches Postgres. Set
// ROUTER_TEST_DATABASE_URL (or DATABASE_URL) to run the checks.
package main

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"time"

	"workweave/router/internal/billing"
	"workweave/router/internal/postgres"
	"workweave/router/internal/router/sessionpin"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	dsn := os.Getenv("ROUTER_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		fmt.Println("ROUTER_TEST_DATABASE_URL / DATABASE_URL not set; skipping live-DB tenant-isolation checks")
		return
	}

	ctx := context.Background()
	pool, err := openPool(ctx, dsn)
	if err != nil {
		fail("connect to database", err)
	}
	defer pool.Close()

	checks := []struct {
		name string
		run  func(context.Context, *pgxpool.Pool) error
	}{
		{"session pin ownership", checkSessionPinOwnership},
		{"key spend ownership", checkKeySpendOwnership},
		{"user month spend ownership", checkUserMonthSpendOwnership},
		{"monthly spend read ownership", checkMonthlySpendReadOwnership},
	}
	for _, check := range checks {
		if err := check.run(ctx, pool); err != nil {
			fail(check.name, err)
		}
		fmt.Printf("ok: %s\n", check.name)
	}
}

func openPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET search_path TO router, public")
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

func fail(step string, err error) {
	fmt.Fprintf(os.Stderr, "FAIL: %s: %v\n", step, err)
	os.Exit(1)
}

func requireEqual(name string, want, got any) error {
	if !reflect.DeepEqual(want, got) {
		return fmt.Errorf("%s: want %v, got %v", name, want, got)
	}
	return nil
}

func checkSessionPinOwnership(ctx context.Context, pool *pgxpool.Pool) error {
	suffix := time.Now().UnixNano()
	orgA := fmt.Sprintf("796-pin-a-%d", suffix%1_000_000_000_000)
	orgB := fmt.Sprintf("796-pin-b-%d", suffix%1_000_000_000_000)
	instA, instB := uuid.New(), uuid.New()
	var sessionKey [sessionpin.SessionKeyLen]byte
	copy(sessionKey[:], []byte(fmt.Sprintf("796pin%08d", suffix%100_000_000)))

	_, err := pool.Exec(ctx, `
		INSERT INTO model_router_installations (id, external_id, name)
		VALUES ($1, $2, '796-pin-A'), ($3, $4, '796-pin-B')`,
		instA, orgA, instB, orgB)
	if err != nil {
		return fmt.Errorf("insert installations: %w", err)
	}
	defer cleanup(ctx, pool,
		cleanupStep{`DELETE FROM session_pins WHERE session_key = $1`, []any{sessionKey[:]}},
		cleanupStep{`DELETE FROM model_router_installations WHERE id IN ($1, $2)`, []any{instA, instB}})

	store := postgres.NewSessionPinRepo(pool)
	if err := store.Upsert(ctx, sessionpin.Pin{
		SessionKey: sessionKey, Role: sessionpin.DefaultRole, InstallationID: instA,
		Provider: "anthropic", Model: "model-from-A", Reason: "796-test-A",
		TurnCount: 1, PinnedUntil: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		return fmt.Errorf("upsert owner row: %w", err)
	}
	if err := store.Upsert(ctx, sessionpin.Pin{
		SessionKey: sessionKey, Role: sessionpin.DefaultRole, InstallationID: instB,
		Provider: "openai", Model: "model-from-B", Reason: "796-test-B",
		TurnCount: 1, PinnedUntil: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		return fmt.Errorf("upsert mismatched row: %w", err)
	}

	var gotInstall uuid.UUID
	var gotModel, gotProvider string
	var turnCount int
	err = pool.QueryRow(ctx, `
		SELECT installation_id, pinned_model, pinned_provider, turn_count
		FROM session_pins
		WHERE session_key = $1 AND role = $2`, sessionKey[:], sessionpin.DefaultRole).
		Scan(&gotInstall, &gotModel, &gotProvider, &turnCount)
	if err != nil {
		return fmt.Errorf("read owner row: %w", err)
	}
	for _, check := range []struct {
		name string
		want any
		got  any
	}{
		{"installation_id", instA, gotInstall},
		{"pinned_model", "model-from-A", gotModel},
		{"pinned_provider", "anthropic", gotProvider},
		{"turn_count", 1, turnCount},
	} {
		if err := requireEqual(check.name, check.want, check.got); err != nil {
			return err
		}
	}
	return nil
}

func checkKeySpendOwnership(ctx context.Context, pool *pgxpool.Pool) error {
	suffix := time.Now().UnixNano()
	orgA := fmt.Sprintf("796-key-a-%d", suffix%1_000_000_000_000)
	orgB := fmt.Sprintf("796-key-b-%d", suffix%1_000_000_000_000)
	instA, instB := uuid.New(), uuid.New()
	keyA, keyB := uuid.New(), uuid.New()
	const startBalance int64 = 1_000_000_000
	const debitMicros int64 = 500_000

	_, err := pool.Exec(ctx, `
		INSERT INTO model_router_installations (id, external_id, name)
		VALUES ($1, $2, '796-key-A'), ($3, $4, '796-key-B')`,
		instA, orgA, instB, orgB)
	if err != nil {
		return fmt.Errorf("insert installations: %w", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO model_router_api_keys
			(id, installation_id, external_id, key_prefix, key_hash, key_suffix, spent_usd_micros)
		VALUES
			($1, $2, $3, 'rk_a796_', $4, 'aaaa', 0),
			($5, $6, $7, 'rk_b796_', $8, 'bbbb', 0)`,
		keyA, instA, "kid-a-"+orgA, fmt.Sprintf("hash-a-%d", suffix),
		keyB, instB, "kid-b-"+orgB, fmt.Sprintf("hash-b-%d", suffix))
	if err != nil {
		return fmt.Errorf("insert API keys: %w", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO organization_credit_balance (organization_id, balance_usd_micros)
		VALUES ($1, $2), ($3, $2)`, orgA, startBalance, orgB)
	if err != nil {
		return fmt.Errorf("insert balances: %w", err)
	}
	defer cleanup(ctx, pool,
		cleanupStep{`DELETE FROM organization_credit_ledger WHERE organization_id IN ($1, $2)`, []any{orgA, orgB}},
		cleanupStep{`DELETE FROM organization_monthly_spend WHERE organization_id IN ($1, $2)`, []any{orgA, orgB}},
		cleanupStep{`DELETE FROM organization_credit_balance WHERE organization_id IN ($1, $2)`, []any{orgA, orgB}},
		cleanupStep{`DELETE FROM model_router_api_keys WHERE id IN ($1, $2)`, []any{keyA, keyB}},
		cleanupStep{`DELETE FROM model_router_installations WHERE id IN ($1, $2)`, []any{instA, instB}})

	repo := postgres.NewBillingRepo(pool)
	_, err = repo.DebitInference(ctx, billing.DebitParams{
		OrganizationID: orgA, DeltaUsdMicros: -debitMicros,
		NotionalCostMicros: debitMicros, EntryType: billing.EntryTypeInference,
		RouterRequestID: fmt.Sprintf("796-key-%d", suffix), RouterModel: "796-fixture",
		APIKeyID: keyB.String(),
	})
	if err != nil {
		return fmt.Errorf("debit credits: %w", err)
	}

	var spentB int64
	if err := pool.QueryRow(ctx,
		`SELECT spent_usd_micros FROM model_router_api_keys WHERE id = $1`, keyB).Scan(&spentB); err != nil {
		return fmt.Errorf("read foreign key spend: %w", err)
	}
	if err := requireEqual("foreign key spent_usd_micros", int64(0), spentB); err != nil {
		return err
	}
	balance, err := repo.GetBalance(ctx, orgA)
	if err != nil {
		return fmt.Errorf("read org A balance: %w", err)
	}
	return requireEqual("org A balance", startBalance-debitMicros, balance)
}

func checkUserMonthSpendOwnership(ctx context.Context, pool *pgxpool.Pool) error {
	suffix := time.Now().UnixNano()
	orgA := fmt.Sprintf("796-usr-a-%d", suffix%1_000_000_000_000)
	orgB := fmt.Sprintf("796-usr-b-%d", suffix%1_000_000_000_000)
	instA, instB := uuid.New(), uuid.New()
	userA, userB := uuid.New(), uuid.New()
	const startBalance int64 = 1_000_000_000
	const debitMicros int64 = 424_242

	_, err := pool.Exec(ctx, `
		INSERT INTO model_router_installations (id, external_id, name)
		VALUES ($1, $2, '796-usr-A'), ($3, $4, '796-usr-B')`,
		instA, orgA, instB, orgB)
	if err != nil {
		return fmt.Errorf("insert installations: %w", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO model_router_users (id, installation_id, email)
		VALUES ($1, $2, $3), ($4, $5, $6)`,
		userA, instA, fmt.Sprintf("a-%d@example.com", suffix),
		userB, instB, fmt.Sprintf("b-%d@example.com", suffix))
	if err != nil {
		return fmt.Errorf("insert users: %w", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO organization_credit_balance (organization_id, balance_usd_micros)
		VALUES ($1, $2), ($3, $2)`, orgA, startBalance, orgB)
	if err != nil {
		return fmt.Errorf("insert balances: %w", err)
	}
	defer cleanup(ctx, pool,
		cleanupStep{`DELETE FROM model_router_user_monthly_spend WHERE router_user_id IN ($1, $2)`, []any{userA, userB}},
		cleanupStep{`DELETE FROM organization_credit_ledger WHERE organization_id IN ($1, $2)`, []any{orgA, orgB}},
		cleanupStep{`DELETE FROM organization_monthly_spend WHERE organization_id IN ($1, $2)`, []any{orgA, orgB}},
		cleanupStep{`DELETE FROM organization_credit_balance WHERE organization_id IN ($1, $2)`, []any{orgA, orgB}},
		cleanupStep{`DELETE FROM model_router_users WHERE id IN ($1, $2)`, []any{userA, userB}},
		cleanupStep{`DELETE FROM model_router_installations WHERE id IN ($1, $2)`, []any{instA, instB}})

	_, err = postgres.NewBillingRepo(pool).DebitInference(ctx, billing.DebitParams{
		OrganizationID: orgA, DeltaUsdMicros: -debitMicros,
		NotionalCostMicros: debitMicros, EntryType: billing.EntryTypeInference,
		RouterRequestID: fmt.Sprintf("796-usr-%d", suffix), RouterModel: "796-fixture",
		RouterUserID: userB.String(),
	})
	if err != nil {
		return fmt.Errorf("debit credits: %w", err)
	}
	var countB int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM model_router_user_monthly_spend
		WHERE router_user_id = $1
		  AND month = DATE_TRUNC('month', NOW() AT TIME ZONE 'utc')::date`, userB).Scan(&countB); err != nil {
		return fmt.Errorf("read foreign user spend: %w", err)
	}
	return requireEqual("foreign user monthly spend rows", 0, countB)
}

func checkMonthlySpendReadOwnership(ctx context.Context, pool *pgxpool.Pool) error {
	suffix := time.Now().UnixNano()
	orgA := fmt.Sprintf("796-rd-a-%d", suffix%1_000_000_000_000)
	orgB := fmt.Sprintf("796-rd-b-%d", suffix%1_000_000_000_000)
	instA, instB := uuid.New(), uuid.New()
	userB := uuid.New()
	const orgADefault int64 = 1_111_111
	const userBSpent int64 = 424_242

	_, err := pool.Exec(ctx, `
		INSERT INTO model_router_installations (id, external_id, name)
		VALUES ($1, $2, '796-rd-A'), ($3, $4, '796-rd-B')`,
		instA, orgA, instB, orgB)
	if err != nil {
		return fmt.Errorf("insert installations: %w", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO model_router_users (id, installation_id, email)
		VALUES ($1, $2, $3)`, userB, instB, fmt.Sprintf("b-rd-%d@example.com", suffix))
	if err != nil {
		return fmt.Errorf("insert user: %w", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO organization_spend_limits (organization_id, user_monthly_limit_usd_micros)
		VALUES ($1, $2)`, orgA, orgADefault)
	if err != nil {
		return fmt.Errorf("insert spend limit: %w", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO model_router_user_monthly_spend (router_user_id, month, spent_usd_micros)
		VALUES ($1, DATE_TRUNC('month', NOW() AT TIME ZONE 'utc')::date, $2)`, userB, userBSpent)
	if err != nil {
		return fmt.Errorf("insert user spend: %w", err)
	}
	defer cleanup(ctx, pool,
		cleanupStep{`DELETE FROM model_router_user_monthly_spend WHERE router_user_id = $1`, []any{userB}},
		cleanupStep{`DELETE FROM organization_spend_limits WHERE organization_id = $1`, []any{orgA}},
		cleanupStep{`DELETE FROM model_router_users WHERE id = $1`, []any{userB}},
		cleanupStep{`DELETE FROM model_router_installations WHERE id IN ($1, $2)`, []any{instA, instB}})

	spent, limit, err := postgres.NewBillingRepo(pool).GetUserMonthlySpendAndLimit(ctx, orgA, userB.String())
	if err != nil {
		return fmt.Errorf("read monthly spend and limit: %w", err)
	}
	if err := requireEqual("foreign user monthly spend", int64(0), spent); err != nil {
		return err
	}
	if limit == nil {
		return fmt.Errorf("org default limit: want %d, got nil", orgADefault)
	}
	return requireEqual("org default limit", orgADefault, *limit)
}

type cleanupStep struct {
	query string
	args  []any
}

func cleanup(ctx context.Context, pool *pgxpool.Pool, statements ...cleanupStep) {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, statement := range statements {
		_, _ = pool.Exec(c, statement.query, statement.args...)
	}
}
