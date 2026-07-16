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
	// Router-keyed request with both a BYOK key and the dedicated subscription
	// header: subscription must win, read past the router-key guard.
	ctx := context.Background()
	ctx = context.WithValue(ctx, InstallationIDContextKey{}, testInstallationID)
	ctx = context.WithValue(ctx, ExternalAPIKeysContextKey{}, []*auth.ExternalAPIKey{
		{Provider: providers.ProviderAnthropic, Plaintext: []byte("sk-ant-api-byok")},
	})
	ctx = context.WithValue(ctx, AnthropicSubscriptionContextKey{}, "sk-ant-oat01-subscription-token")

	out := (&Service{}).resolveAndInjectCredentials(ctx, providers.ProviderAnthropic, http.Header{})
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

	out := (&Service{}).resolveAndInjectCredentials(ctx, providers.ProviderOpenAI, http.Header{})
	creds := CredentialsFromContext(out)
	require.NotNil(t, creds)
	assert.False(t, creds.OAuth)
	assert.Equal(t, []byte("sk-oai-byok"), creds.APIKey,
		"a non-Anthropic route must use its own BYOK key, never the Anthropic subscription token")
}

func TestResolveAndInjectCredentials_InboundSubscriptionBeatsBYOK(t *testing.T) {
	// Self-hosted: inbound subscription bearer must beat BYOK (subscription ->
	// BYOK -> deployment precedence), not just because BYOK happens to be absent.
	ctx := context.WithValue(context.Background(), ExternalAPIKeysContextKey{}, []*auth.ExternalAPIKey{
		{Provider: providers.ProviderAnthropic, Plaintext: []byte("sk-ant-api-byok")},
	})
	headers := http.Header{"Authorization": []string{"Bearer sk-ant-oat01-subscription-token"}}
	out := (&Service{}).resolveAndInjectCredentials(ctx, providers.ProviderAnthropic, headers)
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
	out := (&Service{}).resolveAndInjectCredentials(context.Background(), providers.ProviderAnthropic, headers)
	creds := CredentialsFromContext(out)
	require.NotNil(t, creds)
	assert.True(t, creds.OAuth)
	assert.Equal(t, credSourceSubscription, creds.Source)
}

func TestResolveAndInjectCredentials_RouterKeyedInboundSubscription(t *testing.T) {
	// Managed CC: router key auth via X-Weave-Router-Key, CC's own subscription
	// OAuth token in Authorization, no dedicated header, no BYOK. Inbound bearer
	// must still resolve as the subscription credential.
	ctx := context.WithValue(context.Background(), InstallationIDContextKey{}, testInstallationID)
	headers := http.Header{"Authorization": []string{"Bearer sk-ant-oat01-subscription-token"}}
	out := (&Service{}).resolveAndInjectCredentials(ctx, providers.ProviderAnthropic, headers)
	creds := CredentialsFromContext(out)
	require.NotNil(t, creds)
	assert.True(t, creds.OAuth,
		"a managed CC turn must resolve its inbound subscription bearer even when router-keyed")
	assert.Equal(t, credSourceSubscription, creds.Source)
	assert.Equal(t, []byte("sk-ant-oat01-subscription-token"), creds.APIKey)
}

func TestResolveAndInjectCredentials_RouterKeyedInboundApiKeyNotForwarded(t *testing.T) {
	// Router-key path must not forward a general inbound API key, only the
	// sk-ant-oat OAuth subset — otherwise it'd widen the cross-provider-leak guard.
	ctx := context.WithValue(context.Background(), InstallationIDContextKey{}, testInstallationID)
	headers := http.Header{"Authorization": []string{"Bearer sk-ant-api-real-client-key"}}
	out := (&Service{}).resolveAndInjectCredentials(ctx, providers.ProviderAnthropic, headers)
	assert.Nil(t, CredentialsFromContext(out),
		"a non-OAuth inbound API key must not be forwarded on the router-key path; the deployment key is the correct fallback")
}

const codexTestJWT = "eyJhbGciOiJSUzI1NiJ9.codex-access.signature"

func TestResolveAndInjectCredentials_CodexDedicatedHeadersBeatBYOK(t *testing.T) {
	// Dedicated Codex headers must win over BYOK, read past the router-key
	// guard — mirrors the Anthropic dedicated-header path.
	ctx := context.WithValue(context.Background(), InstallationIDContextKey{}, testInstallationID)
	ctx = context.WithValue(ctx, ExternalAPIKeysContextKey{}, []*auth.ExternalAPIKey{
		{Provider: providers.ProviderOpenAI, Plaintext: []byte("sk-oai-byok")},
	})
	ctx = context.WithValue(ctx, OpenAISubscriptionContextKey{}, codexTestJWT)
	ctx = context.WithValue(ctx, OpenAIAccountIDContextKey{}, "acct-999")

	out := (&Service{}).resolveAndInjectCredentials(ctx, providers.ProviderOpenAI, http.Header{})
	creds := CredentialsFromContext(out)
	require.NotNil(t, creds)
	assert.True(t, creds.OAuth)
	assert.Equal(t, credSourceCodexSubscription, creds.Source)
	assert.Equal(t, []byte(codexTestJWT), creds.APIKey)
	assert.Equal(t, []byte("acct-999"), creds.AccountID)
}

func TestResolveAndInjectCredentials_CodexInboundBeatsBYOK(t *testing.T) {
	// Self-hosted (no router key): an inbound Authorization JWT + ChatGPT-Account-ID
	// must beat a present BYOK OpenAI key so the turn bills at the subscription fee.
	ctx := context.WithValue(context.Background(), ExternalAPIKeysContextKey{}, []*auth.ExternalAPIKey{
		{Provider: providers.ProviderOpenAI, Plaintext: []byte("sk-oai-byok")},
	})
	headers := http.Header{
		"Authorization":      []string{"Bearer " + codexTestJWT},
		"Chatgpt-Account-Id": []string{"acct-999"},
	}
	out := (&Service{}).resolveAndInjectCredentials(ctx, providers.ProviderOpenAI, headers)
	creds := CredentialsFromContext(out)
	require.NotNil(t, creds)
	assert.True(t, creds.OAuth, "the inbound Codex subscription must win over BYOK")
	assert.Equal(t, credSourceCodexSubscription, creds.Source)
	assert.Equal(t, []byte("acct-999"), creds.AccountID)
}

func TestResolveAndInjectCredentials_RouterKeyedInboundCodexSubscription(t *testing.T) {
	// Managed Codex: router key auth via X-Weave-Router-Key, Codex's own ChatGPT
	// auth in Authorization + ChatGPT-Account-ID, no dedicated header, no BYOK.
	// Inbound bearer must still resolve as Codex subscription (mirrors #460).
	ctx := context.WithValue(context.Background(), InstallationIDContextKey{}, testInstallationID)
	headers := http.Header{
		"Authorization":      []string{"Bearer " + codexTestJWT},
		"Chatgpt-Account-Id": []string{"acct-999"},
	}
	out := (&Service{}).resolveAndInjectCredentials(ctx, providers.ProviderOpenAI, headers)
	creds := CredentialsFromContext(out)
	require.NotNil(t, creds)
	assert.True(t, creds.OAuth,
		"a managed Codex turn must resolve its inbound subscription bearer even when router-keyed")
	assert.Equal(t, credSourceCodexSubscription, creds.Source)
	assert.Equal(t, []byte("acct-999"), creds.AccountID)
}

func TestResolveAndInjectCredentials_RouterKeyedInboundOpenAIApiKeyNotForwarded(t *testing.T) {
	// Router-key path must not forward a general inbound OpenAI key — only the
	// Codex OAuth subset (JWT + ChatGPT-Account-ID) is honored.
	ctx := context.WithValue(context.Background(), InstallationIDContextKey{}, testInstallationID)
	headers := http.Header{"Authorization": []string{"Bearer sk-proj-real-client-key"}}
	out := (&Service{}).resolveAndInjectCredentials(ctx, providers.ProviderOpenAI, headers)
	assert.Nil(t, CredentialsFromContext(out),
		"a non-OAuth inbound OpenAI key must not be forwarded on the router-key path; the deployment key is the correct fallback")
}

func TestResolveAndInjectCredentials_CodexHeadersIgnoredForNonOpenAI(t *testing.T) {
	// The Codex token can only pay for OpenAI; a non-OpenAI route must not
	// resolve it.
	ctx := context.WithValue(context.Background(), InstallationIDContextKey{}, testInstallationID)
	ctx = context.WithValue(ctx, ExternalAPIKeysContextKey{}, []*auth.ExternalAPIKey{
		{Provider: providers.ProviderAnthropic, Plaintext: []byte("sk-ant-api-byok")},
	})
	ctx = context.WithValue(ctx, OpenAISubscriptionContextKey{}, codexTestJWT)
	ctx = context.WithValue(ctx, OpenAIAccountIDContextKey{}, "acct-999")

	out := (&Service{}).resolveAndInjectCredentials(ctx, providers.ProviderAnthropic, http.Header{})
	creds := CredentialsFromContext(out)
	require.NotNil(t, creds)
	assert.False(t, creds.OAuth)
	assert.Equal(t, []byte("sk-ant-api-byok"), creds.APIKey,
		"an Anthropic route must use its own BYOK key, never the Codex subscription token")
}

func TestCodexResponsesRequest(t *testing.T) {
	t.Run("dedicated headers on a router-keyed request", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), InstallationIDContextKey{}, testInstallationID)
		ctx = context.WithValue(ctx, OpenAISubscriptionContextKey{}, codexTestJWT)
		ctx = context.WithValue(ctx, OpenAIAccountIDContextKey{}, "acct-1")
		assert.True(t, codexResponsesRequest(ctx, http.Header{}))
	})
	t.Run("inbound bearer + account-id off the router key", func(t *testing.T) {
		headers := http.Header{
			"Authorization":      []string{"Bearer " + codexTestJWT},
			"Chatgpt-Account-Id": []string{"acct-1"},
		}
		assert.True(t, codexResponsesRequest(context.Background(), headers))
	})
	t.Run("inbound bearer + account-id on a router-keyed request", func(t *testing.T) {
		// Detection must agree with resolveAndInjectCredentials here: not
		// gated on the installation's absence.
		ctx := context.WithValue(context.Background(), InstallationIDContextKey{}, testInstallationID)
		headers := http.Header{
			"Authorization":      []string{"Bearer " + codexTestJWT},
			"Chatgpt-Account-Id": []string{"acct-1"},
		}
		assert.True(t, codexResponsesRequest(ctx, headers))
	})
	t.Run("dedicated token without account-id is not a Codex request", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), InstallationIDContextKey{}, testInstallationID)
		ctx = context.WithValue(ctx, OpenAISubscriptionContextKey{}, codexTestJWT)
		assert.False(t, codexResponsesRequest(ctx, http.Header{}))
	})
	t.Run("plain request is not a Codex request", func(t *testing.T) {
		assert.False(t, codexResponsesRequest(context.Background(), http.Header{}))
	})
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
