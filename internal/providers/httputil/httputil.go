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
//
// Aliases providers.ErrUpstreamIdleTimeout — the sentinel lives there so
// providers.IsRetryable can classify it explicitly without an import cycle
// (this package imports providers). errors.Is matches against either name.
var ErrUpstreamIdleTimeout = providers.ErrUpstreamIdleTimeout

// ErrUpstreamOutputStall re-exports providers.ErrUpstreamOutputStall — the
// cause set when the output-progress watchdog (StartIdleWatchdogCause) trips
// because the stream stayed byte-alive but produced no output-bearing content.
var ErrUpstreamOutputStall = providers.ErrUpstreamOutputStall

// DefaultSSEIdleTimeout is the per-read inactivity threshold for streaming
// upstream responses. AWS NAT Gateway / NLB / VPC-endpoint reapers silently
// drop idle TCP connections at 350s; a watchdog below that surfaces stalls
// as a clean error within tens of seconds instead of letting the request
// hang for the full overall deadline.
//
// Tunable via ROUTER_SSE_IDLE_TIMEOUT_SECONDS. Healthy generations stream a
// chunk at least every few seconds, so 45s of silence is unambiguously a stall.
var DefaultSSEIdleTimeout = idleTimeoutFromEnv("ROUTER_SSE_IDLE_TIMEOUT_SECONDS", 45*time.Second)

// DefaultResponsesSSEIdleTimeout is the idle-progress threshold for OpenAI
// Responses API (/v1/responses) streams. More generous than
// DefaultSSEIdleTimeout because a gpt-5.x reasoning turn can go tens of
// seconds between SSE frames while the model thinks (reasoning-summary deltas
// lag the actual reasoning). ANY received bytes count as progress — event
// frames, reasoning deltas, keepalives — so only a zero-BYTE gap trips it.
//
// This catches the byte-silent stall (2026-06-09: two /v1/responses streams
// produced no bytes at all until the 600s request cap). It does NOT catch a
// byte-alive/output-silent stall — a stream that keeps dribbling non-output
// frames (reasoning deltas, keepalives) while producing zero output tokens
// (2026-06-16: gpt-5.5 sat at zero output for the full 600s, bytes flowing the
// whole time). DefaultResponsesOutputStallTimeout guards that mode.
//
// Tunable via ROUTER_RESPONSES_SSE_IDLE_TIMEOUT_SECONDS.
var DefaultResponsesSSEIdleTimeout = idleTimeoutFromEnv("ROUTER_RESPONSES_SSE_IDLE_TIMEOUT_SECONDS", 90*time.Second)

// DefaultResponsesOutputStallTimeout is the OUTPUT-progress threshold for
// OpenAI Responses streams: the maximum time the upstream may stay byte-alive
// (resetting DefaultResponsesSSEIdleTimeout) while producing zero
// output-bearing content (assistant text, tool-call arguments, or a terminal
// response envelope). It is deliberately far larger than the idle timeout so a
// genuinely long reasoning phase that eventually emits output is never clipped
// — only a turn that thinks/keepalives for minutes without ever producing
// output is aborted. Set below the 600s request cap so the watchdog (not the
// cap) surfaces the stall as a retryable error, while staying generous enough
// to clear realistic worst-case reasoning. The OUTPUT-progress mark is fed by
// the Responses→Anthropic translator, which alone can tell output frames from
// reasoning/keepalive frames.
//
// Tunable via ROUTER_RESPONSES_OUTPUT_STALL_TIMEOUT_SECONDS.
var DefaultResponsesOutputStallTimeout = idleTimeoutFromEnv("ROUTER_RESPONSES_OUTPUT_STALL_TIMEOUT_SECONDS", 240*time.Second)

// DefaultOutputStallTimeout is the OUTPUT-progress threshold for the generic
// OpenAI-compatible Chat Completions adapter (OpenRouter / Fireworks /
// DeepInfra / Bedrock). It is the Chat-Completions analogue of
// DefaultResponsesOutputStallTimeout: DefaultSSEIdleTimeout resets on ANY
// upstream byte, so a provider that keeps the connection byte-alive with SSE
// keepalive comments (": OPENROUTER PROCESSING") or empty/role-only delta
// frames while producing zero output content rides to the 600s request cap.
//
// Prod incident 2026-06-19: a DeepInfra deepseek-v4-flash stream stayed
// byte-alive but output-silent for ~10min until the request cap; the client
// then retried and hit a model-not-found 404. The byte-idle watchdog (45s)
// cannot catch a byte-alive stall — only a time-since-last-OUTPUT watchdog can.
// The mark is fed by the OpenAI→Anthropic SSE translator on output-bearing
// deltas (assistant text, streamed reasoning, tool-call arguments, terminal
// finish) and never on keepalives or empty deltas. Unlike the Responses budget,
// streamed reasoning_content DOES count here: OSS reasoning models emit it as
// real tokens (rendered as a thinking block), not a sparse server-side summary.
//
// Set below the 600s request cap so the watchdog (not the cap) surfaces the
// stall as a retryable error, while staying generous enough that a large-context
// prefill emitting only keepalives before its first token is never clipped.
//
// Tunable via ROUTER_OUTPUT_STALL_TIMEOUT_SECONDS.
var DefaultOutputStallTimeout = idleTimeoutFromEnv("ROUTER_OUTPUT_STALL_TIMEOUT_SECONDS", 240*time.Second)

// idleTimeoutFromEnv reads a whole-seconds override from envVar, falling back
// to fallback when unset, unparsable, or non-positive.
func idleTimeoutFromEnv(envVar string, fallback time.Duration) time.Duration {
	v := os.Getenv(envVar)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
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
	return StartIdleWatchdogCause(ctx, cancel, idleTimeout, ErrUpstreamIdleTimeout)
}

// StartIdleWatchdogCause is StartIdleWatchdog with a caller-chosen cancel
// cause. Use it to run a second, independent watchdog on the same request
// context that measures a different progress signal — e.g. an output-progress
// watchdog whose mark is fed only on output-bearing events (cause
// ErrUpstreamOutputStall), running alongside the byte-idle watchdog (cause
// ErrUpstreamIdleTimeout). Whichever watchdog trips first wins: context cancel
// causes are set once, so a later cancel from the other watchdog is a no-op.
//
// idleTimeout <= 0 or cancel == nil disables the watchdog (no-op mark/stop).
func StartIdleWatchdogCause(ctx context.Context, cancel context.CancelCauseFunc, idleTimeout time.Duration, cause error) (mark func(), stop func()) {
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
					cancel(cause)
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
			// A second watchdog (e.g. the OpenAI output-progress watchdog) may
			// share this ctx and cancel with a different cause; surface whichever
			// sentinel tripped so the caller classifies it retryable instead of
			// seeing a bare context.Canceled.
			if cause := context.Cause(ctx); errors.Is(cause, ErrUpstreamIdleTimeout) || errors.Is(cause, ErrUpstreamOutputStall) {
				return cause
			}
			return readErr
		}
	}
}
