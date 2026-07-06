package anthropic_test

// Guards the output-progress watchdog on the native Anthropic adapter: Anthropic
// ping keepalives reset the byte-idle watchdog, so only an output-bearing
// watchdog ends a ping-alive/zero-output stream.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workweave/router/internal/providers"
	"workweave/router/internal/providers/anthropic"
	"workweave/router/internal/providers/httputil"
	"workweave/router/internal/router"
	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flushRecorder is a minimal streaming ResponseWriter without ArmOutputProgress.
type flushRecorder struct {
	hdr http.Header
	n   int
}

func (r *flushRecorder) Header() http.Header {
	if r.hdr == nil {
		r.hdr = make(http.Header)
	}
	return r.hdr
}
func (r *flushRecorder) Write(p []byte) (int, error) { r.n += len(p); return len(p), nil }
func (r *flushRecorder) WriteHeader(int)             {}
func (r *flushRecorder) Flush()                      {}

// streamsFramesForever emits frame every interval until the client disconnects,
// keeping the stream byte-alive so the byte-idle watchdog never trips.
func streamsFramesForever(frame string, interval time.Duration) *httptest.Server {
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

func messagesPrep() providers.PreparedRequest {
	return providers.PreparedRequest{
		Body:    []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}],"stream":true}`),
		Headers: make(http.Header),
	}
}

const pingFrame = "event: ping\ndata: {\"type\":\"ping\"}\n\n"
const textDeltaFrame = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n"

func TestProxy_OutputStallAbortsOnPingOnlyStream(t *testing.T) {
	upstream := streamsFramesForever(pingFrame, 15*time.Millisecond)
	defer upstream.Close()

	// Byte-idle far longer than the ping interval (so it never fires); output-stall
	// short (so it fires): pings flowing, zero output.
	const byteIdle = 5 * time.Second
	const outputStall = 120 * time.Millisecond
	c := anthropic.NewClientWithStallTimeouts("test-key", upstream.URL, byteIdle, outputStall)
	w := translate.NewAnthropicRoutingMarkerWriter(&flushRecorder{}, "claude-sonnet-5", "[routed]")
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	start := time.Now()
	err := c.Proxy(context.Background(), router.Decision{Model: "claude-sonnet-5"}, messagesPrep(), w, clientReq)
	elapsed := time.Since(start)

	require.Error(t, err, "a ping-alive/output-silent stream must surface an error, not hang")
	assert.ErrorIs(t, err, httputil.ErrUpstreamOutputStall)
	assert.NotErrorIs(t, err, httputil.ErrUpstreamIdleTimeout,
		"the byte-idle watchdog must not be what fired — ping bytes were flowing the whole time")
	assert.True(t, providers.IsRetryable(err),
		"the output stall must classify retryable so dispatchWithFallback can re-attempt")
	assert.GreaterOrEqual(t, elapsed, outputStall, "must not abort before the output-stall budget")
	assert.Less(t, elapsed, byteIdle, "must abort at the output-stall budget, not ride to the byte-idle/cap")
}

func TestProxy_ContentDeltasResetOutputStall(t *testing.T) {
	// content_block_delta marks each reset; the stream outlives the budget.
	const frames = 6
	const frameInterval = 20 * time.Millisecond
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
			_, _ = io.WriteString(w, textDeltaFrame)
			f.Flush()
		}
	}))
	defer upstream.Close()

	const byteIdle = 5 * time.Second
	const outputStall = 60 * time.Millisecond // < frames*frameInterval, so total duration exceeds it
	c := anthropic.NewClientWithStallTimeouts("test-key", upstream.URL, byteIdle, outputStall)
	w := translate.NewAnthropicRoutingMarkerWriter(&flushRecorder{}, "claude-sonnet-5", "[routed]")
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	err := c.Proxy(context.Background(), router.Decision{Model: "claude-sonnet-5"}, messagesPrep(), w, clientReq)
	require.NoError(t, err, "a stream producing content_block_delta output must never trip the output-stall watchdog")
}

// A bare writer (no ArmOutputProgress, e.g. marker-suppressed passthrough) is
// still protected by the byte-idle watchdog: a fully byte-silent stream trips it
// even though no output-progress watchdog was wired.
func TestProxy_NotArmedWriterStillByteIdleGuarded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // headers sent, then zero bytes forever
	}))
	defer upstream.Close()

	const byteIdle = 80 * time.Millisecond
	const outputStall = 5 * time.Second
	c := anthropic.NewClientWithStallTimeouts("test-key", upstream.URL, byteIdle, outputStall)
	w := &flushRecorder{} // no ArmOutputProgress hook
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	start := time.Now()
	err := c.Proxy(context.Background(), router.Decision{Model: "claude-sonnet-5"}, messagesPrep(), w, clientReq)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.ErrorIs(t, err, httputil.ErrUpstreamIdleTimeout)
	assert.GreaterOrEqual(t, elapsed, byteIdle)
	assert.Less(t, elapsed, outputStall, "byte-idle must fire well before the output-stall budget")
}
