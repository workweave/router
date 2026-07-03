package openai_test

// Guards the mid-stream half of gpt-5.x stall protection (TTFB half is in
// client_timeout_test.go). Prod incident 2026-06-09: /v1/responses streams
// returned headers then produced ZERO output tokens until the 600s cap.
// ResponseHeaderTimeout can't fire post-header, so the idle-progress
// watchdog is the only guard. Pins: zero-progress stalls abort + retryable +
// no bytes written; flowing streams (any bytes = progress) are never
// aborted; a stalled error body can't hang the buffered-error read.

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
	// Upstream sends headers then goes silent forever. release closes
	// (defer, LIFO) before upstream.Close() so the handler always unblocks.
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
	// Failover is only legal while the preludeBuffer chain holds zero
	// committed bytes, so a zero-byte stall must leave the writer empty.
	assert.Zero(t, rec.Body.Len(), "no response bytes may reach the client writer on a zero-byte stall")
}

func TestProxy_SlowButFlowingStreamIsNotAborted(t *testing.T) {
	// Frames arrive slower than a fast stream but each gap is inside the
	// idle budget, even though total duration exceeds it — only a
	// zero-progress gap may trip the watchdog, never total duration.
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
	// Upstream sends 500 headers, promises a body, never sends it. The
	// buffered-error read must abort on the idle watchdog yet still surface
	// *UpstreamErrorResponse for the dispatch loop's status-based retry.
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
