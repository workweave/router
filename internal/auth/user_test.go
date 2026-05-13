package auth_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"workweave/router/internal/auth"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeUserRepo struct {
	mu             sync.Mutex
	upserts        []auth.UpsertUserParams
	accountUpserts []auth.UpsertUserByAccountUUIDParams
	user           *auth.User
	err            error
}

func (f *fakeUserRepo) UpsertByEmail(ctx context.Context, params auth.UpsertUserParams) (*auth.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts = append(f.upserts, params)
	if f.err != nil {
		return nil, f.err
	}
	return f.user, nil
}

func (f *fakeUserRepo) UpsertByAccountUUID(ctx context.Context, params auth.UpsertUserByAccountUUIDParams) (*auth.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accountUpserts = append(f.accountUpserts, params)
	if f.err != nil {
		return nil, f.err
	}
	return f.user, nil
}

func (f *fakeUserRepo) Get(ctx context.Context, id string) (*auth.User, error) {
	return nil, errors.New("not used by these tests")
}

func (f *fakeUserRepo) ListForInstallation(ctx context.Context, installationID string) ([]*auth.User, error) {
	return nil, errors.New("not used by these tests")
}

func makeServiceWithUsers(t *testing.T, users auth.UserRepository) *auth.Service {
	t.Helper()
	return auth.NewService(
		&fakeInstallationRepository{},
		&fakeAPIKeyRepository{byHash: map[string]fakeKeyRow{}},
		nil,
		users,
		auth.NoOpAPIKeyCache{},
		nil,
		frozenClock(),
	)
}

func TestResolveAndStashUser_UpsertsAndStashesID(t *testing.T) {
	repo := &fakeUserRepo{user: &auth.User{ID: "user-42", InstallationID: "inst-1", Email: "alice@example.com"}}
	svc := makeServiceWithUsers(t, repo)

	ctx := svc.ResolveAndStashUser(context.Background(), "inst-1", "alice@example.com", "claude-acct-9")

	require.Len(t, repo.upserts, 1)
	assert.Equal(t, "inst-1", repo.upserts[0].InstallationID)
	assert.Equal(t, "alice@example.com", repo.upserts[0].Email)
	require.NotNil(t, repo.upserts[0].ClaudeAccountUUID)
	assert.Equal(t, "claude-acct-9", *repo.upserts[0].ClaudeAccountUUID)
	assert.Equal(t, "user-42", auth.UserIDFrom(ctx))
}

func TestResolveAndStashUser_NoIdentitySignalIsNoOp(t *testing.T) {
	repo := &fakeUserRepo{}
	svc := makeServiceWithUsers(t, repo)

	ctx := svc.ResolveAndStashUser(context.Background(), "inst-1", "", "")

	assert.Empty(t, repo.upserts)
	assert.Empty(t, repo.accountUpserts)
	assert.Equal(t, "", auth.UserIDFrom(ctx))
}

func TestResolveAndStashUser_AccountUUIDOnlyUsesAccountUpsert(t *testing.T) {
	// Claude CLI v2.1.x packs only {device_id, account_uuid, session_id}
	// into metadata.user_id — no email. Per-seat attribution must still
	// work via the account_uuid-keyed upsert path.
	repo := &fakeUserRepo{user: &auth.User{ID: "user-9", InstallationID: "inst-1"}}
	svc := makeServiceWithUsers(t, repo)

	ctx := svc.ResolveAndStashUser(context.Background(), "inst-1", "", "2c2aace8-82e9-4cb1-8d1f-2f822da43177")

	assert.Empty(t, repo.upserts, "email-empty input must NOT call UpsertByEmail")
	require.Len(t, repo.accountUpserts, 1)
	assert.Equal(t, "inst-1", repo.accountUpserts[0].InstallationID)
	assert.Equal(t, "2c2aace8-82e9-4cb1-8d1f-2f822da43177", repo.accountUpserts[0].ClaudeAccountUUID)
	assert.Equal(t, "user-9", auth.UserIDFrom(ctx))
}

func TestResolveAndStashUser_EmailPathBeatsAccountUUIDPath(t *testing.T) {
	// When both signals are present, email is the canonical key and the
	// account_uuid rides along as enrichment on the email-keyed row.
	// Using UpsertByAccountUUID here would create a duplicate seat.
	repo := &fakeUserRepo{user: &auth.User{ID: "user-3"}}
	svc := makeServiceWithUsers(t, repo)

	svc.ResolveAndStashUser(context.Background(), "inst-1", "alice@example.com", "2c2aace8-82e9-4cb1-8d1f-2f822da43177")

	require.Len(t, repo.upserts, 1)
	assert.Empty(t, repo.accountUpserts, "email-present input must NOT call UpsertByAccountUUID")
	require.NotNil(t, repo.upserts[0].ClaudeAccountUUID)
	assert.Equal(t, "2c2aace8-82e9-4cb1-8d1f-2f822da43177", *repo.upserts[0].ClaudeAccountUUID)
}

func TestResolveAndStashUser_NoInstallationIsNoOp(t *testing.T) {
	repo := &fakeUserRepo{}
	svc := makeServiceWithUsers(t, repo)

	ctx := svc.ResolveAndStashUser(context.Background(), "", "alice@example.com", "")

	assert.Empty(t, repo.upserts)
	assert.Equal(t, "", auth.UserIDFrom(ctx))
}

func TestResolveAndStashUser_OmitsClaudeAccountWhenEmpty(t *testing.T) {
	repo := &fakeUserRepo{user: &auth.User{ID: "user-1"}}
	svc := makeServiceWithUsers(t, repo)

	svc.ResolveAndStashUser(context.Background(), "inst-1", "alice@example.com", "")

	require.Len(t, repo.upserts, 1)
	assert.Nil(t, repo.upserts[0].ClaudeAccountUUID)
}

func TestResolveAndStashUser_RepoErrorDoesNotPropagate(t *testing.T) {
	repo := &fakeUserRepo{err: errors.New("db down")}
	svc := makeServiceWithUsers(t, repo)

	// Must return the original ctx unchanged so the request still proceeds.
	ctx := svc.ResolveAndStashUser(context.Background(), "inst-1", "alice@example.com", "")

	assert.Equal(t, "", auth.UserIDFrom(ctx))
}

func TestResolveAndStashUser_NilUsersIsNoOp(t *testing.T) {
	svc := makeServiceWithUsers(t, nil)

	ctx := svc.ResolveAndStashUser(context.Background(), "inst-1", "alice@example.com", "")

	assert.Equal(t, "", auth.UserIDFrom(ctx))
}

func TestResolveAndStashUser_CacheHitSkipsRepo(t *testing.T) {
	repo := &fakeUserRepo{user: &auth.User{ID: "user-1"}}
	cache := auth.NewLRUUserCache(8, 5*time.Minute)
	svc := auth.NewService(
		&fakeInstallationRepository{},
		&fakeAPIKeyRepository{byHash: map[string]fakeKeyRow{}},
		nil,
		repo,
		auth.NoOpAPIKeyCache{},
		cache,
		frozenClock(),
	)

	// First call hits repo and populates cache.
	ctx1 := svc.ResolveAndStashUser(context.Background(), "inst-1", "alice@example.com", "")
	require.Equal(t, "user-1", auth.UserIDFrom(ctx1))
	require.Len(t, repo.upserts, 1)

	// Second call must hit cache and skip the upsert entirely.
	ctx2 := svc.ResolveAndStashUser(context.Background(), "inst-1", "alice@example.com", "")
	assert.Equal(t, "user-1", auth.UserIDFrom(ctx2))
	assert.Len(t, repo.upserts, 1, "cache hit must not call repo.Upsert again")
}

func TestLRUUserCache_KeysIncludeInstallation(t *testing.T) {
	cache := auth.NewLRUUserCache(8, time.Minute)
	cache.Set("inst-A", "alice@example.com", "user-1")
	cache.Set("inst-B", "alice@example.com", "user-2")

	got, ok := cache.Get("inst-A", "alice@example.com")
	require.True(t, ok)
	assert.Equal(t, "user-1", got)

	got, ok = cache.Get("inst-B", "alice@example.com")
	require.True(t, ok)
	assert.Equal(t, "user-2", got)

	_, ok = cache.Get("inst-C", "alice@example.com")
	assert.False(t, ok, "unrelated installation must miss")
}
