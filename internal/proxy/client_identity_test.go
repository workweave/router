package proxy_test

import (
	"testing"

	"workweave/router/internal/proxy"

	"github.com/stretchr/testify/assert"
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

func TestNormalizeEmail(t *testing.T) {
	assert.Equal(t, "steven@workweave.ai", proxy.NormalizeEmail("  Steven@Workweave.AI  "))
	assert.Equal(t, "", proxy.NormalizeEmail("   "))
	assert.Equal(t, "", proxy.NormalizeEmail(""))
}
