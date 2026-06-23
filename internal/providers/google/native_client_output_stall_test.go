package google_test

// Guards the OUTPUT-progress watchdog on the native Gemini client. The byte-idle
// watchdog (DefaultSSEIdleTimeout) resets on ANY upstream byte, so a Gemini
// stream that stays byte-alive with SSE keepalive frames while producing zero
// output content would ride to the request cap. The GeminiToOpenAISSETranslator
// reports output progress via ArmOutputProgress; these tests stand in a fake
// writer for it so the client half is pinned independently: a
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
	"workweave/router/internal/providers/google"
	"workweave/router/internal/providers/httputil"
	"workweave/router/internal/router"
	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeProgressWriter stands in for the GeminiToOpenAISSETranslator: it exposes
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
// output-progress watchdog can end it.
func streamsKeepalivesForever(interval time.Duration) *httptest.Server {
	const frame = ": keepalive\n\n"
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

func geminiStreamPrep() providers.PreparedRequest {
	h := make(http.Header)
	h.Set(translate.GeminiStreamHintHeader, "true")
	return providers.PreparedRequest{
		Body:    []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`),
		Headers: h,
	}
}

func TestNativeProxy_OutputStallAbortsRetryable_ByteAliveNoOutput(t *testing.T) {
	upstream := streamsKeepalivesForever(15 * time.Millisecond)
	defer upstream.Close()

	const byteIdle = 5 * time.Second
	const outputStall = 120 * time.Millisecond
	c := google.NewNativeClientWithStallTimeouts("test-key", upstream.URL, byteIdle, outputStall)
	w := &fakeProgressWriter{armReturns: true} // streaming translator; never marks output
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	start := time.Now()
	err := c.Proxy(context.Background(), router.Decision{Model: "gemini-3.1-flash-lite-preview"}, geminiStreamPrep(), w, clientReq)
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

func TestNativeProxy_OutputFlowingStreamIsNotOutputStalled(t *testing.T) {
	const frame = "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"x\"}]}}]}\n\n"
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
	c := google.NewNativeClientWithStallTimeouts("test-key", upstream.URL, byteIdle, outputStall)
	w := &fakeProgressWriter{armReturns: true, markOnWrite: true}
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	err := c.Proxy(context.Background(), router.Decision{Model: "gemini-3.1-flash-lite-preview"}, geminiStreamPrep(), w, clientReq)

	require.NoError(t, err, "a stream that keeps producing output must never trip the output-stall watchdog")
	assert.Positive(t, w.bytesIn)
}

// TestNativeProxy_NotArmedWriterStillByteIdleGuarded confirms graceful
// degradation: a writer that returns armed=false is still protected by the
// byte-idle watchdog. A fully byte-silent stream trips it even though no
// output-progress watchdog was wired.
func TestNativeProxy_NotArmedWriterStillByteIdleGuarded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // headers sent, then zero bytes forever
	}))
	defer upstream.Close()

	const byteIdle = 80 * time.Millisecond
	const outputStall = 5 * time.Second
	c := google.NewNativeClientWithStallTimeouts("test-key", upstream.URL, byteIdle, outputStall)
	w := &fakeProgressWriter{armReturns: false}
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	start := time.Now()
	err := c.Proxy(context.Background(), router.Decision{Model: "gemini-3.1-flash-lite-preview"}, geminiStreamPrep(), w, clientReq)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.ErrorIs(t, err, httputil.ErrUpstreamIdleTimeout)
	assert.GreaterOrEqual(t, elapsed, byteIdle)
	assert.Less(t, elapsed, outputStall, "byte-idle must fire well before the output-stall budget")
}
