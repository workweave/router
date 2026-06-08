// Package httputil provides shared HTTP transport and streaming helpers for provider adapters.
package httputil

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"workweave/router/internal/observability/otel"
	"workweave/router/internal/providers"
)

// FlushChunk is the read buffer size used by all streaming provider adapters.
const FlushChunk = 4 * 1024

// ErrUpstreamIdleTimeout is the sentinel cause set on the request context when
// the SSE inactivity watchdog fires. Provider adapters can check
// errors.Is(err, httputil.ErrUpstreamIdleTimeout) (or context.Cause(ctx)) to
// distinguish a real upstream stall from caller-initiated cancellation, and
// the proxy's failover logic keys on this to retry against the next binding.
var ErrUpstreamIdleTimeout = errors.New("upstream sse idle timeout")

// DefaultSSEIdleTimeout is the per-read inactivity threshold for streaming
// upstream responses. AWS NAT Gateway / NLB / VPC-endpoint reapers silently
// drop idle TCP connections at 350s; a watchdog below that surfaces stalls
// as a clean error within tens of seconds instead of letting the request
// hang for the full overall deadline.
//
// Tunable via ROUTER_SSE_IDLE_TIMEOUT_SECONDS. Healthy generations stream a
// chunk at least every few seconds, so 45s of silence is unambiguously a stall.
var DefaultSSEIdleTimeout = sseIdleTimeoutFromEnv()

func sseIdleTimeoutFromEnv() time.Duration {
	v := os.Getenv("ROUTER_SSE_IDLE_TIMEOUT_SECONDS")
	if v == "" {
		return 45 * time.Second
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 45 * time.Second
	}
	return time.Duration(n) * time.Second
}

// DefaultResponseHeaderTimeout is the time-to-first-byte guard applied by
// NewTransport. Streaming upstreams return headers immediately, so 30s is ample
// for them; it only bites a non-streaming upstream that buffers a slow response.
const DefaultResponseHeaderTimeout = 30 * time.Second

// NewTransport returns a pooled http.Transport sized for sustained traffic to a single upstream host.
//
// KeepAlive=30s is the critical setting against AWS NAT-GW / NLB / VPC-endpoint
// reapers (350s fixed idle): the dialer's TCP keepalives keep the connection
// live so the zero-byte-stall failure mode can't accumulate at the network layer.
// ResponseHeaderTimeout only guards time-to-first-byte; per-read inactivity
// during streaming is enforced by StreamBody's watchdog.
func NewTransport(dialTimeout, tlsTimeout time.Duration) *http.Transport {
	return NewTransportWithResponseHeaderTimeout(dialTimeout, tlsTimeout, DefaultResponseHeaderTimeout)
}

// NewTransportWithResponseHeaderTimeout is NewTransport with a caller-chosen
// time-to-first-byte guard. Upstreams whose first byte can legitimately arrive
// later than the 30s default pass a larger value: the OpenAI Responses API for
// gpt-5.x high-effort reasoning can take well over 30s to emit its first SSE
// event, so a 30s header timeout false-trips even when the model is healthy.
// Per-read inactivity once the stream is flowing is still bounded by
// StreamBody's idle watchdog (DefaultSSEIdleTimeout), so a generous header
// timeout does not reintroduce an unbounded hang.
func NewTransportWithResponseHeaderTimeout(dialTimeout, tlsTimeout, responseHeaderTimeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConnsPerHost:   64,
		MaxIdleConns:          256,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   tlsTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
		ForceAttemptHTTP2:     true,
	}
}

// StartIdleWatchdog starts a goroutine that cancels ctx with ErrUpstreamIdleTimeout
// when more than idleTimeout has elapsed without a Mark call. The returned mark
// function should be invoked on every successful upstream read (or other progress
// signal). The returned stop function terminates the watchdog goroutine and must
// be called via defer in the caller's scope.
//
// idleTimeout <= 0 or cancel == nil disables the watchdog (no-op mark/stop).
func StartIdleWatchdog(ctx context.Context, cancel context.CancelCauseFunc, idleTimeout time.Duration) (mark func(), stop func()) {
	if idleTimeout <= 0 || cancel == nil {
		return func() {}, func() {}
	}
	var lastNS atomic.Int64
	lastNS.Store(time.Now().UnixNano())
	done := make(chan struct{})
	go func() {
		interval := max(idleTimeout/3, 100*time.Millisecond)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				if time.Since(time.Unix(0, lastNS.Load())) > idleTimeout {
					cancel(ErrUpstreamIdleTimeout)
					return
				}
			}
		}
	}()
	return func() { lastNS.Store(time.Now().UnixNano()) },
		sync.OnceFunc(func() { close(done) })
}

// StreamBody reads r chunk-by-chunk into w, flushing after each write. Returns
// UpstreamStatusError when status is non-2xx.
//
// When idleTimeout > 0 a watchdog goroutine cancels ctx via cancel with
// ErrUpstreamIdleTimeout if no upstream bytes arrive for idleTimeout. Callers
// must pass a context wrapped via context.WithCancelCause so the cause survives
// to the error site. On stall, returns ErrUpstreamIdleTimeout directly so the
// caller does not need to inspect context.Cause itself.
func StreamBody(ctx context.Context, cancel context.CancelCauseFunc, idleTimeout time.Duration, r io.Reader, status int, w http.ResponseWriter, t *otel.Timing) error {
	mark, stop := StartIdleWatchdog(ctx, cancel, idleTimeout)
	defer stop()

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, FlushChunk)
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			mark()
			t.StampUpstreamFirstByte()
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr == io.EOF {
			t.StampUpstreamEOF()
			if status < 200 || status >= 300 {
				return &providers.UpstreamStatusError{Status: status}
			}
			return nil
		}
		if readErr != nil {
			if cause := context.Cause(ctx); errors.Is(cause, ErrUpstreamIdleTimeout) {
				return ErrUpstreamIdleTimeout
			}
			return readErr
		}
	}
}
