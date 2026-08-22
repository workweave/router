package postgres_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"workweave/router/internal/billing"
	"workweave/router/internal/postgres"
	"workweave/router/internal/router/sessionpin"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// #796 permanent regressions: SQL must enforce tenant ownership rather than
// relying solely on the app layer always passing same-tenant IDs.
//
// Gated on ROUTER_TEST_DATABASE_URL (falls back to DATABASE_URL). Uses real
// Postgres + real SessionPinRepo / BillingRepo — no mocks.

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("ROUTER_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("ROUTER_TEST_DATABASE_URL / DATABASE_URL not set; need live Postgres for #796 tenant-isolation regressions")
	}
	return dsn
}

func openRouterPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
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
	return pool
}

// TestSessionPin_UpsertDoesNotCrossInstallation reproduces #796 finding 1:
// UpsertSessionPin's ON CONFLICT must not overwrite another installation's
// pin (model/provider) while leaving installation_id stuck on the original
// owner. Mismatch = silent no-op; owner row unchanged.
func TestSessionPin_UpsertDoesNotCrossInstallation(t *testing.T) {
	pool := openRouterPool(t, testDatabaseURL(t))
	ctx := context.Background()

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
	require.NoError(t, err)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM session_pins WHERE session_key = $1`, sessionKey[:])
		_, _ = pool.Exec(c, `DELETE FROM model_router_installations WHERE id IN ($1, $2)`, instA, instB)
	})

	store := postgres.NewSessionPinRepo(pool)
	require.NoError(t, store.Upsert(ctx, sessionpin.Pin{
		SessionKey:     sessionKey,
		Role:           sessionpin.DefaultRole,
		InstallationID: instA,
		Provider:       "anthropic",
		Model:          "model-from-A",
		Reason:         "796-test-A",
		TurnCount:      1,
		PinnedUntil:    time.Now().UTC().Add(time.Hour),
	}))

	// Cross-tenant collision attempt: same (session_key, role), different install.
	require.NoError(t, store.Upsert(ctx, sessionpin.Pin{
		SessionKey:     sessionKey,
		Role:           sessionpin.DefaultRole,
		InstallationID: instB,
		Provider:       "openai",
		Model:          "model-from-B",
		Reason:         "796-test-B",
		TurnCount:      1,
		PinnedUntil:    time.Now().UTC().Add(time.Hour),
	}))

	var (
		gotInstall uuid.UUID
		gotModel   string
		gotProv    string
		turnCount  int
	)
	err = pool.QueryRow(ctx, `
		SELECT installation_id, pinned_model, pinned_provider, turn_count
		FROM session_pins
		WHERE session_key = $1 AND role = $2`, sessionKey[:], sessionpin.DefaultRole).
		Scan(&gotInstall, &gotModel, &gotProv, &turnCount)
	require.NoError(t, err)

	require.Equal(t, instA, gotInstall, "installation_id must stay on original owner A")
	require.Equal(t, "model-from-A", gotModel,
		"#796: mismatched Upsert must not overwrite pinned_model (got %q from install B)", gotModel)
	require.Equal(t, "anthropic", gotProv,
		"#796: mismatched Upsert must not overwrite pinned_provider")
	require.Equal(t, 1, turnCount,
		"#796: mismatched Upsert must not bump turn_count on the owner row")
}

// TestDebitOrgCredits_KeySpendRequiresOrgOwnership reproduces #796 finding 2:
// key_spend must not bump a foreign key's spent_usd_micros when organization_id
// and api_key_id belong to different tenants.
func TestDebitOrgCredits_KeySpendRequiresOrgOwnership(t *testing.T) {
	pool := openRouterPool(t, testDatabaseURL(t))
	ctx := context.Background()

	suffix := time.Now().UnixNano()
	orgA := fmt.Sprintf("796-key-a-%d", suffix%1_000_000_000_000)
	orgB := fmt.Sprintf("796-key-b-%d", suffix%1_000_000_000_000)
	instA, instB := uuid.New(), uuid.New()
	keyA, keyB := uuid.New(), uuid.New()
	const (
		startBalance int64 = 1_000_000_000 // $1000
		debitMicros  int64 = 500_000       // $0.50
	)

	_, err := pool.Exec(ctx, `
		INSERT INTO model_router_installations (id, external_id, name)
		VALUES ($1, $2, '796-key-A'), ($3, $4, '796-key-B')`,
		instA, orgA, instB, orgB)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO model_router_api_keys
			(id, installation_id, external_id, key_prefix, key_hash, key_suffix, spent_usd_micros)
		VALUES
			($1, $2, $3, 'rk_a796_', $4, 'aaaa', 0),
			($5, $6, $7, 'rk_b796_', $8, 'bbbb', 0)`,
		keyA, instA, "kid-a-"+orgA, fmt.Sprintf("hash-a-%d", suffix),
		keyB, instB, "kid-b-"+orgB, fmt.Sprintf("hash-b-%d", suffix))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO organization_credit_balance (organization_id, balance_usd_micros)
		VALUES ($1, $2), ($3, $2)`, orgA, startBalance, orgB)
	require.NoError(t, err)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM organization_credit_ledger WHERE organization_id IN ($1, $2)`, orgA, orgB)
		_, _ = pool.Exec(c, `DELETE FROM organization_monthly_spend WHERE organization_id IN ($1, $2)`, orgA, orgB)
		_, _ = pool.Exec(c, `DELETE FROM organization_credit_balance WHERE organization_id IN ($1, $2)`, orgA, orgB)
		_, _ = pool.Exec(c, `DELETE FROM model_router_api_keys WHERE id IN ($1, $2)`, keyA, keyB)
		_, _ = pool.Exec(c, `DELETE FROM model_router_installations WHERE id IN ($1, $2)`, instA, instB)
	})

	repo := postgres.NewBillingRepo(pool)
	_, err = repo.DebitInference(ctx, billing.DebitParams{
		OrganizationID:     orgA,
		DeltaUsdMicros:     -debitMicros,
		NotionalCostMicros: debitMicros,
		EntryType:          billing.EntryTypeInference,
		RouterRequestID:    fmt.Sprintf("796-key-%d", suffix),
		RouterModel:        "796-fixture",
		APIKeyID:           keyB.String(), // foreign key — must NOT receive the spend bump
	})
	require.NoError(t, err)

	var spentB int64
	err = pool.QueryRow(ctx, `SELECT spent_usd_micros FROM model_router_api_keys WHERE id = $1`, keyB).Scan(&spentB)
	require.NoError(t, err)
	require.Equal(t, int64(0), spentB,
		"#796: debiting org A with org B's api_key_id must not bump B's spent_usd_micros (got %d)", spentB)

	var spentA int64
	err = pool.QueryRow(ctx, `SELECT spent_usd_micros FROM model_router_api_keys WHERE id = $1`, keyA).Scan(&spentA)
	require.NoError(t, err)
	require.Equal(t, int64(0), spentA, "org A's own key was not attributed; spend must stay 0")

	balA, err := repo.GetBalance(ctx, orgA)
	require.NoError(t, err)
	require.Equal(t, startBalance-debitMicros, balA, "org A balance must still debit")
}

// TestDebitOrgCredits_UserMonthSpendRequiresOrgOwnership reproduces the
// write-side twin of #796 finding 3: user_month_spend must not bump a
// foreign user's monthly counter when organization_id and router_user_id
// belong to different tenants.
func TestDebitOrgCredits_UserMonthSpendRequiresOrgOwnership(t *testing.T) {
	pool := openRouterPool(t, testDatabaseURL(t))
	ctx := context.Background()

	suffix := time.Now().UnixNano()
	orgA := fmt.Sprintf("796-usr-a-%d", suffix%1_000_000_000_000)
	orgB := fmt.Sprintf("796-usr-b-%d", suffix%1_000_000_000_000)
	instA, instB := uuid.New(), uuid.New()
	userA, userB := uuid.New(), uuid.New()
	const (
		startBalance int64 = 1_000_000_000
		debitMicros  int64 = 424_242
	)

	_, err := pool.Exec(ctx, `
		INSERT INTO model_router_installations (id, external_id, name)
		VALUES ($1, $2, '796-usr-A'), ($3, $4, '796-usr-B')`,
		instA, orgA, instB, orgB)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO model_router_users (id, installation_id, email)
		VALUES ($1, $2, $3), ($4, $5, $6)`,
		userA, instA, fmt.Sprintf("a-%d@example.com", suffix),
		userB, instB, fmt.Sprintf("b-%d@example.com", suffix))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO organization_credit_balance (organization_id, balance_usd_micros)
		VALUES ($1, $2), ($3, $2)`, orgA, startBalance, orgB)
	require.NoError(t, err)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM model_router_user_monthly_spend WHERE router_user_id IN ($1, $2)`, userA, userB)
		_, _ = pool.Exec(c, `DELETE FROM organization_credit_ledger WHERE organization_id IN ($1, $2)`, orgA, orgB)
		_, _ = pool.Exec(c, `DELETE FROM organization_monthly_spend WHERE organization_id IN ($1, $2)`, orgA, orgB)
		_, _ = pool.Exec(c, `DELETE FROM organization_credit_balance WHERE organization_id IN ($1, $2)`, orgA, orgB)
		_, _ = pool.Exec(c, `DELETE FROM model_router_users WHERE id IN ($1, $2)`, userA, userB)
		_, _ = pool.Exec(c, `DELETE FROM model_router_installations WHERE id IN ($1, $2)`, instA, instB)
	})

	repo := postgres.NewBillingRepo(pool)
	_, err = repo.DebitInference(ctx, billing.DebitParams{
		OrganizationID:     orgA,
		DeltaUsdMicros:     -debitMicros,
		NotionalCostMicros: debitMicros,
		EntryType:          billing.EntryTypeInference,
		RouterRequestID:    fmt.Sprintf("796-usr-%d", suffix),
		RouterModel:        "796-fixture",
		RouterUserID:       userB.String(), // foreign user — must NOT receive the month spend bump
	})
	require.NoError(t, err)

	var countB int
	err = pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM model_router_user_monthly_spend
		WHERE router_user_id = $1
		  AND month = DATE_TRUNC('month', NOW() AT TIME ZONE 'utc')::date`, userB).Scan(&countB)
	require.NoError(t, err)
	require.Equal(t, 0, countB,
		"#796: debiting org A with org B's router_user_id must not insert/bump B's monthly spend row")
}

// TestGetUserMonthlySpendAndLimit_RequiresUserOrgOwnership reproduces #796
// finding 3: spend/override subqueries must not return a foreign user's
// figures when organization_id and router_user_id belong to different tenants.
// Mismatch = silent miss (spent 0, no override); org default still resolves.
func TestGetUserMonthlySpendAndLimit_RequiresUserOrgOwnership(t *testing.T) {
	pool := openRouterPool(t, testDatabaseURL(t))
	ctx := context.Background()

	suffix := time.Now().UnixNano()
	orgA := fmt.Sprintf("796-rd-a-%d", suffix%1_000_000_000_000)
	orgB := fmt.Sprintf("796-rd-b-%d", suffix%1_000_000_000_000)
	instA, instB := uuid.New(), uuid.New()
	userB := uuid.New()
	const (
		orgADefault int64 = 1_111_111
		userBSpent  int64 = 424_242
	)

	_, err := pool.Exec(ctx, `
		INSERT INTO model_router_installations (id, external_id, name)
		VALUES ($1, $2, '796-rd-A'), ($3, $4, '796-rd-B')`,
		instA, orgA, instB, orgB)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO model_router_users (id, installation_id, email)
		VALUES ($1, $2, $3)`, userB, instB, fmt.Sprintf("b-rd-%d@example.com", suffix))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO organization_spend_limits (organization_id, user_monthly_limit_usd_micros)
		VALUES ($1, $2)`, orgA, orgADefault)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO model_router_user_monthly_spend (router_user_id, month, spent_usd_micros)
		VALUES ($1, DATE_TRUNC('month', NOW() AT TIME ZONE 'utc')::date, $2)`, userB, userBSpent)
	require.NoError(t, err)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM model_router_user_monthly_spend WHERE router_user_id = $1`, userB)
		_, _ = pool.Exec(c, `DELETE FROM organization_spend_limits WHERE organization_id = $1`, orgA)
		_, _ = pool.Exec(c, `DELETE FROM model_router_users WHERE id = $1`, userB)
		_, _ = pool.Exec(c, `DELETE FROM model_router_installations WHERE id IN ($1, $2)`, instA, instB)
	})

	repo := postgres.NewBillingRepo(pool)
	spent, limit, err := repo.GetUserMonthlySpendAndLimit(ctx, orgA, userB.String())
	require.NoError(t, err)

	require.Equal(t, int64(0), spent,
		"#796: org A + foreign user B must not return B's real spend (got %d)", spent)
	require.NotNil(t, limit)
	require.Equal(t, orgADefault, *limit,
		"org default limit for A must still resolve on a mismatched user read")
}
