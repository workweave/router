package proxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/providers"
	"workweave/router/internal/proxy/usage"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const exhaustedSubToken = "sk-ant-oat01-subscription-token"

// observerWithSnapshot builds an observer pre-seeded with one snapshot for the
// given token, using a fixed clock so the reading stays fresh for the test.
func observerWithSnapshot(token string, snap usage.Snapshot) *usage.Observer {
	now := time.Unix(1_000_000, 0)
	o := usage.NewObserver([]byte("salt"), 10*time.Minute, func() time.Time { return now })
	o.Record(o.Key([]byte(token)), snap)
	return o
}

func exhaustedSnapshot() usage.Snapshot {
	return usage.Snapshot{Secondary: usage.Window{UsedPercent: 1.0, WindowMinutes: 10080}}
}

func TestResolveAndInjectCredentials_SuppressedSubscriptionFallsThroughToBYOK(t *testing.T) {
	// With the subscription suppressed (its plan window is spent), a present
	// inbound subscription bearer must be skipped so resolution falls through to
	// the BYOK Anthropic key — the turn then serves on that key, not the dead
	// token, and bills at full cost (servedOnSubscription reads non-OAuth).
	ctx := context.WithValue(context.Background(), ExternalAPIKeysContextKey{}, []*auth.ExternalAPIKey{
		{Provider: providers.ProviderAnthropic, Plaintext: []byte("sk-ant-api-byok")},
	})
	ctx = withSuppressedClaudeSubscription(ctx)
	headers := http.Header{"Authorization": []string{"Bearer " + exhaustedSubToken}}

	out := resolveAndInjectCredentials(ctx, providers.ProviderAnthropic, headers)
	creds := CredentialsFromContext(out)
	require.NotNil(t, creds)
	assert.False(t, creds.OAuth, "the spent subscription token must be skipped")
	assert.Equal(t, []byte("sk-ant-api-byok"), creds.APIKey,
		"resolution must fall through to the BYOK Anthropic key")
	assert.False(t, servedOnSubscription(out),
		"a suppressed-subscription turn must bill at full cost, not the subscription rate")
}

func TestResolveAndInjectCredentials_SuppressedSubscriptionFallsThroughToDeployment(t *testing.T) {
	// No BYOK, dedicated subscription header present, subscription suppressed:
	// resolution resolves to NO credential, so the Anthropic provider client uses
	// its own deployment key (the Weave key).
	ctx := context.WithValue(context.Background(), AnthropicSubscriptionContextKey{}, exhaustedSubToken)
	ctx = withSuppressedClaudeSubscription(ctx)

	out := resolveAndInjectCredentials(ctx, providers.ProviderAnthropic, http.Header{})
	assert.Nil(t, CredentialsFromContext(out),
		"with the subscription suppressed and no BYOK, no credential is set so the deployment key serves the turn")
}

func TestResolveAndInjectCredentials_SuppressedInboundBearerNotReResolved(t *testing.T) {
	// Self-hosted (no router key, no BYOK): the spent subscription arrives as an
	// inbound Authorization bearer. With suppression on, neither the
	// subscription-first block NOR the later ExtractClientCredentials fallback may
	// re-resolve it — resolution must end with no credential so the deployment
	// Anthropic key serves the turn.
	ctx := withSuppressedClaudeSubscription(context.Background())
	headers := http.Header{"Authorization": []string{"Bearer " + exhaustedSubToken}}

	out := resolveAndInjectCredentials(ctx, providers.ProviderAnthropic, headers)
	assert.Nil(t, CredentialsFromContext(out),
		"a suppressed inbound subscription bearer must not be re-resolved via client extraction")
}

func TestResolveAndInjectCredentials_SuppressedKeepsRealClientApiKey(t *testing.T) {
	// Suppression targets the spent OAuth subscription only. A self-hosted caller
	// supplying a real Anthropic API key (sk-ant-api-, non-OAuth) must still have
	// it resolved — it is a valid non-subscription credential, not the dead token.
	ctx := withSuppressedClaudeSubscription(context.Background())
	headers := http.Header{"X-Api-Key": []string{"sk-ant-api-real-client-key"}}

	out := resolveAndInjectCredentials(ctx, providers.ProviderAnthropic, headers)
	creds := CredentialsFromContext(out)
	require.NotNil(t, creds, "a real client API key is not the suppressed subscription and must be kept")
	assert.False(t, creds.OAuth)
	assert.Equal(t, []byte("sk-ant-api-real-client-key"), creds.APIKey)
}

func TestResolveAndInjectCredentials_ClaudeSuppressionLeavesCodexIntact(t *testing.T) {
	// A caller with BOTH a healthy Codex subscription and an exhausted Claude
	// subscription, routed to an OpenAI model: the Claude-scoped suppression must
	// NOT touch the Codex token — the OpenAI turn still bills the customer's Codex
	// plan. (Regression: the flag previously suppressed both families.)
	ctx := context.WithValue(context.Background(), OpenAISubscriptionContextKey{}, codexTestJWT)
	ctx = context.WithValue(ctx, OpenAIAccountIDContextKey{}, "acct-1")
	ctx = withSuppressedClaudeSubscription(ctx)

	out := resolveAndInjectCredentials(ctx, providers.ProviderOpenAI, http.Header{})
	creds := CredentialsFromContext(out)
	require.NotNil(t, creds)
	assert.True(t, creds.OAuth, "the Codex subscription must survive Claude-only suppression")
	assert.Equal(t, credSourceCodexSubscription, creds.Source)
}

func TestResolveAndInjectCredentials_UnsuppressedSubscriptionStillWins(t *testing.T) {
	// Control: without suppression, the subscription token still wins (the steady
	// state for a plan with headroom) — the suppression carve-out must not leak
	// into the normal path.
	ctx := context.WithValue(context.Background(), AnthropicSubscriptionContextKey{}, exhaustedSubToken)

	out := resolveAndInjectCredentials(ctx, providers.ProviderAnthropic, http.Header{})
	creds := CredentialsFromContext(out)
	require.NotNil(t, creds)
	assert.True(t, creds.OAuth, "a non-suppressed subscription must still be resolved")
	assert.Equal(t, credSourceSubscription, creds.Source)
}

func TestClaudeSubscriptionExhausted(t *testing.T) {
	headers := func() http.Header {
		return http.Header{"Authorization": []string{"Bearer " + exhaustedSubToken}}
	}
	deploymentKeyed := map[string]struct{}{providers.ProviderAnthropic: {}}

	t.Run("exhausted snapshot + deployment key", func(t *testing.T) {
		s := &Service{
			usageObserver:            observerWithSnapshot(exhaustedSubToken, exhaustedSnapshot()),
			deploymentKeyedProviders: deploymentKeyed,
		}
		assert.True(t, s.claudeSubscriptionExhausted(context.Background(), headers()))
	})

	t.Run("exhausted but no fallback key — keep using the subscription", func(t *testing.T) {
		// Without a deployment / BYOK Anthropic key there is nothing to fall
		// through to; dropping the token would 400 instead of 429, which is worse.
		s := &Service{
			usageObserver:            observerWithSnapshot(exhaustedSubToken, exhaustedSnapshot()),
			deploymentKeyedProviders: map[string]struct{}{},
		}
		assert.False(t, s.claudeSubscriptionExhausted(context.Background(), headers()))
	})

	t.Run("subscription has headroom", func(t *testing.T) {
		s := &Service{
			usageObserver: observerWithSnapshot(exhaustedSubToken, usage.Snapshot{
				Secondary: usage.Window{UsedPercent: 0.80, WindowMinutes: 10080},
			}),
			deploymentKeyedProviders: deploymentKeyed,
		}
		assert.False(t, s.claudeSubscriptionExhausted(context.Background(), headers()))
	})

	t.Run("no subscription token present", func(t *testing.T) {
		s := &Service{
			usageObserver:            observerWithSnapshot(exhaustedSubToken, exhaustedSnapshot()),
			deploymentKeyedProviders: deploymentKeyed,
		}
		assert.False(t, s.claudeSubscriptionExhausted(context.Background(), http.Header{}))
	})

	t.Run("observer not wired", func(t *testing.T) {
		s := &Service{deploymentKeyedProviders: deploymentKeyed}
		assert.False(t, s.claudeSubscriptionExhausted(context.Background(), headers()))
	})

	t.Run("token present but never observed", func(t *testing.T) {
		now := time.Unix(1_000_000, 0)
		s := &Service{
			usageObserver:            usage.NewObserver([]byte("salt"), time.Minute, func() time.Time { return now }),
			deploymentKeyedProviders: deploymentKeyed,
		}
		assert.False(t, s.claudeSubscriptionExhausted(context.Background(), headers()),
			"a never-observed credential is cold-start slack, not exhausted")
	})
}

func TestAnthropicFallbackKeyAvailable(t *testing.T) {
	t.Run("deployment key present", func(t *testing.T) {
		s := &Service{deploymentKeyedProviders: map[string]struct{}{providers.ProviderAnthropic: {}}}
		assert.True(t, s.anthropicFallbackKeyAvailable(context.Background()))
	})
	t.Run("BYOK present", func(t *testing.T) {
		s := &Service{}
		ctx := context.WithValue(context.Background(), ExternalAPIKeysContextKey{}, []*auth.ExternalAPIKey{
			{Provider: providers.ProviderAnthropic, Plaintext: []byte("sk-ant-api-byok")},
		})
		assert.True(t, s.anthropicFallbackKeyAvailable(ctx))
	})
	t.Run("neither", func(t *testing.T) {
		s := &Service{deploymentKeyedProviders: map[string]struct{}{providers.ProviderOpenAI: {}}}
		assert.False(t, s.anthropicFallbackKeyAvailable(context.Background()))
	})
	t.Run("nil deploymentKeyedProviders, no BYOK", func(t *testing.T) {
		s := &Service{}
		assert.False(t, s.anthropicFallbackKeyAvailable(context.Background()))
	})
}
