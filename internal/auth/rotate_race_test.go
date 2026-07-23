package auth_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workweave/router/internal/auth"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// statefulAPIKeyRepo is an in-memory APIKeyRepository whose SoftDelete
// counts rows affected (0 when already gone), matching live Postgres semantics.
type statefulAPIKeyRepo struct {
	mu     sync.Mutex
	keys   []*auth.APIKey
	nextID int
}

func newStatefulAPIKeyRepo() *statefulAPIKeyRepo {
	return &statefulAPIKeyRepo{}
}

func (r *statefulAPIKeyRepo) Create(_ context.Context, params auth.CreateAPIKeyParams) (*auth.APIKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	key := &auth.APIKey{
		ID:             "key-" + itoa(r.nextID),
		InstallationID: params.InstallationID,
		ExternalID:     params.ExternalID,
		Name:           params.Name,
		KeyPrefix:      params.KeyPrefix,
		KeyHash:        params.KeyHash,
		KeySuffix:      params.KeySuffix,
		CreatedBy:      params.CreatedBy,
		CreatedAt:      time.Now(),
	}
	r.keys = append(r.keys, key)
	return key, nil
}

func (r *statefulAPIKeyRepo) GetActiveByHashWithInstallation(context.Context, string) (*auth.APIKey, *auth.Installation, error) {
	return nil, nil, assert.AnError
}

func (r *statefulAPIKeyRepo) ListForInstallation(_ context.Context, installationID string) ([]*auth.APIKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*auth.APIKey, 0, len(r.keys))
	for _, k := range r.keys {
		if k.InstallationID == installationID && k.DeletedAt == nil {
			out = append(out, k)
		}
	}
	return out, nil
}

func (r *statefulAPIKeyRepo) MarkUsed(context.Context, string) error { return nil }

func (r *statefulAPIKeyRepo) SoftDelete(_ context.Context, installationID, id string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range r.keys {
		if k.ID == id && k.InstallationID == installationID && k.DeletedAt == nil {
			now := time.Now()
			k.DeletedAt = &now
			return 1, nil
		}
	}
	return 0, nil
}

func (r *statefulAPIKeyRepo) activeCount(installationID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, k := range r.keys {
		if k.InstallationID == installationID && k.DeletedAt == nil {
			n++
		}
	}
	return n
}

func (r *statefulAPIKeyRepo) createCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.keys)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// listHoldRepo parks ListForInstallation after the read so SoftDelete cannot
// run until all waiters arrive — same TOCTOU window as the live #817 repro.
type listHoldRepo struct {
	inner   *statefulAPIKeyRepo
	n       int
	mu      sync.Mutex
	arrived int
	release chan struct{}
}

func newListHoldRepo(inner *statefulAPIKeyRepo, n int) *listHoldRepo {
	return &listHoldRepo{inner: inner, n: n, release: make(chan struct{})}
}

func (r *listHoldRepo) Create(ctx context.Context, params auth.CreateAPIKeyParams) (*auth.APIKey, error) {
	return r.inner.Create(ctx, params)
}
func (r *listHoldRepo) GetActiveByHashWithInstallation(ctx context.Context, keyHash string) (*auth.APIKey, *auth.Installation, error) {
	return r.inner.GetActiveByHashWithInstallation(ctx, keyHash)
}
func (r *listHoldRepo) ListForInstallation(ctx context.Context, installationID string) ([]*auth.APIKey, error) {
	keys, err := r.inner.ListForInstallation(ctx, installationID)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.arrived++
	release := r.release
	if r.arrived == r.n {
		close(r.release)
		r.arrived = 0
		r.release = make(chan struct{})
	}
	r.mu.Unlock()
	<-release
	return keys, nil
}
func (r *listHoldRepo) MarkUsed(ctx context.Context, id string) error {
	return r.inner.MarkUsed(ctx, id)
}
func (r *listHoldRepo) SoftDelete(ctx context.Context, installationID, id string) (int64, error) {
	return r.inner.SoftDelete(ctx, installationID, id)
}

// signalListRepo parks a single List until release closes (Rotate-vs-Delete).
type signalListRepo struct {
	inner   *statefulAPIKeyRepo
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

const raceInstallID = "inst-rotate-race"

// TestRotateAPIKey_ConcurrentRace_OnlyOneSucceeds verifies the #817 fix:
// the losing racer must return ErrAPIKeyNotFound, not mint an orphan key.
func TestRotateAPIKey_ConcurrentRace_OnlyOneSucceeds(t *testing.T) {
	inner := newStatefulAPIKeyRepo()
	held := newListHoldRepo(inner, 2)
	svc := auth.NewService(nil, held, nil, nil, auth.NoOpAPIKeyCache{}, nil, time.Now)

	name := "race-key"
	issued, _, err := svc.IssueAPIKey(context.Background(), raceInstallID, &name, nil)
	require.NoError(t, err)
	require.Equal(t, 1, inner.activeCount(raceInstallID))

	var (
		ready   sync.WaitGroup
		done    sync.WaitGroup
		start   = make(chan struct{})
		okCount atomic.Int64
		errNF   atomic.Int64
	)
	ready.Add(2)
	done.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			_, _, err := svc.RotateAPIKey(context.Background(), raceInstallID, issued.ID, nil)
			if err == nil {
				okCount.Add(1)
				return
			}
			if errors.Is(err, auth.ErrAPIKeyNotFound) {
				errNF.Add(1)
				return
			}
			t.Errorf("unexpected RotateAPIKey error: %v", err)
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()

	assert.Equal(t, int64(1), okCount.Load(), "exactly one RotateAPIKey must succeed")
	assert.Equal(t, int64(1), errNF.Load(), "the loser must return ErrAPIKeyNotFound")
	assert.Equal(t, 1, inner.activeCount(raceInstallID),
		"exactly one active successor; loser must not mint an orphan key")
	assert.Equal(t, 2, inner.createCount(),
		"seed + one successor only (not seed + two successors)")
}

// TestRotateAPIKey_LosingToDelete_DoesNotIssue verifies the #817 Delete-vs-Rotate
// fix: Rotate must return ErrAPIKeyNotFound and not mint a zombie successor.
func TestRotateAPIKey_LosingToDelete_DoesNotIssue(t *testing.T) {
	inner := newStatefulAPIKeyRepo()
	listed := make(chan struct{})
	release := make(chan struct{})
	wrapped := &signalListRepo{inner: inner, listed: listed, release: release}
	svc := auth.NewService(nil, wrapped, nil, nil, auth.NoOpAPIKeyCache{}, nil, time.Now)

	name := "mixed-key"
	issued, _, err := svc.IssueAPIKey(context.Background(), raceInstallID, &name, nil)
	require.NoError(t, err)
	seedCreates := inner.createCount()

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
		_, _, rotateErr = svc.RotateAPIKey(context.Background(), raceInstallID, issued.ID, nil)
	}()
	go func() {
		defer done.Done()
		ready.Done()
		<-start
		<-listed
		deleteErr = svc.DeleteAPIKey(context.Background(), raceInstallID, issued.ID)
		close(release)
	}()
	ready.Wait()
	close(start)
	done.Wait()

	require.NoError(t, deleteErr, "DeleteAPIKey must succeed")
	assert.ErrorIs(t, rotateErr, auth.ErrAPIKeyNotFound,
		"RotateAPIKey must not mint after Delete already soft-deleted the key")
	assert.Equal(t, 0, inner.activeCount(raceInstallID),
		"no zombie successor after Delete wins")
	assert.Equal(t, seedCreates, inner.createCount(),
		"Rotate must not IssueAPIKey when SoftDelete matched 0 rows")
}
