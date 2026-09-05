package openaicompat_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"weave-os/router/internal/providers"
	"weave-os/router/internal/providers/openaicompat"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/router"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMiniMaxBaseURL(t *testing.T) {
	tests := map[string]string{
		"":       openaicompat.MiniMaxGlobalBaseURL,
		"global": openaicompat.MiniMaxGlobalBaseURL,
		"cn":     openaicompat.MiniMaxCNBaseURL,
		"China":  openaicompat.MiniMaxCNBaseURL,
	}

	for region, want := range tests {
		t.Run(region, func(t *testing.T) {
			assert.Equal(t, want, openaicompat.MiniMaxBaseURL(region))
		})
	}
}

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

// TestProxy_BYOKCredentialsOverrideEnvKey: a BYOK key on context (e.g. a
// managed installation's own OpenRouter key) must win over the deployment env key.
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

// TestProxy_BYOKBaseURLOverridesDeploymentBaseURL: a BYOK key carrying its own
// base URL must be dispatched to the customer's endpoint, not the deployment's.
func TestProxy_BYOKBaseURLOverridesDeploymentBaseURL(t *testing.T) {
	deploymentHit := false
	deployment := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deploymentHit = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-deployment"}`))
	}))
	defer deployment.Close()

	var customerPath, customerAuth string
	customer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		customerPath = r.URL.Path
		customerAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-customer","object":"chat.completion"}`))
	}))
	defer customer.Close()

	c := openaicompat.NewClient("deployment-key", deployment.URL+"/api/v1")
	// Trailing slash is deliberate: EffectiveBaseURL must normalize it so the
	// joined path doesn't end up with a doubled separator.
	byok := &proxy.Credentials{
		APIKey:  []byte("mk-customer-key"),
		Source:  "byok",
		BaseURL: customer.URL + "/v1/",
	}
	ctx := context.WithValue(context.Background(), proxy.CredentialsContextKey{}, byok)

	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	prep := providers.PreparedRequest{Body: []byte(`{"model":"m","messages":[]}`), Headers: make(http.Header)}
	err := c.Proxy(ctx, router.Decision{Model: "m"}, prep, rec, clientReq)

	require.NoError(t, err)
	assert.False(t, deploymentHit, "a BYOK base URL must divert the call away from the deployment endpoint")
	assert.Equal(t, "/v1/chat/completions", customerPath)
	assert.Equal(t, "Bearer mk-customer-key", customerAuth)
}

// TestProxy_DeploymentBaseURLUsedWhenBYOKOmitsIt: a BYOK key with no base URL
// keeps the deployment's endpoint — only the credential changes.
func TestProxy_DeploymentBaseURLUsedWhenBYOKOmitsIt(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion"}`))
	}))
	defer upstream.Close()

	c := openaicompat.NewClient("deployment-key", upstream.URL+"/api/v1")
	byok := &proxy.Credentials{APIKey: []byte("mk-customer-key"), Source: "byok"}
	ctx := context.WithValue(context.Background(), proxy.CredentialsContextKey{}, byok)

	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	prep := providers.PreparedRequest{Body: []byte(`{"model":"m","messages":[]}`), Headers: make(http.Header)}
	err := c.Proxy(ctx, router.Decision{Model: "m"}, prep, rec, clientReq)

	require.NoError(t, err)
	assert.Equal(t, "/api/v1/chat/completions", gotPath,
		"a BYOK key without a base URL must keep using the deployment endpoint")
}

// TestProxy_DevModeEnvKeyUsedWhenNoCredentialsOnContext: with no context
// credentials (WithAuth skipped, e.g. ROUTER_DEV_MODE), setAuth must fall
// back to the deployment env key — the path self-hosters/local dev rely on.
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

// TestProxy_BuffersErrorBodyAndDoesNotTouchWriter: on upstream >=400,
// openaicompat must not touch the client writer — it returns
// *UpstreamErrorResponse so the dispatcher can retry on another binding.
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

	// ResponseRecorder reports Code=200 by default even without WriteHeader,
	// so check that nothing was actually written instead.
	assert.Equal(t, 0, rec.Body.Len(), "writer must not be touched on retryable upstream error")
	assert.False(t, rec.HeaderMap.Get("Content-Type") != "", "writer headers must not be set on retryable upstream error")
}

// TestProxy_BuffersErrorBodyTruncatedAtCap: a chatty upstream can't OOM us —
// beyond MaxBufferedErrorBytes the remainder is drained and discarded.
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

// TestProxy_4xxStillBuffered: non-retryable 4xx is buffered too — the
// adapter just withholds bytes; providers.IsRetryable decides eligibility.
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

// TestGatewayClient_UnconfiguredBaseURLDoesNotFallBackToOpenRouter: an
// unconfigured gateway must fail, not silently spend the tenant's token.
func TestGatewayClient_UnconfiguredBaseURLDoesNotFallBackToOpenRouter(t *testing.T) {
	c := openaicompat.NewGatewayClient("gateway-token", "")
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	prep := providers.PreparedRequest{Body: []byte(`{"model":"gpt-5","messages":[]}`), Headers: make(http.Header)}

	err := c.Proxy(context.Background(), router.Decision{Model: "gpt-5"}, prep, rec, clientReq)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "openrouter.ai")
}

// TestGatewayClient_DispatchesToConfiguredEndpoint: the gateway path is
// otherwise the plain Chat Completions one, bearer-authenticated.
func TestGatewayClient_DispatchesToConfiguredEndpoint(t *testing.T) {
	var gotPath, gotAuth string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-gw","object":"chat.completion"}`))
	}))
	defer upstream.Close()

	c := openaicompat.NewGatewayClient("gateway-token", upstream.URL+"/api/v2/cortex/v1")
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	prep := providers.PreparedRequest{Body: []byte(`{"model":"gpt-5","messages":[]}`), Headers: make(http.Header)}

	require.NoError(t, c.Proxy(context.Background(), router.Decision{Model: "gpt-5"}, prep, rec, clientReq))
	assert.Equal(t, "/api/v2/cortex/v1/chat/completions", gotPath)
	assert.Equal(t, "Bearer gateway-token", gotAuth)
}

// TestProxy_DefaultHeadersSentOnEveryRequest: provider-mandated headers
// configured via WithDefaultHeaders ride on every Proxy dispatch.
func TestProxy_DefaultHeadersSentOnEveryRequest(t *testing.T) {
	var gotZDR string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotZDR = r.Header.Get("Wafer-ZDR")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-zdr","object":"chat.completion"}`))
	}))
	defer upstream.Close()

	c := openaicompat.NewClient("test-key", upstream.URL+"/v1").
		WithDefaultHeaders(http.Header{"Wafer-ZDR": []string{"required"}})
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	prep := providers.PreparedRequest{Body: []byte(`{"model":"GLM-5.2","messages":[]}`), Headers: make(http.Header)}

	require.NoError(t, c.Proxy(context.Background(), router.Decision{Model: "z-ai/glm-5.2"}, prep, rec, clientReq))
	assert.Equal(t, "required", gotZDR)
}

// TestPassthrough_DefaultHeadersSentOnEveryRequest: default headers must also
// ride passthrough calls, not just routed ones.
func TestPassthrough_DefaultHeadersSentOnEveryRequest(t *testing.T) {
	var gotZDR string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotZDR = r.Header.Get("Wafer-ZDR")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer upstream.Close()

	c := openaicompat.NewClient("test-key", upstream.URL+"/v1").
		WithDefaultHeaders(http.Header{"Wafer-ZDR": []string{"required"}})
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)

	require.NoError(t, c.Passthrough(context.Background(), providers.PreparedRequest{Headers: make(http.Header)}, rec, clientReq))
	assert.Equal(t, "required", gotZDR)
}

// TestProxy_PreparedHeadersOverrideDefaults: per-request prepared headers
// win over WithDefaultHeaders values for the same key.
func TestProxy_PreparedHeadersOverrideDefaults(t *testing.T) {
	var gotHeader string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Custom")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-o","object":"chat.completion"}`))
	}))
	defer upstream.Close()

	c := openaicompat.NewClient("test-key", upstream.URL+"/v1").
		WithDefaultHeaders(http.Header{"X-Custom": []string{"default"}})
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	prepHeaders := http.Header{}
	prepHeaders.Set("X-Custom", "per-request")
	prep := providers.PreparedRequest{Body: []byte(`{"model":"m","messages":[]}`), Headers: prepHeaders}

	require.NoError(t, c.Proxy(context.Background(), router.Decision{Model: "m"}, prep, rec, clientReq))
	assert.Equal(t, "per-request", gotHeader)
}

// TestProxyAndPassthrough_ProtectedHeadersOverridePreparedHeaders ensures a
// provider-mandated header cannot be downgraded by translated request headers
// on either dispatch path.
func TestProxyAndPassthrough_ProtectedHeadersOverridePreparedHeaders(t *testing.T) {
	for _, routed := range []bool{true, false} {
		name := "passthrough"
		if routed {
			name = "proxy"
		}
		t.Run(name, func(t *testing.T) {
			var gotZDR string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotZDR = r.Header.Get("Wafer-ZDR")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"chatcmpl-zdr","object":"chat.completion"}`))
			}))
			defer upstream.Close()

			c := openaicompat.NewClient("test-key", upstream.URL+"/v1").
				WithProtectedHeaders(http.Header{"Wafer-ZDR": []string{"required"}})
			rec := httptest.NewRecorder()
			path := "/v1/models"
			prep := providers.PreparedRequest{Headers: make(http.Header)}
			if routed {
				path = "/v1/chat/completions"
				prep.Body = []byte(`{"model":"m","messages":[]}`)
			} else {
				prep.Body = []byte(`{"object":"list","data":[]}`)
			}
			prep.Headers.Set("Wafer-ZDR", "disabled")
			clientReq := httptest.NewRequest(http.MethodPost, path, strings.NewReader(""))
			if !routed {
				clientReq = httptest.NewRequest(http.MethodGet, path, nil)
			}

			var err error
			if routed {
				err = c.Proxy(context.Background(), router.Decision{Model: "m"}, prep, rec, clientReq)
			} else {
				err = c.Passthrough(context.Background(), prep, rec, clientReq)
			}

			require.NoError(t, err)
			assert.Equal(t, "required", gotZDR)
		})
	}
}
