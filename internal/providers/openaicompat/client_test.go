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

// TestProxy_BuffersErrorBodyAndDoesNotTouchWriter is the core failover
// precondition: when the upstream returns >=400, openaicompat MUST NOT
// write headers or status to the client writer. Instead it returns
// *UpstreamErrorResponse carrying the buffered body, leaving the
// dispatcher free to retry on another binding.
func TestProxy_BuffersErrorBodyAndDoesNotTouchWriter(t *testing.T) {
	const errBody = `{"error":{"message":"fireworks is having a moment","type":"service_unavailable"}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(errBody))
	}))
	defer upstream.Close()

	c := openaicompat.NewClient("test-key", upstream.URL+"/api/v1")
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	prep := providers.PreparedRequest{Body: []byte(`{"model":"deepseek/deepseek-v4-pro","messages":[]}`), Headers: make(http.Header)}

	err := c.Proxy(context.Background(), router.Decision{Model: "deepseek/deepseek-v4-pro"}, prep, rec, clientReq)

	require.Error(t, err)
	var buffered *providers.UpstreamErrorResponse
	require.ErrorAs(t, err, &buffered, "openaicompat must return *UpstreamErrorResponse for >=400 status so the dispatcher can retry")
	assert.Equal(t, http.StatusServiceUnavailable, buffered.Status)
	assert.Equal(t, errBody, string(buffered.Body))
	assert.Equal(t, "application/json", buffered.Headers.Get("Content-Type"))

	// The writer must remain pristine — no headers, no body, no status.
	// httptest.ResponseRecorder defaults to 200 and reports it via Code
	// even before WriteHeader is called, so we check that nothing was
	// actually written instead.
	assert.Equal(t, 0, rec.Body.Len(), "writer must not be touched on retryable upstream error")
	assert.False(t, rec.HeaderMap.Get("Content-Type") != "", "writer headers must not be set on retryable upstream error")
}

// TestProxy_BuffersErrorBodyTruncatedAtCap ensures that a chatty upstream
// can't OOM us via a huge error body. Beyond MaxBufferedErrorBytes the
// remainder is drained and discarded.
func TestProxy_BuffersErrorBodyTruncatedAtCap(t *testing.T) {
	// 256KB body, well over the 64KB cap.
	largeBody := strings.Repeat("x", 256*1024)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(largeBody))
	}))
	defer upstream.Close()

	c := openaicompat.NewClient("test-key", upstream.URL+"/api/v1")
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	prep := providers.PreparedRequest{Body: []byte(`{"model":"x","messages":[]}`), Headers: make(http.Header)}

	err := c.Proxy(context.Background(), router.Decision{Model: "x"}, prep, rec, clientReq)

	var buffered *providers.UpstreamErrorResponse
	require.ErrorAs(t, err, &buffered)
	assert.Equal(t, http.StatusBadGateway, buffered.Status)
	assert.Equal(t, providers.MaxBufferedErrorBytes, len(buffered.Body),
		"body must be truncated at MaxBufferedErrorBytes")
}

// TestProxy_4xxStillBuffered confirms that non-retryable 4xx is also
// buffered — the classifier (providers.IsRetryable) decides retry
// eligibility, not the adapter. Adapter just stops bytes from leaking
// to the client until the dispatcher decides whether to flush or retry.
func TestProxy_4xxStillBuffered(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad model"}`))
	}))
	defer upstream.Close()

	c := openaicompat.NewClient("test-key", upstream.URL+"/api/v1")
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	prep := providers.PreparedRequest{Body: []byte(`{"model":"x"}`), Headers: make(http.Header)}

	err := c.Proxy(context.Background(), router.Decision{Model: "x"}, prep, rec, clientReq)

	var buffered *providers.UpstreamErrorResponse
	require.ErrorAs(t, err, &buffered)
	assert.Equal(t, http.StatusBadRequest, buffered.Status)
	assert.False(t, providers.IsRetryableStatus(buffered.Status),
		"sanity: 400 must classify as non-retryable")
}

func TestProxy_ModelIDMapRewritesBodyModelField(t *testing.T) {
	var gotModel string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		gotModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-remapped","object":"chat.completion"}`))
	}))
	defer upstream.Close()

	modelIDMap := map[string]string{
		"deepseek/deepseek-v4-flash": "deepseek-v4-flash",
		"deepseek/deepseek-v4-pro":   "deepseek-v4-pro",
		"moonshotai/kimi-k2.6":       "kimi-k2.6",
	}
	c := openaicompat.NewClientWithModelIDMap("test-key", upstream.URL+"/api/v1", modelIDMap)

	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hello"}]}`)
	prep := providers.PreparedRequest{Body: body, Headers: make(http.Header)}
	err := c.Proxy(context.Background(), router.Decision{Model: "deepseek/deepseek-v4-flash"}, prep, rec, clientReq)

	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "deepseek-v4-flash", gotModel,
		"body 'model' field must be rewritten per modelIDMap")
}

func TestProxy_ModelIDMapLeavesUnmappedModelsAlone(t *testing.T) {
	var gotModel string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		gotModel = body.Model
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-unmapped","object":"chat.completion"}`))
	}))
	defer upstream.Close()

	modelIDMap := map[string]string{
		"deepseek/deepseek-v4-flash": "deepseek-v4-flash",
	}
	c := openaicompat.NewClientWithModelIDMap("test-key", upstream.URL+"/api/v1", modelIDMap)

	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}]}`)
	prep := providers.PreparedRequest{Body: body, Headers: make(http.Header)}
	err := c.Proxy(context.Background(), router.Decision{Model: "claude-opus-4-8"}, prep, rec, clientReq)

	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "claude-opus-4-8", gotModel,
		"unmapped model must be forwarded unchanged")
}

func TestProxy_NilModelIDMapDoesNotRewrite(t *testing.T) {
	var gotModel string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		gotModel = body.Model
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-nomap","object":"chat.completion"}`))
	}))
	defer upstream.Close()

	c := openaicompat.NewClient("test-key", upstream.URL+"/api/v1")

	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"deepseek/deepseek-v4-flash","messages":[]}`)
	prep := providers.PreparedRequest{Body: body, Headers: make(http.Header)}
	err := c.Proxy(context.Background(), router.Decision{Model: "deepseek/deepseek-v4-flash"}, prep, rec, clientReq)

	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "deepseek/deepseek-v4-flash", gotModel,
		"without modelIDMap the body must be forwarded unmodified")
}
