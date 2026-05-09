package proxy_test

import (
	"strings"
	"testing"

	"workweave/router/internal/proxy"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
