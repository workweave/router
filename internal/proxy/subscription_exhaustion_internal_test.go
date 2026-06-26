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
	ctx = withSuppressedSubscription(ctx)
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
	ctx = withSuppressedSubscription(ctx)

	out := resolveAndInjectCredentials(ctx, providers.ProviderAnthropic, http.Header{})
	assert.Nil(t, CredentialsFromContext(out),
		"with the subscription suppressed and no BYOK, no credential is set so the deployment key serves the turn")
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
