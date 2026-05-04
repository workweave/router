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

func TestProxy_ReturnsUpstreamStatusErrorOn4xx(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"adaptive thinking is not supported on this model"}}`))
	}))
	defer upstream.Close()

	c := anthropic.NewClient("k", upstream.URL)
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	prep := providers.PreparedRequest{Body: []byte(`{"model":"x"}`), Headers: make(http.Header)}
	err := c.Proxy(context.Background(), router.Decision{Model: "claude-haiku-4-5"}, prep, rec, clientReq)

	var statusErr *providers.UpstreamStatusError
	require.ErrorAs(t, err, &statusErr,
		"upstream 4xx must surface as a typed *providers.UpstreamStatusError "+
			"so proxy.Service can log upstream_status without changing the "+
			"transparent pass-through of the error envelope")
	assert.Equal(t, http.StatusBadRequest, statusErr.Status)
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"upstream status must still be teed through to the client unchanged")
	assert.Contains(t, rec.Body.String(), "adaptive thinking is not supported",
		"upstream error envelope must still be teed through verbatim")
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

