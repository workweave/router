package proxy_test

import (
	"context"
	"net/http"
	"testing"

	"workweave/router/internal/auth"
	"workweave/router/internal/proxy"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildCredentialsMap_NilOnEmptySlice(t *testing.T) {
	m := proxy.BuildCredentialsMap(nil)
	assert.Nil(t, m,
		"BuildCredentialsMap must return nil when given no keys so callers can "+
			"cheaply distinguish 'no BYOK configured' from 'BYOK configured but empty'")
}

func TestBuildCredentialsMap_IndexesByProvider(t *testing.T) {
	keys := []*auth.ExternalAPIKey{
		{Provider: "anthropic", Plaintext: []byte("sk-ant-byok")},
		{Provider: "openai", Plaintext: []byte("sk-oai-byok")},
	}
	m := proxy.BuildCredentialsMap(keys)
	require.NotNil(t, m)
	require.Contains(t, m, "anthropic")
	assert.Equal(t, []byte("sk-ant-byok"), m["anthropic"].APIKey)
	assert.Equal(t, "byok", m["anthropic"].Source,
		"BYOK credentials must carry Source='byok' for observability")
	require.Contains(t, m, "openai")
	assert.Equal(t, []byte("sk-oai-byok"), m["openai"].APIKey)
}

func TestBuildCredentialsMap_DropsEmptyPlaintext(t *testing.T) {
	// An empty Plaintext indicates a stale or malformed BYOK row (insertion
	// bug, or decryption that produced no bytes). Such an entry must not
	// enroll the provider into the routing eligibility set: argmax would
	// pick it and the upstream call would 401 with no auth header.
	keys := []*auth.ExternalAPIKey{
		{Provider: "openrouter", Plaintext: []byte{}},
		{Provider: "anthropic", Plaintext: []byte("sk-ant-byok")},
	}
	m := proxy.BuildCredentialsMap(keys)
	require.NotNil(t, m)
	assert.NotContains(t, m, "openrouter",
		"BuildCredentialsMap must drop entries with empty Plaintext so the routing layer cannot enroll a provider that would 401 on dispatch")
	assert.Contains(t, m, "anthropic")
}

func TestBuildCredentialsMap_NilWhenAllEmpty(t *testing.T) {
	keys := []*auth.ExternalAPIKey{
		{Provider: "openrouter", Plaintext: []byte{}},
		{Provider: "fireworks", Plaintext: nil},
	}
	m := proxy.BuildCredentialsMap(keys)
	assert.Nil(t, m,
		"when every BYOK entry is empty the map must be nil so callers see 'no BYOK configured' rather than 'BYOK present but unusable'")
}

func TestExtractClientCredentials_Anthropic(t *testing.T) {
	headers := http.Header{"X-Api-Key": []string{"sk-ant-client"}}
	creds := proxy.ExtractClientCredentials("anthropic", headers)
	require.NotNil(t, creds)
	assert.Equal(t, []byte("sk-ant-client"), creds.APIKey)
	assert.Equal(t, "client", creds.Source,
		"client-header credentials must carry Source='client'")
}

func TestExtractClientCredentials_OpenAI(t *testing.T) {
	headers := http.Header{"Authorization": []string{"Bearer sk-oai-client"}}
	creds := proxy.ExtractClientCredentials("openai", headers)
	require.NotNil(t, creds)
	assert.Equal(t, []byte("sk-oai-client"), creds.APIKey)
	assert.Equal(t, "client", creds.Source)
}

func TestExtractClientCredentials_Google(t *testing.T) {
	headers := http.Header{"Authorization": []string{"Bearer goog-client-key"}}
	creds := proxy.ExtractClientCredentials("google", headers)
	require.NotNil(t, creds)
	assert.Equal(t, []byte("goog-client-key"), creds.APIKey)
}

func TestExtractClientCredentials_MissingHeader(t *testing.T) {
	creds := proxy.ExtractClientCredentials("anthropic", http.Header{})
	assert.Nil(t, creds,
		"ExtractClientCredentials must return nil when the required header is absent")
}

func TestExtractClientCredentials_RejectsRouterBearerForOpenAI(t *testing.T) {
	headers := http.Header{"Authorization": []string{"Bearer rk_should_not_leak_upstream"}}
	creds := proxy.ExtractClientCredentials("openai", headers)
	assert.Nil(t, creds,
		"router-issued bearer tokens (rk_...) must never be forwarded as upstream OpenAI credentials")
}

func TestExtractClientCredentials_RejectsRouterBearerForGoogle(t *testing.T) {
	headers := http.Header{"Authorization": []string{"Bearer rk_should_not_leak_upstream"}}
	creds := proxy.ExtractClientCredentials("google", headers)
	assert.Nil(t, creds,
		"router-issued bearer tokens (rk_...) must never be forwarded as upstream Google credentials")
}

func TestExtractClientCredentials_OpenRouter(t *testing.T) {
	headers := http.Header{"Authorization": []string{"Bearer sk-or-v1-byok-openrouter-key"}}
	creds := proxy.ExtractClientCredentials("openrouter", headers)
	require.NotNil(t, creds)
	assert.Equal(t, []byte("sk-or-v1-byok-openrouter-key"), creds.APIKey)
	assert.Equal(t, "client", creds.Source)
}

func TestExtractClientCredentials_Fireworks(t *testing.T) {
	headers := http.Header{"Authorization": []string{"Bearer fw_byok-fireworks-key"}}
	creds := proxy.ExtractClientCredentials("fireworks", headers)
	require.NotNil(t, creds)
	assert.Equal(t, []byte("fw_byok-fireworks-key"), creds.APIKey)
	assert.Equal(t, "client", creds.Source)
}

func TestExtractClientCredentials_RejectsRouterBearerForOpenRouter(t *testing.T) {
	headers := http.Header{"Authorization": []string{"Bearer rk_should_not_leak_upstream"}}
	creds := proxy.ExtractClientCredentials("openrouter", headers)
	assert.Nil(t, creds,
		"router-issued bearer tokens (rk_...) must never be forwarded as upstream OpenRouter credentials")
}

func TestExtractClientCredentials_RejectsRouterBearerForFireworks(t *testing.T) {
	headers := http.Header{"Authorization": []string{"Bearer rk_should_not_leak_upstream"}}
	creds := proxy.ExtractClientCredentials("fireworks", headers)
	assert.Nil(t, creds,
		"router-issued bearer tokens (rk_...) must never be forwarded as upstream Fireworks credentials")
}

func TestExtractClientCredentials_OpenRouterNoAuthHeader(t *testing.T) {
	// Anthropic-format clients (Claude Code with x-api-key) have no Authorization
	// header. ExtractClientCredentials must return nil so the caller falls back to
	// the deployment-level env key rather than injecting empty credentials.
	headers := http.Header{"X-Api-Key": []string{"rk_router_key"}}
	creds := proxy.ExtractClientCredentials("openrouter", headers)
	assert.Nil(t, creds,
		"when no Authorization header is present, ExtractClientCredentials must return nil for openrouter "+
			"so setAuth falls back to the deployment-level OPENROUTER_API_KEY env key")
}

func TestExtractClientCredentials_RejectsRouterKeyForAnthropic(t *testing.T) {
	headers := http.Header{"X-Api-Key": []string{"rk_should_not_leak_upstream"}}
	creds := proxy.ExtractClientCredentials("anthropic", headers)
	assert.Nil(t, creds,
		"router-issued tokens (rk_...) supplied via x-api-key must never be forwarded as upstream Anthropic credentials")
}

func TestResolveCredentials_RouterKeyDoesNotLeakWhenBYOKAbsent(t *testing.T) {
	headers := http.Header{"Authorization": []string{"Bearer rk_authed_router_key"}}
	creds := proxy.ResolveCredentials("openai", nil, headers)
	assert.Nil(t, creds,
		"when no BYOK is configured and the inbound bearer is a router key, ResolveCredentials must NOT fall back to forwarding it upstream")
}

func TestExtractClientCredentials_RejectsRouterBearerWithLeadingWhitespace(t *testing.T) {
	headers := http.Header{"Authorization": []string{"Bearer  rk_whitespace_bypass"}}
	creds := proxy.ExtractClientCredentials("openai", headers)
	assert.Nil(t, creds,
		"the router-key guard must canonicalize whitespace; the auth middleware accepts 'Bearer  rk_...' as a router credential, so this path must not forward it upstream")
}

func TestExtractClientCredentials_RejectsRouterKeyWithLeadingWhitespaceForAnthropic(t *testing.T) {
	headers := http.Header{"X-Api-Key": []string{"  rk_whitespace_bypass"}}
	creds := proxy.ExtractClientCredentials("anthropic", headers)
	assert.Nil(t, creds,
		"x-api-key values must be TrimSpace'd before the prefix check to match the auth middleware's normalization")
}

func TestExtractClientCredentials_TrimsWhitespaceFromForwardedKey(t *testing.T) {
	headers := http.Header{"Authorization": []string{"Bearer  sk-oai-client  "}}
	creds := proxy.ExtractClientCredentials("openai", headers)
	require.NotNil(t, creds)
	assert.Equal(t, []byte("sk-oai-client"), creds.APIKey,
		"the forwarded credential must be canonicalized; passing through embedded whitespace risks confusing upstream providers and inviting normalization-bypass bugs")
}

func TestResolveCredentials_BYOKTakesPrecedence(t *testing.T) {
	byok := map[string]*proxy.Credentials{
		"anthropic": {APIKey: []byte("sk-ant-byok"), Source: "byok"},
	}
	headers := http.Header{"X-Api-Key": []string{"sk-ant-client"}}
	creds := proxy.ResolveCredentials("anthropic", byok, headers)
	require.NotNil(t, creds)
	assert.Equal(t, "byok", creds.Source,
		"when BYOK key is configured for a provider it must take precedence over client headers")
	assert.Equal(t, []byte("sk-ant-byok"), creds.APIKey)
}

func TestResolveCredentials_FallsBackToClientHeaders(t *testing.T) {
	headers := http.Header{"X-Api-Key": []string{"sk-ant-client"}}
	creds := proxy.ResolveCredentials("anthropic", nil, headers)
	require.NotNil(t, creds,
		"when no BYOK key is configured, client header credentials must be used")
	assert.Equal(t, "client", creds.Source)
	assert.Equal(t, []byte("sk-ant-client"), creds.APIKey)
}

func TestResolveCredentials_NilWhenNeitherAvailable(t *testing.T) {
	creds := proxy.ResolveCredentials("anthropic", nil, http.Header{})
	assert.Nil(t, creds,
		"ResolveCredentials must return nil when neither BYOK nor client headers supply credentials")
}

func TestCredentialsFromContext_ReturnsNilWhenAbsent(t *testing.T) {
	creds := proxy.CredentialsFromContext(context.Background())
	assert.Nil(t, creds,
		"CredentialsFromContext must return nil when no credentials are on the context")
}

func TestCredentialsFromContext_ReturnsStashedCredentials(t *testing.T) {
	want := &proxy.Credentials{APIKey: []byte("sk-test"), Source: "byok"}
	ctx := context.WithValue(context.Background(), proxy.CredentialsContextKey{}, want)
	got := proxy.CredentialsFromContext(ctx)
	require.NotNil(t, got)
	assert.Equal(t, want.Source, got.Source)
	assert.Equal(t, want.APIKey, got.APIKey)
}
