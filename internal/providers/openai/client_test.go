package openai_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/providers/openai"
	"workweave/router/internal/router"
	"workweave/router/internal/timing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProxy_RewritesModelAndForwardsAuth(t *testing.T) {
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

	c := openai.NewClient("test-key", upstream.URL)
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))

	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	prep := providers.PreparedRequest{Body: body, Headers: make(http.Header)}
	err := c.Proxy(context.Background(), router.Decision{Model: "gpt-4o-mini"}, prep, rec, clientReq)

	require.NoError(t, err)
	assert.Equal(t, "/v1/chat/completions", gotPath)
	assert.Equal(t, "Bearer test-key", gotAuth)
	assert.Equal(t, "gpt-4o-mini", gotBody["model"],
		"Proxy must send body verbatim — model rewriting is the envelope's job")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"id":"chatcmpl-1"`)
}

func TestProxy_StripsDynamicHopByHopHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "Keep-Alive, X-Internal-Trace")
		w.Header().Set("X-Internal-Trace", "abc123")
		w.Header().Set("X-Public-Header", "ok")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	c := openai.NewClient("k", upstream.URL)
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))

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
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		require.True(t, ok)
		for i := 0; i < 3; i++ {
			_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-1\"}\n\n"))
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	c := openai.NewClient("k", upstream.URL)
	rec := &flushSpyWriter{ResponseRecorder: httptest.NewRecorder()}
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))

	prep := providers.PreparedRequest{Body: []byte(`{"model":"x"}`), Headers: make(http.Header)}
	err := c.Proxy(context.Background(), router.Decision{Model: "gpt-4o-mini"}, prep, rec, clientReq)

	require.NoError(t, err)
	assert.GreaterOrEqual(t, rec.flushes, 1, "Proxy must flush at least once mid-stream")
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
}

func TestProxy_StampsTimingMilestones(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-1\"}\n\n"))
	}))
	defer upstream.Close()

	c := openai.NewClient("k", upstream.URL)
	ctx, tm := timing.WithTiming(context.Background())
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))

	prep := providers.PreparedRequest{Body: []byte(`{"model":"x"}`), Headers: make(http.Header)}
	err := c.Proxy(ctx, router.Decision{Model: "m"}, prep, rec, clientReq)
	require.NoError(t, err)

	assert.NotZero(t, tm.UpstreamRequestNanos.Load(), "UpstreamRequestNanos must be stamped before http.Do")
	assert.NotZero(t, tm.UpstreamHeadersNanos.Load(), "UpstreamHeadersNanos must be stamped after http.Do returns")
	assert.NotZero(t, tm.UpstreamFirstByteNanos.Load(), "UpstreamFirstByteNanos must be stamped on first body byte")
	assert.NotZero(t, tm.UpstreamEOFNanos.Load(), "UpstreamEOFNanos must be stamped on EOF")
	assert.LessOrEqual(t, tm.UpstreamRequestNanos.Load(), tm.UpstreamHeadersNanos.Load())
	assert.LessOrEqual(t, tm.UpstreamFirstByteNanos.Load(), tm.UpstreamEOFNanos.Load())
}

// TestProxy_BuffersUpstream4xx pins the fix for the gpt-5.5 wedge
// (session 93e918bf): a 4xx from the OpenAI upstream must come back as
// *providers.UpstreamErrorResponse with the upstream body buffered, so
// the dispatch loop can render it in the inbound wire format (or fail
// over on a multi-binding model). The prior behavior wrote the upstream
// 4xx straight through, corrupting the SSE stream when the translator's
// Prelude had already buffered HTTP 200 + message_start, and surfaced
// only an opaque *UpstreamStatusError to the proxy — failover never
// fired and the body_preview log site never executed.
func TestProxy_BuffersUpstream4xx(t *testing.T) {
	const upstreamBody = `{"error":{"message":"max_completion_tokens exceeds model limit","type":"invalid_request_error"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Custom-Upstream", "abc")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer upstream.Close()

	c := openai.NewClient("k", upstream.URL)
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))

	prep := providers.PreparedRequest{Body: []byte(`{"model":"gpt-5.5"}`), Headers: make(http.Header)}
	err := c.Proxy(context.Background(), router.Decision{Model: "gpt-5.5"}, prep, rec, clientReq)
	require.Error(t, err)

	var buffered *providers.UpstreamErrorResponse
	require.ErrorAs(t, err, &buffered,
		"non-2xx upstream must surface as *UpstreamErrorResponse so dispatch can fail over / render in-format")
	assert.Equal(t, http.StatusBadRequest, buffered.Status)
	assert.Equal(t, upstreamBody, string(buffered.Body),
		"buffered body must round-trip the upstream error envelope verbatim")
	assert.Equal(t, "abc", buffered.Headers.Get("X-Custom-Upstream"),
		"adapter must capture upstream headers so dispatch can preserve them when flushing")
	assert.Equal(t, http.StatusOK, rec.Code,
		"adapter must NOT touch the response writer on 4xx; the dispatch loop decides what the client sees")
	assert.Empty(t, rec.Body.String(),
		"writing the upstream body directly corrupts the SSE stream when the prelude is buffered")
}

// TestProxy_5xxIsBufferedToo guards that 5xx follows the same buffered
// path as 4xx; on multi-binding models the dispatch loop's IsRetryable
// check needs the typed UpstreamErrorResponse to fail over to OpenRouter.
func TestProxy_5xxIsBufferedToo(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`upstream bad gateway`))
	}))
	defer upstream.Close()

	c := openai.NewClient("k", upstream.URL)
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))

	prep := providers.PreparedRequest{Body: []byte(`{"model":"x"}`), Headers: make(http.Header)}
	err := c.Proxy(context.Background(), router.Decision{Model: "m"}, prep, rec, clientReq)

	var buffered *providers.UpstreamErrorResponse
	require.ErrorAs(t, err, &buffered)
	assert.Equal(t, http.StatusBadGateway, buffered.Status)
	assert.True(t, providers.IsRetryable(err),
		"5xx must remain retryable so multi-binding models can fail over")
}

func TestPassthrough_ForwardsPathAndAuth(t *testing.T) {
	var (
		gotPath string
		gotAuth string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer upstream.Close()

	c := openai.NewClient("test-key", upstream.URL)
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)

	prep := providers.PreparedRequest{Body: nil, Headers: make(http.Header)}
	err := c.Passthrough(context.Background(), prep, rec, clientReq)
	require.NoError(t, err)

	assert.Equal(t, "/v1/models", gotPath)
	assert.Equal(t, "Bearer test-key", gotAuth)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"object":"list"`)
}
