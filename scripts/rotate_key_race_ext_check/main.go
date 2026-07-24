// Command rotate_key_race_ext_check extends the RotateAPIKey race coverage
// confirmed by scripts/rotate_key_race_check. It is a separate main package
// (not a _test.go), gated on ROUTER_TEST_DATABASE_URL, and a no-op without it.
//
// Axes:
//  1. Concurrency sweep — List-held-open barrier at N = 2, 3, 10, 25
//  2. Mixed RotateAPIKey vs DeleteAPIKey on the same key
//  3. HTTP-level POST /admin/v1/keys/:id/rotate via gin httptest + real
//     WithAdminOnly cookie auth (not a live docker listener)
//  4. Timing sensitivity — N=2, 20 runs each: post-List sleep sweep, then
//     start-stagger sweep to find the largest stagger with ≥1/20 hit
//
// Usage (from the repo root, against the docker-compose Postgres):
//
//	ROUTER_TEST_DATABASE_URL="postgres://router:router@localhost:5433/router?search_path=router" \
//	    go run ./scripts/rotate_key_race_ext_check
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"workweave/router/internal/api/admin"
	"workweave/router/internal/auth"
	"workweave/router/internal/postgres"
	"workweave/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	dsn := os.Getenv("ROUTER_TEST_DATABASE_URL")
	if dsn == "" {
		fmt.Println("ROUTER_TEST_DATABASE_URL not set; skipping live-DB rotate-key race ext check (see file header for usage)")
		return
	}
	gin.SetMode(gin.TestMode)
	ctx := context.Background()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		fail("parse database url", err)
	}
	cfg.MaxConns = 32
	cfg.MinConns = 8

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		fail("connect to database", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		fail("ping database", err)
	}

	fmt.Println("=== 1. Concurrency sweep (List-held-open) ===")
	if err := checkConcurrencySweep(ctx, pool); err != nil {
		fail("concurrency sweep", err)
	}

	fmt.Println("\n=== 2. Mixed RotateAPIKey vs DeleteAPIKey ===")
	if err := checkRotateVsDelete(ctx, pool); err != nil {
		fail("rotate vs delete", err)
	}

	fmt.Println("\n=== 3. HTTP-level rotate race (httptest + WithAdminOnly) ===")
	if err := checkHTTPRotateRace(ctx, pool); err != nil {
		fail("http rotate race", err)
	}

	fmt.Println("\n=== 4. Timing sensitivity (N=2, 20 runs each) ===")
	if err := checkTimingSensitivity(ctx, pool); err != nil {
		fail("timing sensitivity", err)
	}
}

func fail(step string, err error) {
	fmt.Fprintf(os.Stderr, "FAIL: %s: %v\n", step, err)
	os.Exit(1)
}

// ---------------------------------------------------------------------------
// 1. Concurrency sweep
// ---------------------------------------------------------------------------

func checkConcurrencySweep(ctx context.Context, pool *pgxpool.Pool) error {
	fmt.Printf("%4s  %10s  %11s  %s\n", "N", "successes", "active_keys", "notes")
	for _, n := range []int{2, 3, 10, 25} {
		successes, active, errCounts, err := runHeldRotateRace(ctx, pool, n)
		if err != nil {
			return fmt.Errorf("N=%d: %w", n, err)
		}
		note := "linear"
		if successes != n || active != n {
			note = fmt.Sprintf("NON-LINEAR err_counts=%v", errCounts)
		}
		fmt.Printf("%4d  %10d  %11d  %s\n", n, successes, active, note)
	}
	return nil
}

func runHeldRotateRace(ctx context.Context, pool *pgxpool.Pool, n int) (successes, active int, errCounts map[string]int, err error) {
	repo := postgres.NewRepository(pool, auth.NoOpEncryptor{})
	hold := newListHold(n)
	svc := auth.NewService(
		repo.Installations,
		&holdingAPIKeyRepo{inner: repo.APIKeys, hold: hold},
		repo.ExternalAPIKeys,
		repo.Users,
		auth.NoOpAPIKeyCache{},
		nil,
		time.Now,
	)

	install, err := repo.Installations.Create(ctx, auth.CreateInstallationParams{
		ExternalID: "org_rot_sweep_" + uuid.NewString()[:8],
		Name:       fmt.Sprintf("rotate sweep N=%d", n),
	})
	if err != nil {
		return 0, 0, nil, fmt.Errorf("create installation: %w", err)
	}
	name := "sweep-key"
	issued, _, err := svc.IssueAPIKey(ctx, install.ID, &name, nil)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("issue key: %w", err)
	}

	var (
		ready   sync.WaitGroup
		done    sync.WaitGroup
		start   = make(chan struct{})
		okCount atomic.Int64
		errMu   sync.Mutex
	)
	errCounts = map[string]int{}
	ready.Add(n)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			_, _, err := svc.RotateAPIKey(ctx, install.ID, issued.ID, nil)
			if err == nil {
				okCount.Add(1)
				return
			}
			errMu.Lock()
			errCounts[err.Error()]++
			errMu.Unlock()
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()

	keys, err := repo.APIKeys.ListForInstallation(ctx, install.ID)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("list keys: %w", err)
	}
	return int(okCount.Load()), len(keys), errCounts, nil
}

// ---------------------------------------------------------------------------
// 2. Mixed Rotate vs Delete
// ---------------------------------------------------------------------------

// checkRotateVsDelete forces DeleteAPIKey to win SoftDelete after Rotate has
// already Listed the key as active. Expected buggy outcome: Rotate still
// IssueAPIKeys a live successor even though the key was deleted (not rotated).
func checkRotateVsDelete(ctx context.Context, pool *pgxpool.Pool) error {
	const trials = 10
	var (
		zombieIssued int // rotate succeeded after delete won SoftDelete
		rotateFails  int
		deleteFails  int
	)

	for i := 0; i < trials; i++ {
		outcome, err := runOneRotateVsDelete(ctx, pool)
		if err != nil {
			return err
		}
		if outcome.deleteErr != nil {
			deleteFails++
		}
		if outcome.rotateErr != nil {
			rotateFails++
			continue
		}
		// Rotate returned a new key. If Delete already soft-deleted the
		// original, that new key is a zombie replacement for a deleted key.
		if outcome.deleteWonSoftDelete && outcome.activeAfter > 0 {
			zombieIssued++
		}
	}

	fmt.Printf("trials=%d zombie_replacements=%d rotate_errors=%d delete_errors=%d\n",
		trials, zombieIssued, rotateFails, deleteFails)
	if zombieIssued > 0 {
		fmt.Println("YES: DeleteAPIKey winning SoftDelete still lets RotateAPIKey mint a live replacement")
	} else {
		fmt.Println("NO: did not observe a zombie replacement this run")
	}
	return nil
}

type rotateVsDeleteOutcome struct {
	rotateErr           error
	deleteErr           error
	deleteWonSoftDelete bool
	activeAfter         int
}

func runOneRotateVsDelete(ctx context.Context, pool *pgxpool.Pool) (rotateVsDeleteOutcome, error) {
	repo := postgres.NewRepository(pool, auth.NoOpEncryptor{})

	listed := make(chan struct{})
	releaseRotate := make(chan struct{})
	apiKeys := &signalListRepo{
		inner:   repo.APIKeys,
		listed:  listed,
		release: releaseRotate,
	}
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
		ExternalID: "org_rot_del_" + uuid.NewString()[:8],
		Name:       "rotate vs delete",
	})
	if err != nil {
		return rotateVsDeleteOutcome{}, fmt.Errorf("create installation: %w", err)
	}
	name := "mixed-key"
	issued, _, err := svc.IssueAPIKey(ctx, install.ID, &name, nil)
	if err != nil {
		return rotateVsDeleteOutcome{}, fmt.Errorf("issue key: %w", err)
	}

	var (
		ready     sync.WaitGroup
		done      sync.WaitGroup
		start     = make(chan struct{})
		rotateErr error
		deleteErr error
	)
	ready.Add(2)
	done.Add(2)

	go func() {
		defer done.Done()
		ready.Done()
		<-start
		_, _, rotateErr = svc.RotateAPIKey(ctx, install.ID, issued.ID, nil)
	}()
	go func() {
		defer done.Done()
		ready.Done()
		<-start
		<-listed // Rotate has Listed; key still appears active to it
		deleteErr = svc.DeleteAPIKey(ctx, install.ID, issued.ID)
		close(releaseRotate) // let Rotate proceed to SoftDelete (no-op) + Issue
	}()

	ready.Wait()
	close(start)
	done.Wait()

	keys, err := repo.APIKeys.ListForInstallation(ctx, install.ID)
	if err != nil {
		return rotateVsDeleteOutcome{}, fmt.Errorf("list keys: %w", err)
	}
	return rotateVsDeleteOutcome{
		rotateErr:           rotateErr,
		deleteErr:           deleteErr,
		deleteWonSoftDelete: deleteErr == nil,
		activeAfter:         len(keys),
	}, nil
}

// signalListRepo parks ListForInstallation after the read until release closes,
// and signals listed once the pre-delete snapshot is in hand.
type signalListRepo struct {
	inner   auth.APIKeyRepository
	listed  chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *signalListRepo) Create(ctx context.Context, params auth.CreateAPIKeyParams) (*auth.APIKey, error) {
	return r.inner.Create(ctx, params)
}
func (r *signalListRepo) GetActiveByHashWithInstallation(ctx context.Context, keyHash string) (*auth.APIKey, *auth.Installation, error) {
	return r.inner.GetActiveByHashWithInstallation(ctx, keyHash)
}
func (r *signalListRepo) ListForInstallation(ctx context.Context, installationID string) ([]*auth.APIKey, error) {
	keys, err := r.inner.ListForInstallation(ctx, installationID)
	if err != nil {
		return nil, err
	}
	r.once.Do(func() { close(r.listed) })
	<-r.release
	return keys, nil
}
func (r *signalListRepo) MarkUsed(ctx context.Context, id string) error {
	return r.inner.MarkUsed(ctx, id)
}
func (r *signalListRepo) SoftDelete(ctx context.Context, installationID, id string) (int64, error) {
	return r.inner.SoftDelete(ctx, installationID, id)
}

// ---------------------------------------------------------------------------
// 3. HTTP-level race
// ---------------------------------------------------------------------------

func checkHTTPRotateRace(ctx context.Context, pool *pgxpool.Pool) error {
	const n = 5
	const adminPassword = "rotate-race-http-check-password"

	repo := postgres.NewRepository(pool, auth.NoOpEncryptor{})
	hold := newListHold(n)
	svc := auth.NewService(
		repo.Installations,
		&holdingAPIKeyRepo{inner: repo.APIKeys, hold: hold},
		repo.ExternalAPIKeys,
		repo.Users,
		auth.NoOpAPIKeyCache{},
		nil,
		time.Now,
	).WithAdminPassword(adminPassword)

	// Admin cookie sessions operate on EnsureAdminInstallation's singleton.
	install, err := svc.EnsureAdminInstallation(ctx)
	if err != nil {
		return fmt.Errorf("ensure admin installation: %w", err)
	}
	name := "http-race-key-" + uuid.NewString()[:8]
	issued, _, err := svc.IssueAPIKey(ctx, install.ID, &name, nil)
	if err != nil {
		return fmt.Errorf("issue key: %w", err)
	}
	fmt.Printf("method=httptest+WithAdminOnly installation_id=%s initial_key_id=%s\n", install.ID, issued.ID)

	session, _, err := svc.IssueAdminSession()
	if err != nil {
		return fmt.Errorf("issue admin session: %w", err)
	}

	engine := gin.New()
	mgmt := engine.Group("/admin/v1", middleware.WithAdminOnly(svc))
	mgmt.POST("/keys/:id/rotate", admin.RotateAPIKeyHandler(svc))

	var (
		ready    sync.WaitGroup
		done     sync.WaitGroup
		start    = make(chan struct{})
		okCount  atomic.Int64
		statusMu sync.Mutex
		statuses = map[int]int{}
	)
	ready.Add(n)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/admin/v1/keys/"+issued.ID+"/rotate", nil)
			req.AddCookie(&http.Cookie{Name: auth.AdminSessionCookieName, Value: session})
			engine.ServeHTTP(rec, req)
			statusMu.Lock()
			statuses[rec.Code]++
			statusMu.Unlock()
			if rec.Code == http.StatusCreated {
				okCount.Add(1)
			}
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()

	keys, err := repo.APIKeys.ListForInstallation(ctx, install.ID)
	if err != nil {
		return fmt.Errorf("list keys: %w", err)
	}
	// Count only keys from this race (name match) — admin install may have leftovers.
	activeNamed := 0
	for _, k := range keys {
		if k.Name != nil && *k.Name == name {
			activeNamed++
		}
	}

	fmt.Printf("http_status_counts=%v\n", statuses)
	fmt.Printf("rotate_http_201=%d active_keys_with_race_name=%d\n", okCount.Load(), activeNamed)
	if okCount.Load() > 1 && activeNamed > 1 {
		fmt.Println("YES: race reproduces through gin WithAdminOnly + RotateAPIKeyHandler (httptest)")
	} else {
		fmt.Println("NO: HTTP path did not show multi-successor race")
	}
	return nil
}

// ---------------------------------------------------------------------------
// 4. Timing sensitivity
// ---------------------------------------------------------------------------

func checkTimingSensitivity(ctx context.Context, pool *pgxpool.Pool) error {
	const trials = 20

	// A) Simultaneous start, optional post-List sleep (widens SoftDelete delay).
	fmt.Println("-- A) simultaneous start, post-List sleep --")
	fmt.Printf("%12s  %8s  %s\n", "post_list_sleep", "hits/20", "min_list_gap_on_hit")
	delays := []time.Duration{
		0,
		time.Microsecond,
		10 * time.Microsecond,
		100 * time.Microsecond,
		time.Millisecond,
	}
	for _, delay := range delays {
		hits, minGap, err := sweepTimedPairs(ctx, pool, trials, delay, 0)
		if err != nil {
			return err
		}
		gapStr := "-"
		if minGap >= 0 {
			gapStr = minGap.String()
		}
		fmt.Printf("%12s  %3d/20    %s\n", delay, hits, gapStr)
	}

	// B) No post-List sleep; stagger second caller's start by G after the
	// first begins. Largest G with ≥1/20 hit ≈ real-world overlap window
	// (e.g. retry-on-timeout re-sending rotate).
	fmt.Println("-- B) start-stagger of 2nd caller (no post-List sleep) --")
	fmt.Printf("%12s  %8s  %s\n", "start_stagger", "hits/20", "max_list_gap_on_hit")
	staggers := []time.Duration{
		0,
		10 * time.Microsecond,
		50 * time.Microsecond,
		100 * time.Microsecond,
		250 * time.Microsecond,
		500 * time.Microsecond,
		time.Millisecond,
		2 * time.Millisecond,
		5 * time.Millisecond,
		10 * time.Millisecond,
	}
	var maxHitStagger time.Duration = -1
	for _, stagger := range staggers {
		hits, maxGap, err := sweepTimedPairs(ctx, pool, trials, 0, stagger)
		if err != nil {
			return err
		}
		gapStr := "-"
		if maxGap >= 0 {
			gapStr = maxGap.String()
		}
		fmt.Printf("%12s  %3d/20    %s\n", stagger, hits, gapStr)
		if hits > 0 {
			maxHitStagger = stagger
		}
	}
	if maxHitStagger < 0 {
		fmt.Println("timing_threshold: race never hit under start-stagger sweep")
	} else {
		fmt.Printf("timing_threshold: largest start-stagger with ≥1/20 hit = %s\n", maxHitStagger)
		fmt.Println("(N=2 uncoordinated overlap within that stagger is enough; tighter gaps hit more often)")
	}
	return nil
}

func sweepTimedPairs(ctx context.Context, pool *pgxpool.Pool, trials int, postListSleep, startStagger time.Duration) (hits int, extremeGap time.Duration, err error) {
	extremeGap = -1
	for t := 0; t < trials; t++ {
		hit, gap, err := runTimedRotatePair(ctx, pool, postListSleep, startStagger)
		if err != nil {
			return 0, -1, err
		}
		if !hit {
			continue
		}
		hits++
		// For sleep sweep report min gap; for stagger sweep report max gap.
		if startStagger > 0 {
			if extremeGap < 0 || gap > extremeGap {
				extremeGap = gap
			}
		} else if extremeGap < 0 || gap < extremeGap {
			extremeGap = gap
		}
	}
	return hits, extremeGap, nil
}

func runTimedRotatePair(ctx context.Context, pool *pgxpool.Pool, postListSleep, startStagger time.Duration) (hit bool, listGap time.Duration, err error) {
	repo := postgres.NewRepository(pool, auth.NoOpEncryptor{})
	timed := &timedListRepo{inner: repo.APIKeys, sleep: postListSleep}
	svc := auth.NewService(
		repo.Installations,
		timed,
		repo.ExternalAPIKeys,
		repo.Users,
		auth.NoOpAPIKeyCache{},
		nil,
		time.Now,
	)

	install, err := repo.Installations.Create(ctx, auth.CreateInstallationParams{
		ExternalID: "org_rot_time_" + uuid.NewString()[:8],
		Name:       "timing",
	})
	if err != nil {
		return false, 0, fmt.Errorf("create installation: %w", err)
	}
	name := "timing-key"
	issued, _, err := svc.IssueAPIKey(ctx, install.ID, &name, nil)
	if err != nil {
		return false, 0, fmt.Errorf("issue key: %w", err)
	}

	var (
		done    sync.WaitGroup
		okCount atomic.Int64
	)
	done.Add(2)
	go func() {
		defer done.Done()
		_, _, err := svc.RotateAPIKey(ctx, install.ID, issued.ID, nil)
		if err == nil {
			okCount.Add(1)
		}
	}()
	go func() {
		defer done.Done()
		if startStagger > 0 {
			time.Sleep(startStagger)
		}
		_, _, err := svc.RotateAPIKey(ctx, install.ID, issued.ID, nil)
		if err == nil {
			okCount.Add(1)
		}
	}()
	done.Wait()

	keys, err := repo.APIKeys.ListForInstallation(ctx, install.ID)
	if err != nil {
		return false, 0, fmt.Errorf("list keys: %w", err)
	}
	return okCount.Load() == 2 && len(keys) >= 2, timed.listGap(), nil
}

// timedListRepo optionally sleeps after each List and records List-completion
// timestamps so we can report the observed inter-List gap when a race hits.
type timedListRepo struct {
	inner auth.APIKeyRepository
	sleep time.Duration

	mu        sync.Mutex
	listTimes []time.Time
}

func (r *timedListRepo) Create(ctx context.Context, params auth.CreateAPIKeyParams) (*auth.APIKey, error) {
	return r.inner.Create(ctx, params)
}
func (r *timedListRepo) GetActiveByHashWithInstallation(ctx context.Context, keyHash string) (*auth.APIKey, *auth.Installation, error) {
	return r.inner.GetActiveByHashWithInstallation(ctx, keyHash)
}
func (r *timedListRepo) ListForInstallation(ctx context.Context, installationID string) ([]*auth.APIKey, error) {
	keys, err := r.inner.ListForInstallation(ctx, installationID)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.listTimes = append(r.listTimes, time.Now())
	r.mu.Unlock()
	if r.sleep > 0 {
		time.Sleep(r.sleep)
	}
	return keys, nil
}
func (r *timedListRepo) MarkUsed(ctx context.Context, id string) error {
	return r.inner.MarkUsed(ctx, id)
}
func (r *timedListRepo) SoftDelete(ctx context.Context, installationID, id string) (int64, error) {
	return r.inner.SoftDelete(ctx, installationID, id)
}

func (r *timedListRepo) listGap() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.listTimes) < 2 {
		return 0
	}
	d := r.listTimes[1].Sub(r.listTimes[0])
	if d < 0 {
		return -d
	}
	return d
}

// ---------------------------------------------------------------------------
// Shared list-hold helpers (same pattern as rotate_key_race_check)
// ---------------------------------------------------------------------------

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
