package proxy_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"workweave/router/internal/auth"
	"workweave/router/internal/proxy"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureUserRepo is the minimal auth.UserRepository needed to verify that
// ResolveUserFromContext reaches Service.ResolveAndStashUser. Only the two
// Upsert methods are exercised by the request path; Get/List are unused.
type captureUserRepo struct {
	emailUpserts   []auth.UpsertUserParams
	accountUpserts []auth.UpsertUserByAccountUUIDParams
}

func (r *captureUserRepo) UpsertByEmail(ctx context.Context, p auth.UpsertUserParams) (*auth.User, error) {
	r.emailUpserts = append(r.emailUpserts, p)
	return &auth.User{ID: "user-from-email"}, nil
}
func (r *captureUserRepo) UpsertByAccountUUID(ctx context.Context, p auth.UpsertUserByAccountUUIDParams) (*auth.User, error) {
	r.accountUpserts = append(r.accountUpserts, p)
	return &auth.User{ID: "user-from-account"}, nil
}
func (r *captureUserRepo) Get(ctx context.Context, id string) (*auth.User, error) {
	return nil, errors.New("not used")
}
func (r *captureUserRepo) ListForInstallation(ctx context.Context, _ string) ([]*auth.User, error) {
	return nil, errors.New("not used")
}

// newTestAuthSvc wires a Service with only what ResolveAndStashUser touches
// (users repo + the no-op user cache). The other interface params aren't
// exercised by the resolver path so nil is safe.
func newTestAuthSvc(users auth.UserRepository) *auth.Service {
	return auth.NewService(nil, nil, nil, users, nil, auth.NoOpUserCache{}, nil)
}

func TestParseClaudeCodeMetadata_PullsEmail(t *testing.T) {
	raw := `{"device_id":"dev-1","account_uuid":"acct-1","session_id":"sess-1","email":"User@Example.com"}`
	got := proxy.ParseClaudeCodeMetadata(raw)

	assert.Equal(t, "dev-1", got.DeviceID)
	assert.Equal(t, "acct-1", got.AccountID)
	assert.Equal(t, "sess-1", got.SessionID)
	// Email is preserved verbatim; normalization is the caller's job
	// so the parser stays a pure data transform.
	assert.Equal(t, "User@Example.com", got.Email)
}

func TestParseClaudeCodeMetadata_EmptyAndMalformed(t *testing.T) {
	assert.Equal(t, proxy.ClaudeCodeMetadata{}, proxy.ParseClaudeCodeMetadata(""))
	assert.Equal(t, proxy.ClaudeCodeMetadata{}, proxy.ParseClaudeCodeMetadata("not-json"))
}

func TestNormalizeEmail_Valid(t *testing.T) {
	assert.Equal(t, "steven@workweave.ai", proxy.NormalizeEmail("  Steven@Workweave.AI  "))
	assert.Equal(t, "a@b.co", proxy.NormalizeEmail("a@b.co"))
}

func TestNormalizeEmail_RejectsEmptyAndWhitespace(t *testing.T) {
	assert.Equal(t, "", proxy.NormalizeEmail("   "))
	assert.Equal(t, "", proxy.NormalizeEmail(""))
}

func TestNormalizeEmail_RejectsBadShape(t *testing.T) {
	// No @ — would otherwise let a caller flood the user table with arbitrary
	// strings under the guise of "email".
	assert.Equal(t, "", proxy.NormalizeEmail("not-an-email"))
	// Empty local-part.
	assert.Equal(t, "", proxy.NormalizeEmail("@example.com"))
	// Empty domain.
	assert.Equal(t, "", proxy.NormalizeEmail("alice@"))
	// Multiple @s.
	assert.Equal(t, "", proxy.NormalizeEmail("a@b@c"))
}

func TestNormalizeEmail_RejectsOverLength(t *testing.T) {
	// Right at the cap — accepted.
	local := strings.Repeat("a", proxy.MaxEmailLen-len("@x.co"))
	atCap := local + "@x.co"
	require.Len(t, atCap, proxy.MaxEmailLen)
	assert.Equal(t, atCap, proxy.NormalizeEmail(atCap))

	// One byte over the cap — rejected. The cap bounds row growth from
	// caller-controlled inputs; without it any authenticated key could flood
	// router.model_router_users with unbounded distinct strings.
	overCap := strings.Repeat("a", proxy.MaxEmailLen-len("@x.co")+1) + "@x.co"
	require.Greater(t, len(overCap), proxy.MaxEmailLen)
	assert.Equal(t, "", proxy.NormalizeEmail(overCap))
}

func TestNormalizeClientIdentifier_PassesThroughShortValues(t *testing.T) {
	// Typical Claude Code device_id / session_id shapes (UUIDs and
	// short tokens) flow through unchanged. The cap is a flood-
	// protection floor, not a format check.
	assert.Equal(t, "dev-abc123", proxy.NormalizeClientIdentifier("dev-abc123"))
	assert.Equal(t, "550e8400-e29b-41d4-a716-446655440000",
		proxy.NormalizeClientIdentifier("550e8400-e29b-41d4-a716-446655440000"))
	// Empty input flows through as empty — no signal stays no signal.
	assert.Equal(t, "", proxy.NormalizeClientIdentifier(""))
}

func TestNormalizeClientIdentifier_RejectsOverLength(t *testing.T) {
	// Right at the cap — accepted.
	atCap := strings.Repeat("a", proxy.MaxClientIdentifierLen)
	require.Len(t, atCap, proxy.MaxClientIdentifierLen)
	assert.Equal(t, atCap, proxy.NormalizeClientIdentifier(atCap))

	// One byte over the cap — rejected (empty string). Without this an
	// authenticated caller could pad device_id/session_id to arbitrary
	// length and inflate router.model_router_request_telemetry storage
	// per request.
	overCap := strings.Repeat("a", proxy.MaxClientIdentifierLen+1)
	require.Greater(t, len(overCap), proxy.MaxClientIdentifierLen)
	assert.Equal(t, "", proxy.NormalizeClientIdentifier(overCap))
}

func TestResolveUserFromContext_AccountUUIDOnlyReachesUpsert(t *testing.T) {
	// Regression: an earlier version of this guard returned early on
	// id.Email == "", which made the new account_uuid-only upsert path
	// (added for Claude CLI v2.1.x, which packs only account_uuid in
	// metadata.user_id) completely unreachable from any inbound handler.
	repo := &captureUserRepo{}
	svc := newTestAuthSvc(repo)
	inst := &auth.Installation{ID: "inst-1"}

	ctx := context.WithValue(context.Background(), proxy.ClientIdentityContextKey{}, proxy.ClientIdentity{
		AccountID: "2c2aace8-82e9-4cb1-8d1f-2f822da43177",
	})
	ctx = proxy.ResolveUserFromContext(ctx, svc, inst)

	assert.Empty(t, repo.emailUpserts, "no email signal: must not hit the email-keyed upsert")
	require.Len(t, repo.accountUpserts, 1, "account_uuid alone must still reach UpsertByAccountUUID")
	assert.Equal(t, "2c2aace8-82e9-4cb1-8d1f-2f822da43177", repo.accountUpserts[0].ClaudeAccountUUID)
	assert.Equal(t, "user-from-account", auth.UserIDFrom(ctx))
}

func TestResolveUserFromContext_EmailOnlyReachesEmailUpsert(t *testing.T) {
	repo := &captureUserRepo{}
	svc := newTestAuthSvc(repo)
	inst := &auth.Installation{ID: "inst-1"}

	ctx := context.WithValue(context.Background(), proxy.ClientIdentityContextKey{}, proxy.ClientIdentity{
		Email: "alice@example.com",
	})
	ctx = proxy.ResolveUserFromContext(ctx, svc, inst)

	require.Len(t, repo.emailUpserts, 1)
	assert.Empty(t, repo.accountUpserts)
	assert.Equal(t, "alice@example.com", repo.emailUpserts[0].Email)
	assert.Equal(t, "user-from-email", auth.UserIDFrom(ctx))
}

func TestResolveUserFromContext_BothMissingIsNoOp(t *testing.T) {
	repo := &captureUserRepo{}
	svc := newTestAuthSvc(repo)
	inst := &auth.Installation{ID: "inst-1"}

	// ClientIdentity stashed but both Email and AccountID empty: nothing
	// to attribute, so neither upsert path should fire and the ctx must
	// flow through with no UserID set.
	ctx := context.WithValue(context.Background(), proxy.ClientIdentityContextKey{}, proxy.ClientIdentity{})
	ctx = proxy.ResolveUserFromContext(ctx, svc, inst)

	assert.Empty(t, repo.emailUpserts)
	assert.Empty(t, repo.accountUpserts)
	assert.Equal(t, "", auth.UserIDFrom(ctx))
}
