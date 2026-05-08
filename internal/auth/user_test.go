package auth_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"workweave/router/internal/auth"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeUserRepo struct {
	mu      sync.Mutex
	upserts []auth.UpsertUserParams
	user    *auth.User
	err     error
}

func (f *fakeUserRepo) Upsert(ctx context.Context, params auth.UpsertUserParams) (*auth.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts = append(f.upserts, params)
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
		fakeInstallationRepository{},
		&fakeAPIKeyRepository{byHash: map[string]fakeKeyRow{}},
		nil,
		users,
		auth.NoOpAPIKeyCache{},
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

func TestResolveAndStashUser_NoEmailIsNoOp(t *testing.T) {
	repo := &fakeUserRepo{}
	svc := makeServiceWithUsers(t, repo)

	ctx := svc.ResolveAndStashUser(context.Background(), "inst-1", "", "")

	assert.Empty(t, repo.upserts)
	assert.Equal(t, "", auth.UserIDFrom(ctx))
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
