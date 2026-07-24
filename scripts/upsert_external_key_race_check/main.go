// Command upsert_external_key_race_check races N=2 concurrent
// UpsertExternalAPIKey calls for the same (installation, provider) to see
// whether the UNIQUE (installation_id, provider) WHERE deleted_at IS NULL
// index cleanly fails the loser (vs. silently minting two live BYOK rows).
//
// SoftDeleteExternalAPIKeyByProvider is :exec (same shape as the RotateAPIKey
// bug); the question is whether Create's unique-violation error propagates.
//
// Gated on ROUTER_TEST_DATABASE_URL; no-op when unset.
//
//	ROUTER_TEST_DATABASE_URL="postgres://router:router@localhost:5433/router?search_path=router" \
//	    go run ./scripts/upsert_external_key_race_check
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/postgres"
	"workweave/router/internal/providers"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const n = 2

func main() {
	dsn := os.Getenv("ROUTER_TEST_DATABASE_URL")
	if dsn == "" {
		fmt.Println("ROUTER_TEST_DATABASE_URL not set; skipping live-DB upsert-external race check (see file header for usage)")
		return
	}
	ctx := context.Background()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		fail("parse database url", err)
	}
	cfg.MaxConns = 8
	cfg.MinConns = 2

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		fail("connect to database", err)
	}
	defer pool.Close()

	if err := checkUpsertExternalRace(ctx, pool); err != nil {
		fail("upsert external race check", err)
	}
}

func fail(step string, err error) {
	fmt.Fprintf(os.Stderr, "FAIL: %s: %v\n", step, err)
	os.Exit(1)
}

func checkUpsertExternalRace(ctx context.Context, pool *pgxpool.Pool) error {
	repo := postgres.NewRepository(pool, auth.NoOpEncryptor{})

	// Seed with the unwrapped repo so SoftDelete hold isn't engaged yet.
	seedSvc := auth.NewService(
		repo.Installations,
		repo.APIKeys,
		repo.ExternalAPIKeys,
		repo.Users,
		auth.NoOpAPIKeyCache{},
		nil,
		time.Now,
	)

	install, err := repo.Installations.Create(ctx, auth.CreateInstallationParams{
		ExternalID: "org_upsert_race_" + uuid.NewString()[:8],
		Name:       "UpsertExternalAPIKey race",
	})
	if err != nil {
		return fmt.Errorf("create installation: %w", err)
	}
	fmt.Printf("installation_id=%s\n", install.ID)
	fmt.Printf("provider=%s\n", providers.ProviderAnthropic)

	seedName := "seed-byok"
	seeded, err := seedSvc.UpsertExternalAPIKey(ctx, install.ID, providers.ProviderAnthropic, "sk-ant-race-seed-key", &seedName, nil)
	if err != nil {
		return fmt.Errorf("seed upsert: %w", err)
	}
	fmt.Printf("seeded_key_id=%s\n", seeded.ID)

	// Barrier AFTER SoftDeleteByProvider so both soft-deletes finish before
	// either Create — Upsert's SoftDelete→Create window held open.
	hold := newBarrier(n)
	var softDeleteCalls atomic.Int64
	ext := &holdingExternalRepo{
		inner:           repo.ExternalAPIKeys,
		hold:            hold,
		softDeleteCalls: &softDeleteCalls,
	}
	raceSvc := auth.NewService(
		repo.Installations,
		repo.APIKeys,
		ext,
		repo.Users,
		auth.NoOpAPIKeyCache{},
		nil,
		time.Now,
	)

	type result struct {
		key *auth.ExternalAPIKey
		err error
	}
	results := make([]result, n)

	var ready, done sync.WaitGroup
	start := make(chan struct{})
	ready.Add(n)
	done.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			name := fmt.Sprintf("race-byok-%d", i)
			key, err := raceSvc.UpsertExternalAPIKey(ctx, install.ID, providers.ProviderAnthropic,
				fmt.Sprintf("sk-ant-race-upsert-%d-%s", i, uuid.NewString()[:8]), &name, nil)
			results[i] = result{key: key, err: err}
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()

	var okCount, errCount int
	for i, r := range results {
		if r.err == nil {
			okCount++
			fmt.Printf("caller[%d] OK key_id=%s\n", i, r.key.ID)
			continue
		}
		errCount++
		fmt.Printf("caller[%d] ERR type=%T msg=%v\n", i, r.err, r.err)
		var pgErr *pgconn.PgError
		if errors.As(r.err, &pgErr) {
			fmt.Printf("caller[%d] pg_code=%s pg_constraint=%s pg_detail=%s\n",
				i, pgErr.Code, pgErr.ConstraintName, pgErr.Detail)
		} else {
			fmt.Printf("caller[%d] errors.As(*pgconn.PgError)=false (wrapped?)\n", i)
			fmt.Printf("caller[%d] errors.Unwrap=%v\n", i, errors.Unwrap(r.err))
		}
	}
	fmt.Printf("upsert_successes=%d upsert_errors=%d soft_delete_calls=%d\n",
		okCount, errCount, softDeleteCalls.Load())

	var active, deleted int
	if err := pool.QueryRow(ctx, `
		SELECT
		  COUNT(*) FILTER (WHERE deleted_at IS NULL),
		  COUNT(*) FILTER (WHERE deleted_at IS NOT NULL)
		FROM router.model_router_external_api_keys
		WHERE installation_id = $1::uuid AND provider = $2`,
		install.ID, providers.ProviderAnthropic,
	).Scan(&active, &deleted); err != nil {
		return fmt.Errorf("count rows: %w", err)
	}
	fmt.Printf("psql active=%d soft_deleted=%d (want active=1)\n", active, deleted)

	rows, err := pool.Query(ctx, `
		SELECT id, key_prefix, deleted_at IS NULL AS active, created_at
		FROM router.model_router_external_api_keys
		WHERE installation_id = $1::uuid AND provider = $2
		ORDER BY created_at`,
		install.ID, providers.ProviderAnthropic)
	if err != nil {
		return fmt.Errorf("select rows: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, prefix string
		var activeRow bool
		var created time.Time
		if err := rows.Scan(&id, &prefix, &activeRow, &created); err != nil {
			return err
		}
		fmt.Printf("  id=%s prefix=%s active=%v created_at=%s\n", id, prefix, activeRow, created.Format(time.RFC3339Nano))
	}

	switch {
	case okCount == 1 && errCount == 1 && active == 1:
		fmt.Println("RESULT: unique constraint stops zombie keys; loser error propagates to UpsertExternalAPIKey caller")
	case okCount == 2 && active == 2:
		fmt.Println("RESULT: ESCALATE — same zombie-key outcome as RotateAPIKey (constraint did not stop it)")
	default:
		fmt.Printf("RESULT: unexpected (ok=%d err=%d active=%d)\n", okCount, errCount, active)
	}
	return nil
}

type barrier struct {
	n       int
	mu      sync.Mutex
	arrived int
	release chan struct{}
}

func newBarrier(n int) *barrier {
	return &barrier{n: n, release: make(chan struct{})}
}

func (h *barrier) Wait() {
	h.mu.Lock()
	h.arrived++
	release := h.release
	if h.arrived == h.n {
		close(h.release)
		h.arrived = 0
		h.release = make(chan struct{})
	}
	h.mu.Unlock()
	<-release
}

// holdingExternalRepo parks SoftDeleteByProvider after the real UPDATE so both
// racers finish soft-delete before either Create.
type holdingExternalRepo struct {
	inner           auth.ExternalAPIKeyRepository
	hold            *barrier
	softDeleteCalls *atomic.Int64
}

func (r *holdingExternalRepo) Create(ctx context.Context, params auth.CreateExternalAPIKeyParams) (*auth.ExternalAPIKey, error) {
	return r.inner.Create(ctx, params)
}
func (r *holdingExternalRepo) GetForInstallation(ctx context.Context, installationID string) ([]*auth.ExternalAPIKey, error) {
	return r.inner.GetForInstallation(ctx, installationID)
}
func (r *holdingExternalRepo) SoftDeleteByProvider(ctx context.Context, installationID, provider string) error {
	err := r.inner.SoftDeleteByProvider(ctx, installationID, provider)
	if err != nil {
		return err
	}
	r.softDeleteCalls.Add(1)
	r.hold.Wait()
	return nil
}
func (r *holdingExternalRepo) SoftDelete(ctx context.Context, installationID, id string) error {
	return r.inner.SoftDelete(ctx, installationID, id)
}
func (r *holdingExternalRepo) MarkUsed(ctx context.Context, id string) error {
	return r.inner.MarkUsed(ctx, id)
}
