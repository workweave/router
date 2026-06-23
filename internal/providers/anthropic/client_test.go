package anthropic_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workweave/router/internal/observability/otel"
	"workweave/router/internal/providers"
	"workweave/router/internal/providers/anthropic"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProxy_RewritesModelAndForwardsAuth(t *testing.T) {
	var (
		gotPath    string
		gotAPIKey  string
		gotVersion string
		gotBody    map[string]any
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		body, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(body, &gotBody))
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1"}`))
	}))
	defer upstream.Close()

	c := anthropic.NewClient("test-key", upstream.URL)
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)
	headers := make(http.Header)
	headers.Set("anthropic-version", "2024-10-22")
	prep := providers.PreparedRequest{Body: body, Headers: headers}

	err := c.Proxy(context.Background(), router.Decision{Model: "claude-haiku-4-5"}, prep, rec, clientReq)

	require.NoError(t, err)
	assert.Equal(t, "/v1/messages", gotPath)
	assert.Equal(t, "test-key", gotAPIKey)
	assert.Equal(t, "2024-10-22", gotVersion)
	assert.Equal(t, "claude-opus-4-7", gotBody["model"],
		"Proxy must send body verbatim — model rewriting is the envelope's job")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"id":"msg_1"`)
}

func TestProxy_PassesThroughAnthropicAuthWithoutRouterKey(t *testing.T) {
	var (
		gotAuth      string
		gotAPIKey    string
		gotRouterKey string
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		gotRouterKey = r.Header.Get("X-Weave-Router-Key")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1"}`))
	}))
	defer upstream.Close()

	c := anthropic.NewClient("", upstream.URL)
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	clientReq.Header.Set("Authorization", "Bearer anthropic-oauth-token")
	clientReq.Header.Set("X-Weave-Router-Key", "rk_router")

	prep := providers.PreparedRequest{
		Body:    []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`),
		Headers: make(http.Header),
	}
	err := c.Proxy(context.Background(), router.Decision{Model: "claude-haiku-4-5"}, prep, rec, clientReq)

	require.NoError(t, err)
	assert.Equal(t, "Bearer anthropic-oauth-token", gotAuth)
	assert.Empty(t, gotAPIKey)
	assert.Empty(t, gotRouterKey, "router auth must not be forwarded to Anthropic")
}

func TestProxy_RouterKeyDoesNotOverrideConfiguredAnthropicKey(t *testing.T) {
	var (
		gotAuth   string
		gotAPIKey string
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1"}`))
	}))
	defer upstream.Close()

	c := anthropic.NewClient("router-anthropic-key", upstream.URL)
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	clientReq.Header.Set("Authorization", "Bearer anthropic-oauth-token")
	clientReq.Header.Set("x-api-key", "anthropic-api-key")
	clientReq.Header.Set("X-Weave-Router-Key", "rk_router")

	prep := providers.PreparedRequest{
		Body:    []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`),
		Headers: make(http.Header),
	}
	err := c.Proxy(context.Background(), router.Decision{Model: "claude-haiku-4-5"}, prep, rec, clientReq)

	require.NoError(t, err)
	assert.Empty(t, gotAuth, "configured router Anthropic key should own upstream auth")
	assert.Equal(t, "router-anthropic-key", gotAPIKey)
}

func TestProxy_SubscriptionOAuthCredentialUsesBearerAndBetaHeader(t *testing.T) {
	var (
		gotAuth   string
		gotAPIKey string
		gotBeta   string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		gotBeta = r.Header.Get("anthropic-beta")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1"}`))
	}))
	defer upstream.Close()

	// Deployment key is configured, but a resolved subscription OAuth credential
	// must win and authenticate via Bearer — never x-api-key.
	c := anthropic.NewClient("deployment-key", upstream.URL)
	ctx := context.WithValue(context.Background(), proxy.CredentialsContextKey{},
		&proxy.Credentials{APIKey: []byte("sk-ant-oat01-subscription-token"), Source: "subscription", OAuth: true})
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	// Pre-seed a model-capability-filtered beta token to prove it is preserved,
	// not clobbered, when the oauth flag is merged in.
	headers := make(http.Header)
	headers.Set("anthropic-beta", "fine-grained-tool-streaming-2025-05-14")
	prep := providers.PreparedRequest{Body: []byte(`{"model":"x"}`), Headers: headers}

	err := c.Proxy(ctx, router.Decision{Model: "claude-opus-4-8"}, prep, rec, clientReq)
	require.NoError(t, err)

	assert.Equal(t, "Bearer sk-ant-oat01-subscription-token", gotAuth)
	assert.Empty(t, gotAPIKey, "a subscription OAuth credential must NOT send x-api-key (would 401)")
	assert.Contains(t, gotBeta, "oauth-2025-04-20", "subscription tokens require the oauth beta flag on /v1/messages")
	assert.Contains(t, gotBeta, "fine-grained-tool-streaming-2025-05-14",
		"merging the oauth flag must preserve the model-capability-filtered beta tokens")
}

func TestProxy_NonOAuthCredentialUsesAPIKey(t *testing.T) {
	var (
		gotAuth   string
		gotAPIKey string
		gotBeta   string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		gotBeta = r.Header.Get("anthropic-beta")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1"}`))
	}))
	defer upstream.Close()

	c := anthropic.NewClient("deployment-key", upstream.URL)
	ctx := context.WithValue(context.Background(), proxy.CredentialsContextKey{},
		&proxy.Credentials{APIKey: []byte("sk-ant-api-byok"), Source: "byok"})
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	prep := providers.PreparedRequest{Body: []byte(`{"model":"x"}`), Headers: make(http.Header)}

	err := c.Proxy(ctx, router.Decision{Model: "claude-opus-4-8"}, prep, rec, clientReq)
	require.NoError(t, err)

	assert.Equal(t, "sk-ant-api-byok", gotAPIKey)
	assert.Empty(t, gotAuth, "a non-OAuth credential authenticates via x-api-key, not Authorization")
	assert.NotContains(t, gotBeta, "oauth-2025-04-20", "the oauth beta flag must only be added for subscription tokens")
}

func TestProxy_PassesThroughInboundSubscriptionWithBetaHeader(t *testing.T) {
	var (
		gotAuth string
		gotBeta string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotBeta = r.Header.Get("anthropic-beta")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1"}`))
	}))
	defer upstream.Close()

	// Self-hosted pure passthrough: no deployment key, no resolved credential —
	// the caller's own subscription bearer rides the inbound Authorization and
	// must still get the oauth beta flag.
	c := anthropic.NewClient("", upstream.URL)
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	clientReq.Header.Set("Authorization", "Bearer sk-ant-oat01-subscription-token")
	prep := providers.PreparedRequest{Body: []byte(`{"model":"x"}`), Headers: make(http.Header)}

	err := c.Proxy(context.Background(), router.Decision{Model: "claude-opus-4-8"}, prep, rec, clientReq)
	require.NoError(t, err)

	assert.Equal(t, "Bearer sk-ant-oat01-subscription-token", gotAuth, "inbound subscription bearer must pass through verbatim")
	assert.Contains(t, gotBeta, "oauth-2025-04-20", "a passed-through subscription bearer must still get the oauth beta flag")
}

func TestProxy_BuffersUpstream4xx(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Custom-Upstream", "abc")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"adaptive thinking is not supported on this model"}}`))
	}))
	defer upstream.Close()

	c := anthropic.NewClient("k", upstream.URL)
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	prep := providers.PreparedRequest{Body: []byte(`{"model":"x"}`), Headers: make(http.Header)}
	err := c.Proxy(context.Background(), router.Decision{Model: "claude-haiku-4-5"}, prep, rec, clientReq)

	var buffered *providers.UpstreamErrorResponse
	require.ErrorAs(t, err, &buffered,
		"non-2xx upstream responses must be buffered so proxy.Service can discard marker Prelude bytes and retry or flush cleanly")
	assert.Equal(t, http.StatusBadRequest, buffered.Status)
	assert.Equal(t, "abc", buffered.Headers.Get("X-Custom-Upstream"))
	assert.Contains(t, string(buffered.Body), "adaptive thinking is not supported")
	assert.Empty(t, rec.Body.String(), "buffered upstream errors must not write through the response writer")
}

func TestProxy_BuffersRetryable429(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`))
	}))
	defer upstream.Close()

	c := anthropic.NewClient("k", upstream.URL)
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	prep := providers.PreparedRequest{Body: []byte(`{"model":"x"}`), Headers: make(http.Header)}
	err := c.Proxy(context.Background(), router.Decision{Model: "claude-haiku-4-5"}, prep, rec, clientReq)

	var buffered *providers.UpstreamErrorResponse
	require.ErrorAs(t, err, &buffered)
	assert.Equal(t, http.StatusTooManyRequests, buffered.Status)
	assert.True(t, providers.IsRetryable(err), "buffered 429 must remain eligible for clean pre-commit retry")
	assert.Empty(t, rec.Body.String(), "retryable upstream errors must not commit response bytes")
}

func TestProxy_StripsDynamicHopByHopHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "Keep-Alive, X-Internal-Trace")
		w.Header().Set("X-Internal-Trace", "abc123")
		w.Header().Set("X-Public-Header", "ok")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	c := anthropic.NewClient("k", upstream.URL)
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	prep := providers.PreparedRequest{Body: []byte(`{"model":"x"}`), Headers: make(http.Header)}
	err := c.Proxy(context.Background(), router.Decision{Model: "m"}, prep, rec, clientReq)
	require.NoError(t, err)

	assert.Empty(t, rec.Header().Get("X-Internal-Trace"), "headers named in upstream Connection list must be stripped")
	assert.Empty(t, rec.Header().Get("Keep-Alive"), "static hop-by-hop headers must remain stripped")
	assert.Equal(t, "ok", rec.Header().Get("X-Public-Header"))
}

type flushSpyWriter struct {
	*httptest.ResponseRecorder
	flushes int
}

func (f *flushSpyWriter) Flush() { f.flushes++; f.ResponseRecorder.Flush() }

func TestProxy_StreamsAndFlushes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		require.True(t, ok)
		for i := 0; i < 3; i++ {
			_, _ = w.Write([]byte("event: ping\ndata: {}\n\n"))
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	c := anthropic.NewClient("k", upstream.URL)
	rec := &flushSpyWriter{ResponseRecorder: httptest.NewRecorder()}
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	prep := providers.PreparedRequest{Body: []byte(`{"model":"x"}`), Headers: make(http.Header)}
	err := c.Proxy(context.Background(), router.Decision{Model: "claude-haiku-4-5"}, prep, rec, clientReq)

	require.NoError(t, err)
	assert.GreaterOrEqual(t, rec.flushes, 1, "Proxy must flush at least once mid-stream")
	assert.Equal(t, "text/event-stream", rec.Header().Get("content-type"))
}

func TestProxy_StampsTimingMilestones(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message_start\ndata: {}\n\n"))
	}))
	defer upstream.Close()

	c := anthropic.NewClient("k", upstream.URL)
	ctx, tm := otel.WithTiming(context.Background())
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	prep := providers.PreparedRequest{Body: []byte(`{"model":"x"}`), Headers: make(http.Header)}
	err := c.Proxy(ctx, router.Decision{Model: "m"}, prep, rec, clientReq)
	require.NoError(t, err)

	assert.NotZero(t, tm.UpstreamRequestNanos.Load(), "UpstreamRequestNanos must be stamped before http.Do")
	assert.NotZero(t, tm.UpstreamHeadersNanos.Load(), "UpstreamHeadersNanos must be stamped after http.Do returns")
	assert.NotZero(t, tm.UpstreamFirstByteNanos.Load(), "UpstreamFirstByteNanos must be stamped on first body byte")
	assert.NotZero(t, tm.UpstreamEOFNanos.Load(), "UpstreamEOFNanos must be stamped on EOF")
	assert.LessOrEqual(t, tm.UpstreamRequestNanos.Load(), tm.UpstreamHeadersNanos.Load())
	assert.LessOrEqual(t, tm.UpstreamHeadersNanos.Load(), tm.UpstreamFirstByteNanos.Load())
	assert.LessOrEqual(t, tm.UpstreamFirstByteNanos.Load(), tm.UpstreamEOFNanos.Load())
}

func TestProxy_StampsTimingOnError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad"}`))
	}))
	defer upstream.Close()

	c := anthropic.NewClient("k", upstream.URL)
	ctx, tm := otel.WithTiming(context.Background())
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	prep := providers.PreparedRequest{Body: []byte(`{"model":"x"}`), Headers: make(http.Header)}
	_ = c.Proxy(ctx, router.Decision{Model: "m"}, prep, rec, clientReq)

	assert.NotZero(t, tm.UpstreamRequestNanos.Load(), "must stamp UpstreamRequestNanos even on error path")
	assert.NotZero(t, tm.UpstreamHeadersNanos.Load(), "must stamp UpstreamHeadersNanos even on error path")
	assert.NotZero(t, tm.UpstreamFirstByteNanos.Load(), "must stamp UpstreamFirstByteNanos on error body read")
	assert.NotZero(t, tm.UpstreamEOFNanos.Load(), "must stamp UpstreamEOFNanos after error body is drained")
}
