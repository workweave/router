// Command rotate_key_race_check reproduces a concurrent RotateAPIKey race
// against a live Postgres: N goroutines soft-delete + re-issue the same key
// with no transaction / rows-affected check, so more than one successor key
// can end up live from a single rotation intent.
//
// It is a separate main package (not a _test.go), so `go test ./...` never
// touches Postgres. It is gated on ROUTER_TEST_DATABASE_URL (a DSN to a
// database with the router migrations applied) and is a no-op without it.
//
// Timing note: a start-only barrier (line up, then release into RotateAPIKey)
// does not hit the race in practice — List→SoftDelete is microseconds, so one
// goroutine soft-deletes before the others list (they then get
// ErrAPIKeyNotFound). The pool is not the limiter (peak acquired == N). A
// second barrier inside ListForInstallation holds every caller after the
// pre-delete read and before SoftDelete, which is the actual TOCTOU window.
// SoftDelete stays the real :exec SQLC path (silent no-op on 0 rows).
//
// Usage (from the repo root, against the docker-compose Postgres):
//
//	ROUTER_TEST_DATABASE_URL="postgres://router:router@localhost:5433/router?search_path=router" \
//	    go run ./scripts/rotate_key_race_check
package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/postgres"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const concurrentRotations = 5

func main() {
	dsn := os.Getenv("ROUTER_TEST_DATABASE_URL")
	if dsn == "" {
		fmt.Println("ROUTER_TEST_DATABASE_URL not set; skipping live-DB rotate-key race check (see file header for usage)")
		return
	}
	ctx := context.Background()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		fail("parse database url", err)
	}
	// Enough connections that N concurrent RotateAPIKey calls are not
	// artificially serialized by the pool (default is already ≥4).
	cfg.MaxConns = int32(concurrentRotations + 2)
	cfg.MinConns = int32(concurrentRotations)

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		fail("connect to database", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		fail("ping database", err)
	}

	if err := checkRotateKeyRace(ctx, pool); err != nil {
		fail("rotate-key race check", err)
	}
}

func fail(step string, err error) {
	fmt.Fprintf(os.Stderr, "FAIL: %s: %v\n", step, err)
	os.Exit(1)
}

// checkRotateKeyRace creates a fresh installation + one API key, then fires
// concurrentRotations barrier-synchronized RotateAPIKey calls on that same
// keyID and reports how many succeeded vs how many active keys remain.
func checkRotateKeyRace(ctx context.Context, pool *pgxpool.Pool) error {
	repo := postgres.NewRepository(pool, auth.NoOpEncryptor{})

	// Hold every caller between List and SoftDelete to open the TOCTOU window;
	// a start-only barrier misses it because List→SoftDelete is microseconds.
	listHold := newListHold(concurrentRotations)
	apiKeys := &holdingAPIKeyRepo{inner: repo.APIKeys, hold: listHold}

	svc := auth.NewService(
		repo.Installations,
		apiKeys,
		repo.ExternalAPIKeys,
		repo.Users,
		auth.NoOpAPIKeyCache{},
		nil,
		time.Now,
	)

	install, err := repo.Installations.Create(ctx, auth.CreateInstallationParams{
		ExternalID: "org_rotate_race_" + uuid.NewString()[:8],
		Name:       "RotateAPIKey race repro",
	})
	if err != nil {
		return fmt.Errorf("create installation: %w", err)
	}
	fmt.Printf("installation_id=%s\n", install.ID)

	name := "race-repro-key"
	issued, _, err := svc.IssueAPIKey(ctx, install.ID, &name, nil)
	if err != nil {
		return fmt.Errorf("issue initial key: %w", err)
	}
	fmt.Printf("initial_key_id=%s\n", issued.ID)

	var (
		ready   sync.WaitGroup
		done    sync.WaitGroup
		start   = make(chan struct{})
		okCount atomic.Int64
		stopMon = make(chan struct{})
		peak    atomic.Int32
	)
	ready.Add(concurrentRotations)
	done.Add(concurrentRotations)

	go func() {
		for {
			select {
			case <-stopMon:
				return
			default:
				if c := int32(pool.Stat().AcquiredConns()); c > peak.Load() {
					peak.Store(c)
				}
				time.Sleep(50 * time.Microsecond)
			}
		}
	}()

	for i := 0; i < concurrentRotations; i++ {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			_, _, err := svc.RotateAPIKey(ctx, install.ID, issued.ID, nil)
			if err == nil {
				okCount.Add(1)
			} else {
				fmt.Printf("RotateAPIKey error: %v\n", err)
			}
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()
	close(stopMon)

	// Use the unwrapped repo: holdingAPIKeyRepo would block forever waiting
	// for N more List arrivals that never come.
	keys, err := repo.APIKeys.ListForInstallation(ctx, install.ID)
	if err != nil {
		return fmt.Errorf("list API keys: %w", err)
	}

	fmt.Printf("concurrent_calls=%d\n", concurrentRotations)
	fmt.Printf("rotate_successes=%d\n", okCount.Load())
	fmt.Printf("active_keys=%d\n", len(keys))
	fmt.Printf("pool_max_conns=%d\n", pool.Stat().MaxConns())
	fmt.Printf("pool_peak_acquired_conns=%d\n", peak.Load())
	fmt.Printf("list_hold_releases=%d\n", listHold.releases.Load())
	for _, k := range keys {
		fmt.Printf("  active key_id=%s prefix=%s\n", k.ID, k.KeyPrefix)
	}
	if okCount.Load() > 1 && len(keys) > 1 {
		fmt.Println("RACE CONFIRMED: multiple RotateAPIKey calls succeeded and multiple active keys remain")
	} else {
		fmt.Println("race not observed this run (only one successor / one success)")
	}
	return nil
}

// listHold is a reusable N-party barrier: the Nth arrival closes release so
// every waiter proceeds together.
type listHold struct {
	n        int
	mu       sync.Mutex
	arrived  int
	release  chan struct{}
	releases atomic.Int64
}

func newListHold(n int) *listHold {
	return &listHold{n: n, release: make(chan struct{})}
}

func (h *listHold) Wait() {
	h.mu.Lock()
	h.arrived++
	release := h.release
	if h.arrived == h.n {
		h.releases.Add(1)
		close(h.release)
		h.arrived = 0
		h.release = make(chan struct{})
	}
	h.mu.Unlock()
	<-release
}

// holdingAPIKeyRepo wraps an APIKeyRepository and parks every
// ListForInstallation caller at listHold so SoftDelete cannot run until all
// concurrent rotators have observed the pre-delete key set.
type holdingAPIKeyRepo struct {
	inner auth.APIKeyRepository
	hold  *listHold
}

func (r *holdingAPIKeyRepo) Create(ctx context.Context, params auth.CreateAPIKeyParams) (*auth.APIKey, error) {
	return r.inner.Create(ctx, params)
}

func (r *holdingAPIKeyRepo) GetActiveByHashWithInstallation(ctx context.Context, keyHash string) (*auth.APIKey, *auth.Installation, error) {
	return r.inner.GetActiveByHashWithInstallation(ctx, keyHash)
}

func (r *holdingAPIKeyRepo) ListForInstallation(ctx context.Context, installationID string) ([]*auth.APIKey, error) {
	keys, err := r.inner.ListForInstallation(ctx, installationID)
	if err != nil {
		return nil, err
	}
	r.hold.Wait()
	return keys, nil
}

func (r *holdingAPIKeyRepo) MarkUsed(ctx context.Context, id string) error {
	return r.inner.MarkUsed(ctx, id)
}

func (r *holdingAPIKeyRepo) SoftDelete(ctx context.Context, installationID, id string) (int64, error) {
	return r.inner.SoftDelete(ctx, installationID, id)
}
