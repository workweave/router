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

// captureUserRepo is the minimal auth.UserRepository for these tests; only
// the Upsert methods are exercised.
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

// newTestAuthSvc wires a Service with only what ResolveAndStashUser touches.
func newTestAuthSvc(users auth.UserRepository) *auth.Service {
	return auth.NewService(nil, nil, nil, users, nil, auth.NoOpUserCache{}, nil)
}

func TestParseClaudeCodeMetadata_PullsEmail(t *testing.T) {
	raw := `{"device_id":"dev-1","account_uuid":"acct-1","session_id":"sess-1","email":"User@Example.com"}`
	got := proxy.ParseClaudeCodeMetadata(raw)

	assert.Equal(t, "dev-1", got.DeviceID)
	assert.Equal(t, "acct-1", got.AccountID)
	assert.Equal(t, "sess-1", got.SessionID)
	// Email preserved verbatim; normalization is the caller's job.
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
	assert.Equal(t, "", proxy.NormalizeEmail("not-an-email"))
	assert.Equal(t, "", proxy.NormalizeEmail("@example.com"))
	assert.Equal(t, "", proxy.NormalizeEmail("alice@"))
	assert.Equal(t, "", proxy.NormalizeEmail("a@b@c"))
}

func TestNormalizeEmail_RejectsOverLength(t *testing.T) {
	local := strings.Repeat("a", proxy.MaxEmailLen-len("@x.co"))
	atCap := local + "@x.co"
	require.Len(t, atCap, proxy.MaxEmailLen)
	assert.Equal(t, atCap, proxy.NormalizeEmail(atCap))

	overCap := strings.Repeat("a", proxy.MaxEmailLen-len("@x.co")+1) + "@x.co"
	require.Greater(t, len(overCap), proxy.MaxEmailLen)
	assert.Equal(t, "", proxy.NormalizeEmail(overCap))
}

func TestNormalizeDisplayName_TrimsAndPassesUnicode(t *testing.T) {
	assert.Equal(t, "Alice Liddell", proxy.NormalizeDisplayName("  Alice Liddell  "))
	assert.Equal(t, "Renée Fleming", proxy.NormalizeDisplayName("Renée Fleming"))
	assert.Equal(t, "", proxy.NormalizeDisplayName(""))
	assert.Equal(t, "", proxy.NormalizeDisplayName("   "))
}

func TestNormalizeDisplayName_StripsControlBytes(t *testing.T) {
	// CR/LF + control bytes must be dropped so the value can't break log
	// lines or smuggle extra HTTP headers. Surrounding visible chars stay.
	out := proxy.NormalizeDisplayName("Alice\r\nMallory")
	assert.NotContains(t, out, "\r")
	assert.NotContains(t, out, "\n")
	assert.Equal(t, "AliceMallory", out)
	assert.Equal(t, "Alice", proxy.NormalizeDisplayName("Alice\x00\x07"))
}

func TestNormalizeDisplayName_RejectsOverLength(t *testing.T) {
	atCap := strings.Repeat("a", proxy.MaxDisplayNameLen)
	require.Len(t, atCap, proxy.MaxDisplayNameLen)
	assert.Equal(t, atCap, proxy.NormalizeDisplayName(atCap))

	overCap := strings.Repeat("a", proxy.MaxDisplayNameLen+1)
	require.Greater(t, len(overCap), proxy.MaxDisplayNameLen)
	assert.Equal(t, "", proxy.NormalizeDisplayName(overCap))
}

func TestNormalizeClientIdentifier_PassesThroughShortValues(t *testing.T) {
	assert.Equal(t, "dev-abc123", proxy.NormalizeClientIdentifier("dev-abc123"))
	assert.Equal(t, "550e8400-e29b-41d4-a716-446655440000",
		proxy.NormalizeClientIdentifier("550e8400-e29b-41d4-a716-446655440000"))
	assert.Equal(t, "", proxy.NormalizeClientIdentifier(""))
}

func TestNormalizeClientIdentifier_RejectsOverLength(t *testing.T) {
	atCap := strings.Repeat("a", proxy.MaxClientIdentifierLen)
	require.Len(t, atCap, proxy.MaxClientIdentifierLen)
	assert.Equal(t, atCap, proxy.NormalizeClientIdentifier(atCap))

	overCap := strings.Repeat("a", proxy.MaxClientIdentifierLen+1)
	require.Greater(t, len(overCap), proxy.MaxClientIdentifierLen)
	assert.Equal(t, "", proxy.NormalizeClientIdentifier(overCap))
}

func TestResolveUserFromContext_AccountUUIDOnlyReachesUpsert(t *testing.T) {
	// Regression: an earlier guard returned early on id.Email == "", making
	// the account_uuid-only upsert path (Claude CLI v2.1.x) unreachable.
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

func TestNormalizeClientApp(t *testing.T) {
	cases := []struct {
		name      string
		xApp      string
		userAgent string
		want      string
	}{
		{"explicit header wins", "codex", "claude-cli/2.0.1", proxy.ClientAppCodex},
		{"explicit header lowercased", "Claude-Code", "", proxy.ClientAppClaudeCode},
		{"explicit header trimmed", "  cursor  ", "", proxy.ClientAppCursor},
		{"oversized header falls through to UA", strings.Repeat("a", proxy.MaxClientAppLen+1), "claude-cli/2.0.1", proxy.ClientAppClaudeCode},
		{"UA claude-cli", "", "claude-cli/2.0.1 (cli, win32)", proxy.ClientAppClaudeCode},
		{"UA codex_cli_rs", "", "codex_cli_rs/0.39.0 (darwin)", proxy.ClientAppCodex},
		{"UA cursor", "", "Cursor/0.42.1 (darwin x64)", proxy.ClientAppCursor},
		{"UA gemini-cli", "", "gemini-cli/1.0.0 (linux)", proxy.ClientAppGeminiCLI},
		{"UA unknown returns empty", "", "curl/8.4.0", ""},
		{"both empty returns empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := proxy.NormalizeClientApp(tc.xApp, tc.userAgent)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestResolveUserFromContext_BothMissingIsNoOp(t *testing.T) {
	repo := &captureUserRepo{}
	svc := newTestAuthSvc(repo)
	inst := &auth.Installation{ID: "inst-1"}

	ctx := context.WithValue(context.Background(), proxy.ClientIdentityContextKey{}, proxy.ClientIdentity{})
	ctx = proxy.ResolveUserFromContext(ctx, svc, inst)

	assert.Empty(t, repo.emailUpserts)
	assert.Empty(t, repo.accountUpserts)
	assert.Equal(t, "", auth.UserIDFrom(ctx))
}
