package openaicompat_test

// Guards the OUTPUT-progress watchdog on the generic OpenAI-compatible adapter
// (OpenRouter / Fireworks / DeepInfra / Bedrock). Prod incident 2026-06-19: a
// DeepInfra deepseek-v4-flash stream stayed byte-alive (SSE keepalive / empty
// frames kept the byte-idle watchdog reset) yet produced ZERO output content for
// ~10min until the router's 600s request cap; the client then retried and hit a
// model-not-found 404. The byte-idle watchdog cannot catch that — only a
// watchdog that measures time-since-last-OUTPUT can. The OpenAI→Anthropic
// translator reports output progress via ArmOutputProgress; these tests stand in
// a fake writer for it so the client half is pinned independently: a
// byte-alive/output-silent stream aborts at the output-stall budget with a
// retryable ErrUpstreamOutputStall, while a stream whose writer keeps marking
// output progress is never aborted.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workweave/router/internal/providers"
	"workweave/router/internal/providers/httputil"
	"workweave/router/internal/providers/openaicompat"
	"workweave/router/internal/router"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeProgressWriter stands in for the OpenAI→Anthropic translator: it exposes
// ArmOutputProgress so the client wires its output-progress watchdog, and
// optionally calls the mark on each Write to simulate output flowing.
type fakeProgressWriter struct {
	mu          sync.Mutex
	hdr         http.Header
	bytesIn     int
	mark        func()
	armReturns  bool
	markOnWrite bool
}

var _ providers.OutputProgressArmer = (*fakeProgressWriter)(nil)

func (f *fakeProgressWriter) Header() http.Header {
	if f.hdr == nil {
		f.hdr = make(http.Header)
	}
	return f.hdr
}

func (f *fakeProgressWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	f.bytesIn += len(p)
	mark := f.mark
	markOnWrite := f.markOnWrite
	f.mu.Unlock()
	if markOnWrite && mark != nil {
		mark()
	}
	return len(p), nil
}

func (f *fakeProgressWriter) WriteHeader(int) {}
func (f *fakeProgressWriter) Flush()          {}

func (f *fakeProgressWriter) ArmOutputProgress(mark func()) bool {
	f.mu.Lock()
	f.mark = mark
	f.mu.Unlock()
	return f.armReturns
}

// streamsKeepalivesForever commits 200 + SSE headers then emits a keepalive
// comment every interval until the client disconnects. Bytes keep flowing (the
// byte-idle watchdog never trips); none carries output, so only the
// output-progress watchdog can end it. This is the 2026-06-19 DeepInfra mode.
func streamsKeepalivesForever(interval time.Duration) *httptest.Server {
	const frame = ": OPENROUTER PROCESSING\n\n"
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

func chatPrep() providers.PreparedRequest {
	return providers.PreparedRequest{
		Body:    []byte(`{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`),
		Headers: make(http.Header),
	}
}

func TestProxy_OutputStallAbortsRetryable_ByteAliveNoOutput(t *testing.T) {
	upstream := streamsKeepalivesForever(15 * time.Millisecond)
	defer upstream.Close()

	// Byte-idle far longer than the keepalive interval (so it never fires);
	// output-stall short (so it fires): bytes flowing, output silent.
	const byteIdle = 5 * time.Second
	const outputStall = 120 * time.Millisecond
	c := openaicompat.NewClientWithStallTimeouts("test-key", upstream.URL, byteIdle, outputStall)
	w := &fakeProgressWriter{armReturns: true} // streaming translator; never marks output
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))

	start := time.Now()
	err := c.Proxy(context.Background(), router.Decision{Model: "deepseek/deepseek-v4-flash"}, chatPrep(), w, clientReq)
	elapsed := time.Since(start)

	require.Error(t, err, "a byte-alive/output-silent stream must surface an error, not hang")
	assert.ErrorIs(t, err, httputil.ErrUpstreamOutputStall)
	assert.NotErrorIs(t, err, httputil.ErrUpstreamIdleTimeout,
		"the byte-idle watchdog must not be what fired — bytes were flowing the whole time")
	assert.True(t, providers.IsRetryable(err),
		"the output stall must classify retryable so dispatchWithFallback can re-attempt")
	assert.GreaterOrEqual(t, elapsed, outputStall, "must not abort before the output-stall budget")
	assert.Less(t, elapsed, byteIdle, "must abort at the output-stall budget, not ride to the byte-idle/cap")
	assert.Positive(t, w.bytesIn, "the upstream did keep the stream byte-alive (precondition for this stall mode)")
}

func TestProxy_OutputFlowingStreamIsNotOutputStalled(t *testing.T) {
	// Same byte cadence, but the writer marks output progress on every relayed
	// frame (simulating real content). The output-stall watchdog must keep
	// resetting and never trip, even though total duration exceeds its budget.
	const frame = "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"
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
			_, _ = io.WriteString(w, frame)
			f.Flush()
		}
	}))
	defer upstream.Close()

	const byteIdle = 5 * time.Second
	const outputStall = 60 * time.Millisecond // < frames*frameInterval, so total duration exceeds it
	c := openaicompat.NewClientWithStallTimeouts("test-key", upstream.URL, byteIdle, outputStall)
	w := &fakeProgressWriter{armReturns: true, markOnWrite: true}
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))

	err := c.Proxy(context.Background(), router.Decision{Model: "deepseek/deepseek-v4-flash"}, chatPrep(), w, clientReq)

	require.NoError(t, err, "a stream that keeps producing output must never trip the output-stall watchdog")
	assert.Positive(t, w.bytesIn)
}

// TestProxy_NotArmedWriterStillByteIdleGuarded confirms graceful degradation:
// a writer that does not expose ArmOutputProgress (or returns armed=false) is
// still protected by the byte-idle watchdog. A fully byte-silent stream trips
// it even though no output-progress watchdog was wired.
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
	c := openaicompat.NewClientWithStallTimeouts("test-key", upstream.URL, byteIdle, outputStall)
	w := &fakeProgressWriter{armReturns: false} // declines arming (e.g. non-streaming translator)
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))

	start := time.Now()
	err := c.Proxy(context.Background(), router.Decision{Model: "deepseek/deepseek-v4-flash"}, chatPrep(), w, clientReq)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.ErrorIs(t, err, httputil.ErrUpstreamIdleTimeout)
	assert.GreaterOrEqual(t, elapsed, byteIdle)
	assert.Less(t, elapsed, outputStall, "byte-idle must fire well before the output-stall budget")
}
