package openaicompat_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/providers/openaicompat"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProxy_ForwardsToChatCompletionsUnderVersionedBaseURL(t *testing.T) {
	var (
		gotPath string
		gotAuth string
		gotBody map[string]any
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(body, &gotBody))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion"}`))
	}))
	defer upstream.Close()

	c := openaicompat.NewClient("test-key", upstream.URL+"/api/v1/")
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))

	body := []byte(`{"model":"qwen/qwen3-30b-a3b-instruct-2507","messages":[{"role":"user","content":"hi"}]}`)
	prep := providers.PreparedRequest{Body: body, Headers: make(http.Header)}
	err := c.Proxy(context.Background(), router.Decision{Model: "qwen/qwen3-30b-a3b-instruct-2507"}, prep, rec, clientReq)

	require.NoError(t, err)
	assert.Equal(t, "/api/v1/chat/completions", gotPath)
	assert.Equal(t, "Bearer test-key", gotAuth)
	assert.Equal(t, "qwen/qwen3-30b-a3b-instruct-2507", gotBody["model"])
	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestProxy_BYOKCredentialsOverrideEnvKey covers the case where a BYOK key is
// stashed on context (e.g. a managed installation that brings its own
// OpenRouter key). The context credential must win over the deployment-level
// env key.
func TestProxy_BYOKCredentialsOverrideEnvKey(t *testing.T) {
	var gotAuth string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-byok","object":"chat.completion"}`))
	}))
	defer upstream.Close()

	c := openaicompat.NewClient("deployment-key", upstream.URL+"/api/v1")
	byok := &proxy.Credentials{APIKey: []byte("sk-or-v1-byok-key"), Source: "byok"}
	ctx := context.WithValue(context.Background(), proxy.CredentialsContextKey{}, byok)

	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	prep := providers.PreparedRequest{Body: []byte(`{"model":"m","messages":[]}`), Headers: make(http.Header)}
	err := c.Proxy(ctx, router.Decision{Model: "m"}, prep, rec, clientReq)

	require.NoError(t, err)
	assert.Equal(t, "Bearer sk-or-v1-byok-key", gotAuth,
		"BYOK credentials on context must override the deployment-level env key")
}

// TestProxy_DevModeEnvKeyUsedWhenNoCredentialsOnContext covers the dev-mode /
// non-BYOK path: no credentials are stashed on context (WithAuth middleware
// is skipped), so setAuth must fall back to the deployment-level env key.
// This is the primary path for self-hosters and local dev with OPENROUTER_API_KEY
// set in .env.local.
func TestProxy_DevModeEnvKeyUsedWhenNoCredentialsOnContext(t *testing.T) {
	var gotAuth string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-dev","object":"chat.completion"}`))
	}))
	defer upstream.Close()

	c := openaicompat.NewClient("sk-or-v1-env-key", upstream.URL+"/api/v1")
	// context.Background() has no credentials — simulates ROUTER_DEV_MODE=true
	// where WithAuth is skipped and CredentialsFromContext returns nil.
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	// Inbound has only x-api-key (Anthropic-format client like Claude Code).
	// No Authorization header — ExtractClientCredentials returns nil for OpenRouter.
	clientReq.Header.Set("X-Api-Key", "rk_router_key")
	prep := providers.PreparedRequest{Body: []byte(`{"model":"deepseek/deepseek-v4-pro","messages":[]}`), Headers: make(http.Header)}
	err := c.Proxy(context.Background(), router.Decision{Model: "deepseek/deepseek-v4-pro"}, prep, rec, clientReq)

	require.NoError(t, err)
	assert.Equal(t, "Bearer sk-or-v1-env-key", gotAuth,
		"when no credentials are on context (dev mode / no BYOK), the deployment env key must be sent to OpenRouter")
}

func TestPassthrough_StripsInboundV1Prefix(t *testing.T) {
	var gotPath string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer upstream.Close()

	c := openaicompat.NewClient("test-key", upstream.URL+"/api/v1")
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)

	prep := providers.PreparedRequest{Headers: make(http.Header)}
	err := c.Passthrough(context.Background(), prep, rec, clientReq)
	require.NoError(t, err)

	assert.Equal(t, "/api/v1/models", gotPath)
	assert.Equal(t, http.StatusOK, rec.Code)
}
