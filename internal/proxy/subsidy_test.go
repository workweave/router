package proxy

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The subsidy must recognize a subscription presented via the inbound
// Authorization bearer (Claude Code's sk-ant-oat…, Codex CLI's JWT+account-id on
// their native harnesses), not only via the dedicated X-Weave-*-Subscription
// headers (opencode). Otherwise the discount would be opencode-only.
func TestPresentSubscriptionTokens_InboundBearerHarnesses(t *testing.T) {
	s := &Service{}

	t.Run("claude code: sk-ant-oat in Authorization", func(t *testing.T) {
		h := http.Header{}
		h.Set("Authorization", "Bearer sk-ant-oat01-claudecode-token")
		codex, anthro := s.presentSubscriptionTokens(context.Background(), h)
		assert.Equal(t, "sk-ant-oat01-claudecode-token", anthro)
		assert.Empty(t, codex, "an sk-ant token must not be misread as a Codex sub")
	})

	t.Run("codex cli: JWT + ChatGPT-Account-ID in Authorization", func(t *testing.T) {
		h := http.Header{}
		h.Set("Authorization", "Bearer eyJhbGciOi.codex.jwt")
		h.Set("ChatGPT-Account-ID", "acct-abc-123")
		codex, anthro := s.presentSubscriptionTokens(context.Background(), h)
		assert.Equal(t, "eyJhbGciOi.codex.jwt", codex)
		assert.Empty(t, anthro, "a Codex JWT must not be misread as a Claude sub")
	})

	t.Run("codex jwt without account-id is not a usable codex sub", func(t *testing.T) {
		h := http.Header{}
		h.Set("Authorization", "Bearer eyJhbGciOi.codex.jwt")
		codex, _ := s.presentSubscriptionTokens(context.Background(), h)
		assert.Empty(t, codex, "the Codex backend needs the account-id; no pairing = no sub")
	})

	t.Run("no subscription headers: both empty", func(t *testing.T) {
		codex, anthro := s.presentSubscriptionTokens(context.Background(), http.Header{})
		assert.Empty(t, codex)
		assert.Empty(t, anthro)
	})
}
