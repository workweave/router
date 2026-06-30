package openaicompat_test

// Guards the MINIMUM-THROUGHPUT watchdog on the generic OpenAI-compatible
// adapter. Prod incident 2026-06-25: a deepseek/deepseek-v4-flash stream emitted
// ~1774 output deltas over ~132s (~13 events/s) — a clean, steadily-flowing 200
// that reset BOTH the byte-idle (45s) and output-stall (240s) watchdogs on every
// delta yet was so slow the turn rode toward the 600s request cap with the client
// (Claude Code) frozen, and was never classified retryable. Only a watchdog that
// measures the RATE of output deltas (not time-since-last-output) can catch it.
// The translator reports each output-bearing delta via ArmOutputProgress; these
// tests stand in a fake writer that marks on each relayed frame so the client
// half is pinned independently of the translator: a stream marking far below the
// floor aborts with a retryable ErrUpstreamSlowThroughput, while one marking
// above the floor is never aborted.

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
	"workweave/router/internal/providers/openaicompat"
	"workweave/router/internal/router"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// streamsContentForever commits 200 + SSE headers then emits one output-bearing
// content frame every interval until the client disconnects. The fakeProgressWriter
// (markOnWrite=true) marks output progress on each, so byte-idle and output-stall
// both stay reset — only the throughput watchdog can end it.
func streamsContentForever(interval time.Duration) *httptest.Server {
	const frame = "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		f.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(interval):
			}
			if _, err := io.WriteString(w, frame); err != nil {
				return
			}
			f.Flush()
		}
	}))
}

func TestProxy_SlowThroughputAbortsRetryable(t *testing.T) {
	// One delta every ~40ms = ~25/s. With a tiny warmup and a floor of 100
	// deltas per 60ms window (~1666/s), the stream is steadily alive but far
	// below the floor, so the throughput watchdog must trip — even though
	// neither byte-idle nor output-stall ever would.
	upstream := streamsContentForever(40 * time.Millisecond)
	defer upstream.Close()

	c := openaicompat.NewClientWithThroughputGuard("test-key", upstream.URL,
		60*time.Millisecond /*window*/, 30*time.Millisecond /*minElapsed*/, 100 /*minDeltas*/)
	w := &fakeProgressWriter{armReturns: true, markOnWrite: true}
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))

	start := time.Now()
	err := c.Proxy(context.Background(), router.Decision{Model: "deepseek/deepseek-v4-flash"}, chatPrep(), w, clientReq)
	elapsed := time.Since(start)

	require.Error(t, err, "a sustained sub-throughput stream must surface an error, not hang")
	assert.ErrorIs(t, err, httputil.ErrUpstreamSlowThroughput)
	assert.True(t, providers.IsRetryable(err),
		"the slow-throughput stall must classify retryable so dispatchWithFallback can re-attempt")
	assert.Less(t, elapsed, 5*time.Second, "must abort promptly after warmup, not ride to the cap")
	assert.Positive(t, w.bytesIn, "the upstream did keep the stream alive with output (precondition)")
}

func TestProxy_HealthyThroughputIsNotAborted(t *testing.T) {
	// One delta every ~5ms = ~200/s, comfortably above a floor of 3 deltas per
	// 60ms window (~50/s). The throughput watchdog must never trip; the stream
	// ends on its own when the upstream closes.
	const frame = "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"
	const frames = 80
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		f.Flush()
		for range frames {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
			_, _ = io.WriteString(w, frame)
			f.Flush()
		}
	}))
	defer upstream.Close()

	c := openaicompat.NewClientWithThroughputGuard("test-key", upstream.URL,
		60*time.Millisecond /*window*/, 30*time.Millisecond /*minElapsed*/, 3 /*minDeltas*/)
	w := &fakeProgressWriter{armReturns: true, markOnWrite: true}
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))

	err := c.Proxy(context.Background(), router.Decision{Model: "deepseek/deepseek-v4-flash"}, chatPrep(), w, clientReq)

	require.NoError(t, err, "a stream above the throughput floor must never trip the watchdog")
	assert.Positive(t, w.bytesIn)
}
