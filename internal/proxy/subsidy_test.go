package proxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/providers"
	"workweave/router/internal/proxy/usage"
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

// End-to-end: the key withUsageObserver records under must equal the key
// subsidyFactors reads, or the discount never materializes. Drives the real
// observer closure (as a provider would) with a resolved Codex credential and an
// upstream rate-limit response, then asserts subsidyFactors returns the discount.
func TestSubsidy_RecordReadKeyAgreement(t *testing.T) {
	s := (&Service{}).WithSubscriptionAwareRouting(
		usage.NewObserver([]byte("salt"), 10*time.Minute, time.Now), 0.05, 2.0)

	const jwt = "eyJhbGciOi.codex.jwt"
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+jwt)
	headers.Set("ChatGPT-Account-ID", "acct-1")

	ctx := context.Background()
	obsCtx := s.withUsageObserver(ctx, headers)

	// Simulate the resolved Codex credential + an upstream response at 10% used,
	// invoked the way a provider does after the upstream call.
	cred := &Credentials{APIKey: []byte(jwt), AccountID: []byte("acct-1"), Source: credSourceCodexSubscription, OAuth: true}
	callCtx := context.WithValue(obsCtx, CredentialsContextKey{}, cred)
	resp := http.Header{}
	resp.Set("x-codex-primary-used-percent", "10")
	resp.Set("x-codex-primary-window-minutes", "300")
	providers.ObserveUpstreamHeaders(callCtx, resp)

	// subsidyFactors must read back the SAME key and discount covered GPT models.
	factors := s.subsidyFactors(ctx, headers)
	require.NotNil(t, factors, "headroom was observed; factors must be non-nil")
	f, ok := factors["gpt-5.5"]
	require.True(t, ok, "covered GPT model must be subsidized")
	assert.Less(t, f, 1.0, "10%% used → discounted below full price")
	assert.GreaterOrEqual(t, f, 0.05, "never below epsilon")
}
