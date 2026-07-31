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

// TestBaselineFailoverCtxSuppression tests the race condition where
// claudeSubscriptionExhausted fires at the baseline check (line 2097) but
// NOT at the pre-dispatch check (line 1794). In the baseline failover scenario,
// the primary dispatch is to an OSS provider (OpenRouter, Fireworks, etc.), so
// line 1797 does NOT stamp an Anthropic OAuth credential into ctx. When line
// 2148 re-resolves ctx with finalProvider=ProviderAnthropic, it sees the
// inbound OAuth bearer and must decide whether to inject it. Without the fix
// (line 2098 absent), no suppression is on ctx and the bearer is injected —
// servedOnSubscription returns true and billing is miscounted. With the fix,
// ctx is suppressed before line 2148 runs and the bearer is dropped.
func TestBaselineFailoverCtxSuppression(t *testing.T) {
	// headers carries the subscription OAuth bearer exactly as Claude Code sends it.
	// ctx starts with no Anthropic credential: the primary dispatch was to an OSS
	// provider, so line 1797 resolved OSS credentials and left no Anthropic OAuth
	// in ctx. This is the state of ctx arriving at line 2148 in the race scenario.
	headers := http.Header{"Authorization": []string{"Bearer " + exhaustedSubToken}}
	base := context.Background()

	// fails_without_fix: simulates line 2148 when ctx has no suppression marker.
	// Exhaustion was not detected at line 1794 (subscription was healthy), so
	// withSuppressedClaudeSubscription was not applied. Line 2097 detects
	// exhaustion for the baseline, but line 2098 (the fix) is absent, so ctx
	// carries no suppression into line 2148. resolveAndInjectCredentials sees
	// the inbound OAuth bearer and injects it — billing misfires.
	t.Run("fails_without_fix", func(t *testing.T) {
		// Simulate line 2148 with no suppression on ctx (line 2098 absent).
		out := resolveAndInjectCredentials(base, providers.ProviderAnthropic, headers)
		assert.True(t, servedOnSubscription(out),
			"BUG: an unsuppressed ctx at line 2148 lets the OAuth bearer in; "+
				"servedOnSubscription returns true and billing is charged to the subscription "+
				"even though the Weave deployment key actually served the turn")
	})

	// passes_with_fix: simulates line 2148 after line 2098 applied suppression.
	// claudeSubscriptionExhausted fires at line 2097, and line 2098 applies
	// withSuppressedClaudeSubscription to ctx. Both guards in
	// resolveAndInjectCredentials fire: the line-2694 OAuth block is skipped
	// (suppressClaudeSub=true), and the line-2759 inbound-bearer nil-out drops
	// the OAuth bearer from ExtractClientCredentials. No credential is set and
	// ctx is returned unchanged with no Anthropic OAuth — servedOnSubscription
	// returns false and billing correctly charges at full cost.
	t.Run("passes_with_fix", func(t *testing.T) {
		// Simulate line 2098: withSuppressedClaudeSubscription applied to ctx
		// when claudeSubscriptionExhausted fires at the baseline check.
		suppressed := withSuppressedClaudeSubscription(base)
		// Simulate line 2148: re-resolve ctx for finalProvider=ProviderAnthropic.
		out := resolveAndInjectCredentials(suppressed, providers.ProviderAnthropic, headers)
		assert.False(t, servedOnSubscription(out),
			"with suppression on ctx before line 2148, the OAuth bearer is dropped "+
				"and servedOnSubscription correctly returns false")
		creds := CredentialsFromContext(out)
		assert.True(t, creds == nil || !creds.OAuth,
			"no Anthropic OAuth credential must be present: deployment key serves the turn")
	})
}
