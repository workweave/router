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

// drive429 replays the way the Anthropic adapter reports a subscription-served
// 429 to the usage observer, returning the ctx-wired service ready for a
// claudeSubscriptionExhausted read on the NEXT turn.
func drive429(t *testing.T, s *Service, subToken string, retryAfter string) {
	t.Helper()
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+subToken)
	obsCtx := s.withUsageObserver(context.Background(), headers)
	cred := &Credentials{APIKey: []byte(subToken), Source: credSourceSubscription, OAuth: true}
	callCtx := context.WithValue(obsCtx, CredentialsContextKey{}, cred)
	resp := http.Header{}
	if retryAfter != "" {
		resp.Set("Retry-After", retryAfter)
	}
	providers.ObserveUpstreamHeaders(callCtx, http.StatusTooManyRequests, resp)
}

// The core fix: a Claude OAuth session-limit 429 carries no unified-utilization
// headers, so header-only detection never fired and the spent token kept getting
// re-injected. Treating the 429 itself as exhaustion must flip
// claudeSubscriptionExhausted on the next turn so the failover engages.
func TestClaude429_RecordsExhaustion_EngagesFailover(t *testing.T) {
	obs := usage.NewObserver([]byte("salt"), 10*time.Minute, time.Now)
	s := (&Service{}).WithSubscriptionAwareRouting(obs, 0.05, 2.0)
	// A deployment Anthropic key exists to serve the turn after suppression.
	s.deploymentKeyedProviders = map[string]struct{}{providers.ProviderAnthropic: {}}

	const tok = "sk-ant-oat01-session-capped"
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+tok)

	require.False(t, s.claudeSubscriptionExhausted(context.Background(), headers),
		"a fresh subscription is not exhausted before any 429")

	drive429(t, s, tok, "3600")

	assert.True(t, s.claudeSubscriptionExhausted(context.Background(), headers),
		"after a subscription 429 the plan is known-spent and the failover must engage")
}

// Without a fallback Anthropic key (managed BYOK-only, no ANTHROPIC_API_KEY),
// dropping the subscription would 400 on no credential — strictly worse than the
// 429 — so exhaustion is recorded but the failover stays disengaged and the
// caller keeps the subscription.
func TestClaude429_NoFallbackKey_KeepsSubscription(t *testing.T) {
	obs := usage.NewObserver([]byte("salt"), 10*time.Minute, time.Now)
	s := (&Service{}).WithSubscriptionAwareRouting(obs, 0.05, 2.0)
	// deploymentKeyedProviders intentionally empty; no BYOK on the request.

	const tok = "sk-ant-oat01-session-capped"
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+tok)

	drive429(t, s, tok, "3600")

	assert.False(t, s.claudeSubscriptionExhausted(context.Background(), headers),
		"no fallback key: keep the subscription rather than 400 on no credential")
}

// A successful (non-429) turn must still take the header-utilization path, not
// be misread as exhaustion — otherwise every turn would suppress the sub.
func TestClaudeSuccess_DoesNotRecordExhaustion(t *testing.T) {
	obs := usage.NewObserver([]byte("salt"), 10*time.Minute, time.Now)
	s := (&Service{}).WithSubscriptionAwareRouting(obs, 0.05, 2.0)
	s.deploymentKeyedProviders = map[string]struct{}{providers.ProviderAnthropic: {}}

	const tok = "sk-ant-oat01-healthy"
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+tok)

	obsCtx := s.withUsageObserver(context.Background(), headers)
	cred := &Credentials{APIKey: []byte(tok), Source: credSourceSubscription, OAuth: true}
	callCtx := context.WithValue(obsCtx, CredentialsContextKey{}, cred)
	resp := http.Header{}
	resp.Set("anthropic-ratelimit-unified-5h-utilization", "20")
	providers.ObserveUpstreamHeaders(callCtx, http.StatusOK, resp)

	assert.False(t, s.claudeSubscriptionExhausted(context.Background(), headers),
		"a 200 at 20%% used must not be treated as exhaustion")
}
