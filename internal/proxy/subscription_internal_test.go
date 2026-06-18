package proxy

import (
	"context"
	"net/http"
	"testing"

	"workweave/router/internal/auth"
	"workweave/router/internal/providers"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testInstallationID = "11111111-1111-1111-1111-111111111111"

func TestSubscriptionCredsFromHeaderValue(t *testing.T) {
	t.Run("accepts oat token", func(t *testing.T) {
		creds := subscriptionCredsFromHeaderValue("sk-ant-oat01-token")
		require.NotNil(t, creds)
		assert.True(t, creds.OAuth)
		assert.Equal(t, credSourceSubscription, creds.Source)
		assert.Equal(t, []byte("sk-ant-oat01-token"), creds.APIKey)
	})
	t.Run("trims whitespace", func(t *testing.T) {
		creds := subscriptionCredsFromHeaderValue("  sk-ant-oat01-token  ")
		require.NotNil(t, creds)
		assert.Equal(t, []byte("sk-ant-oat01-token"), creds.APIKey,
			"the dedicated header value must be canonicalized before use")
	})
	t.Run("rejects api key", func(t *testing.T) {
		assert.Nil(t, subscriptionCredsFromHeaderValue("sk-ant-api-real-key"),
			"a real API key is not a subscription token and must not be flagged OAuth")
	})
	t.Run("rejects router key", func(t *testing.T) {
		assert.Nil(t, subscriptionCredsFromHeaderValue("rk_router_key"))
	})
	t.Run("rejects empty", func(t *testing.T) {
		assert.Nil(t, subscriptionCredsFromHeaderValue(""))
	})
}

func TestResolveAndInjectCredentials_SubscriptionHeaderBeatsBYOK(t *testing.T) {
	// Router-keyed request (non-nil installation) carrying both a BYOK Anthropic
	// key and the dedicated subscription header. The subscription must win, and
	// it must be read past the router-key guard that normally skips inbound
	// header extraction.
	ctx := context.Background()
	ctx = context.WithValue(ctx, InstallationIDContextKey{}, testInstallationID)
	ctx = context.WithValue(ctx, ExternalAPIKeysContextKey{}, []*auth.ExternalAPIKey{
		{Provider: providers.ProviderAnthropic, Plaintext: []byte("sk-ant-api-byok")},
	})
	ctx = context.WithValue(ctx, AnthropicSubscriptionContextKey{}, "sk-ant-oat01-subscription-token")

	out := resolveAndInjectCredentials(ctx, providers.ProviderAnthropic, http.Header{})
	creds := CredentialsFromContext(out)
	require.NotNil(t, creds)
	assert.True(t, creds.OAuth)
	assert.Equal(t, credSourceSubscription, creds.Source)
	assert.Equal(t, []byte("sk-ant-oat01-subscription-token"), creds.APIKey)
}

func TestResolveAndInjectCredentials_SubscriptionHeaderIgnoredForNonAnthropic(t *testing.T) {
	// The subscription token can only pay for Claude models. A non-Anthropic
	// route must fall back to BYOK and never resolve the subscription token.
	ctx := context.Background()
	ctx = context.WithValue(ctx, InstallationIDContextKey{}, testInstallationID)
	ctx = context.WithValue(ctx, ExternalAPIKeysContextKey{}, []*auth.ExternalAPIKey{
		{Provider: providers.ProviderOpenAI, Plaintext: []byte("sk-oai-byok")},
	})
	ctx = context.WithValue(ctx, AnthropicSubscriptionContextKey{}, "sk-ant-oat01-subscription-token")

	out := resolveAndInjectCredentials(ctx, providers.ProviderOpenAI, http.Header{})
	creds := CredentialsFromContext(out)
	require.NotNil(t, creds)
	assert.False(t, creds.OAuth)
	assert.Equal(t, []byte("sk-oai-byok"), creds.APIKey,
		"a non-Anthropic route must use its own BYOK key, never the Anthropic subscription token")
}

func TestResolveAndInjectCredentials_InboundSubscriptionBeatsBYOK(t *testing.T) {
	// Self-hosted (no router key): an inbound Authorization subscription bearer
	// must beat a present BYOK key so the turn bills at the 5% subscription fee,
	// honoring the subscription -> BYOK -> deployment precedence explicitly
	// rather than by coincidence of BYOK being absent off the router-key path.
	ctx := context.WithValue(context.Background(), ExternalAPIKeysContextKey{}, []*auth.ExternalAPIKey{
		{Provider: providers.ProviderAnthropic, Plaintext: []byte("sk-ant-api-byok")},
	})
	headers := http.Header{"Authorization": []string{"Bearer sk-ant-oat01-subscription-token"}}
	out := resolveAndInjectCredentials(ctx, providers.ProviderAnthropic, headers)
	creds := CredentialsFromContext(out)
	require.NotNil(t, creds)
	assert.True(t, creds.OAuth, "the inbound subscription bearer must win over BYOK")
	assert.Equal(t, credSourceSubscription, creds.Source)
	assert.Equal(t, []byte("sk-ant-oat01-subscription-token"), creds.APIKey)
}

func TestResolveAndInjectCredentials_SelfHostedInboundSubscription(t *testing.T) {
	// No router key (nil installation): the caller's own Authorization bearer
	// carries the subscription token and is resolved via client extraction.
	headers := http.Header{"Authorization": []string{"Bearer sk-ant-oat01-subscription-token"}}
	out := resolveAndInjectCredentials(context.Background(), providers.ProviderAnthropic, headers)
	creds := CredentialsFromContext(out)
	require.NotNil(t, creds)
	assert.True(t, creds.OAuth)
	assert.Equal(t, credSourceSubscription, creds.Source)
}

func TestResolveAndInjectCredentials_RouterKeyedInboundSubscription(t *testing.T) {
	// Claude Code routed through the Weave Router: the router key authenticates
	// via X-Weave-Router-Key (installation set), while CC leaves its own
	// subscription OAuth token in Authorization. No dedicated header, no BYOK.
	// The inbound bearer must resolve as the subscription credential so the turn
	// bills at the 5% fee — the managed-CC case this path exists for.
	ctx := context.WithValue(context.Background(), InstallationIDContextKey{}, testInstallationID)
	headers := http.Header{"Authorization": []string{"Bearer sk-ant-oat01-subscription-token"}}
	out := resolveAndInjectCredentials(ctx, providers.ProviderAnthropic, headers)
	creds := CredentialsFromContext(out)
	require.NotNil(t, creds)
	assert.True(t, creds.OAuth,
		"a managed CC turn must resolve its inbound subscription bearer even when router-keyed")
	assert.Equal(t, credSourceSubscription, creds.Source)
	assert.Equal(t, []byte("sk-ant-oat01-subscription-token"), creds.APIKey)
}

func TestResolveAndInjectCredentials_RouterKeyedInboundApiKeyNotForwarded(t *testing.T) {
	// The router-key path still must NOT forward a general inbound API key: only
	// the sk-ant-oat OAuth subset is honored. A real client API key in
	// Authorization must resolve to no credential, so the deployment key (not the
	// client's key) is the upstream fallback — preserving the cross-provider-leak
	// guard the OAuth carve-out is careful not to widen.
	ctx := context.WithValue(context.Background(), InstallationIDContextKey{}, testInstallationID)
	headers := http.Header{"Authorization": []string{"Bearer sk-ant-api-real-client-key"}}
	out := resolveAndInjectCredentials(ctx, providers.ProviderAnthropic, headers)
	assert.Nil(t, CredentialsFromContext(out),
		"a non-OAuth inbound API key must not be forwarded on the router-key path; the deployment key is the correct fallback")
}

func TestClearCredentials(t *testing.T) {
	ctx := context.WithValue(context.Background(), CredentialsContextKey{},
		&Credentials{APIKey: []byte("sk-ant-oat01-token"), Source: credSourceSubscription, OAuth: true})
	require.NotNil(t, CredentialsFromContext(ctx))

	cleared := clearCredentials(ctx)
	assert.Nil(t, CredentialsFromContext(cleared),
		"clearCredentials must make CredentialsFromContext report none so the synthetic call falls back to the deployment key")
}

func TestResolveSummarizerCreds_DropsSubscriptionToken(t *testing.T) {
	// The synthetic summarizer body has no Claude Code identity block, which a
	// subscription token requires — so it must never run on one.
	headers := http.Header{"Authorization": []string{"Bearer sk-ant-oat01-subscription-token"}}
	creds := resolveSummarizerCreds(context.Background(), providers.ProviderAnthropic, headers)
	assert.Nil(t, creds,
		"resolveSummarizerCreds must decline a subscription OAuth token so the synthetic summary call doesn't 401")
}

func TestServedOnSubscription(t *testing.T) {
	t.Run("true for an OAuth credential", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), CredentialsContextKey{},
			&Credentials{APIKey: []byte("sk-ant-oat01-token"), Source: credSourceSubscription, OAuth: true})
		assert.True(t, servedOnSubscription(ctx), "billing must treat an OAuth-credentialed turn as subscription-served")
	})
	t.Run("false for a non-OAuth credential", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), CredentialsContextKey{},
			&Credentials{APIKey: []byte("sk-ant-api-byok"), Source: credSourceBYOK})
		assert.False(t, servedOnSubscription(ctx))
	})
	t.Run("false when no credential resolved (deployment key)", func(t *testing.T) {
		assert.False(t, servedOnSubscription(context.Background()),
			"a deployment-key turn is Weave-fronted and must bill at full cost")
	})
}

func TestResolveSummarizerCreds_ReturnsBYOK(t *testing.T) {
	ctx := context.WithValue(context.Background(), ExternalAPIKeysContextKey{}, []*auth.ExternalAPIKey{
		{Provider: providers.ProviderAnthropic, Plaintext: []byte("sk-ant-api-byok")},
	})
	creds := resolveSummarizerCreds(ctx, providers.ProviderAnthropic, http.Header{})
	require.NotNil(t, creds)
	assert.Equal(t, []byte("sk-ant-api-byok"), creds.APIKey,
		"a real BYOK key is a valid summarizer credential and must be used")
}
