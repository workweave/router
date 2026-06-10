package openai_test

// Guards the mid-stream half of the gpt-5.x stall protection (the
// time-to-first-byte half lives in client_timeout_test.go). Prod incident
// 2026-06-09 (customer benchmark org): two /v1/responses streams returned
// headers and then produced ZERO output tokens until the router's 600s cap
// (proxy_ms 599,991 and 599,880, both resp_output_tokens=0). The
// ResponseHeaderTimeout cannot fire once headers have arrived, so the only
// guard against a post-header stall is the idle-progress watchdog. These
// tests pin its contract: a zero-progress stall aborts at the idle threshold,
// the error is retryable (so dispatchWithFallback re-attempts), and no
// response bytes reach the client writer; a slow-but-flowing stream — long
// reasoning turns emit SSE event frames even while "thinking", and ANY bytes
// count as progress — is never aborted; and a stalled >=400 error body cannot
// hang the buffered-error read.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workweave/router/internal/providers"
	"workweave/router/internal/providers/httputil"
	"workweave/router/internal/providers/openai"
	"workweave/router/internal/router"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const stallTestHeaderTimeout = 5 * time.Second

func responsesPrep() providers.PreparedRequest {
	return providers.PreparedRequest{
		Body:     []byte(`{"model":"gpt-5.5","stream":true}`),
		Headers:  make(http.Header),
		Endpoint: providers.EndpointResponses,
	}
}

func TestProxy_MidStreamStallAbortsRetryableNothingWritten(t *testing.T) {
	// The upstream commits 200 + SSE headers, then goes silent forever. The
	// only timer in play is the injected tiny idle threshold; release is
	// closed (defer, LIFO) before upstream.Close() so the handler unblocks
	// even if the watchdog's cancel didn't already end its request context.
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer close(release)

	const idleTimeout = 100 * time.Millisecond
	c := openai.NewClientWithTimeouts("test-key", upstream.URL, stallTestHeaderTimeout, idleTimeout)
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(""))

	start := time.Now()
	err := c.Proxy(context.Background(), router.Decision{Model: "gpt-5.5"}, responsesPrep(), rec, clientReq)
	elapsed := time.Since(start)

	require.Error(t, err, "a post-header zero-progress stall must surface an error, not hang")
	assert.ErrorIs(t, err, httputil.ErrUpstreamIdleTimeout)
	assert.True(t, providers.IsRetryable(err),
		"the stall must classify retryable so dispatchWithFallback can re-attempt the binding")
	assert.GreaterOrEqual(t, elapsed, idleTimeout, "must not abort before the idle threshold")
	assert.Less(t, elapsed, stallTestHeaderTimeout,
		"must abort at the idle threshold — a regression here re-opens the 600s zero-token hang")
	// In the dispatch path the per-attempt writer is the preludeBuffer chain,
	// and failover is only legal while it holds zero committed bytes. A stall
	// with no upstream bytes must therefore leave the writer body empty.
	assert.Zero(t, rec.Body.Len(), "no response bytes may reach the client writer on a zero-byte stall")
}

func TestProxy_SlowButFlowingStreamIsNotAborted(t *testing.T) {
	// Frames arrive slower than a fast stream but well inside the idle
	// budget; the stream's TOTAL duration exceeds the idle budget several
	// times over. Only a zero-progress gap may trip the watchdog — total
	// duration never does (long thinking turns with progress must survive).
	const frame = "event: response.in_progress\ndata: {}\n\n"
	const frames = 5
	const frameInterval = 60 * time.Millisecond
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		f.Flush()
		for range frames {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(frameInterval):
			}
			_, _ = io.WriteString(w, frame)
			f.Flush()
		}
	}))
	defer upstream.Close()

	const idleTimeout = 150 * time.Millisecond
	c := openai.NewClientWithTimeouts("test-key", upstream.URL, stallTestHeaderTimeout, idleTimeout)
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(""))

	err := c.Proxy(context.Background(), router.Decision{Model: "gpt-5.5"}, responsesPrep(), rec, clientReq)

	require.NoError(t, err, "a stream that keeps producing bytes must never trip the idle watchdog")
	assert.Equal(t, strings.Repeat(frame, frames), rec.Body.String(),
		"every upstream frame must be relayed to the client writer")
}

func TestProxy_StalledErrorBodyDoesNotHang(t *testing.T) {
	// The upstream returns 500 headers, promises a body, and never sends it.
	// The buffered-error read must abort on the idle watchdog and still
	// surface the status-classified *UpstreamErrorResponse so the dispatch
	// loop's status-based retry logic applies.
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusInternalServerError)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer close(release)

	const idleTimeout = 100 * time.Millisecond
	c := openai.NewClientWithTimeouts("test-key", upstream.URL, stallTestHeaderTimeout, idleTimeout)
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(""))

	start := time.Now()
	err := c.Proxy(context.Background(), router.Decision{Model: "gpt-5.5"}, responsesPrep(), rec, clientReq)
	elapsed := time.Since(start)

	var upstreamErr *providers.UpstreamErrorResponse
	require.ErrorAs(t, err, &upstreamErr, "a stalled error body must still buffer as *UpstreamErrorResponse")
	assert.Equal(t, http.StatusInternalServerError, upstreamErr.Status)
	assert.True(t, providers.IsRetryable(err), "a buffered 500 stays retryable by status")
	assert.Less(t, elapsed, stallTestHeaderTimeout,
		"the error-body read must abort at the idle threshold, not hang on the missing body")
	assert.Zero(t, rec.Body.Len(), "buffered upstream errors must not touch the client writer")
}
