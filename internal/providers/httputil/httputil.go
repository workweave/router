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

// ErrUpstreamIdleTimeout is the cause set on the request context when the SSE
// inactivity watchdog fires; the proxy's failover logic retries on it. Aliases
// providers.ErrUpstreamIdleTimeout so providers.IsRetryable can classify it
// without an import cycle — errors.Is matches either name.
var ErrUpstreamIdleTimeout = providers.ErrUpstreamIdleTimeout

// ErrUpstreamOutputStall re-exports providers.ErrUpstreamOutputStall: set when
// the output-progress watchdog trips because the stream stayed byte-alive but
// produced no output-bearing content.
var ErrUpstreamOutputStall = providers.ErrUpstreamOutputStall

// ErrUpstreamSlowThroughput re-exports providers.ErrUpstreamSlowThroughput:
// set when the minimum-throughput watchdog trips because output is flowing
// but below the configured floor over the rolling window.
var ErrUpstreamSlowThroughput = providers.ErrUpstreamSlowThroughput

// DefaultSSEIdleTimeout is the per-read inactivity threshold for streaming
// upstream responses. AWS NAT/NLB/VPC-endpoint reapers drop idle TCP
// connections at 350s; a watchdog below that surfaces stalls quickly instead
// of hanging for the full deadline. Tunable via ROUTER_SSE_IDLE_TIMEOUT_SECONDS.
var DefaultSSEIdleTimeout = idleTimeoutFromEnv("ROUTER_SSE_IDLE_TIMEOUT_SECONDS", 45*time.Second)

// DefaultResponsesSSEIdleTimeout is the idle-progress threshold for OpenAI
// Responses API streams. More generous than DefaultSSEIdleTimeout because a
// gpt-5.x reasoning turn can go tens of seconds between SSE frames; any
// received byte counts as progress, so only a zero-byte gap trips it. Catches
// the byte-silent stall (2026-06-09 incident); does not catch a byte-alive but
// output-silent stall — see DefaultResponsesOutputStallTimeout for that.
// Tunable via ROUTER_RESPONSES_SSE_IDLE_TIMEOUT_SECONDS.
var DefaultResponsesSSEIdleTimeout = idleTimeoutFromEnv("ROUTER_RESPONSES_SSE_IDLE_TIMEOUT_SECONDS", 90*time.Second)

// DefaultResponsesOutputStallTimeout is the OUTPUT-progress threshold for
// OpenAI Responses streams: max time the upstream may stay byte-alive while
// producing zero output-bearing content. Set well above the idle timeout so a
// long-but-real reasoning phase is never clipped, and below the 600s request
// cap so the watchdog surfaces the stall as retryable first. Fed by the
// Responses→Anthropic translator, the only place that can tell output frames
// from reasoning/keepalive frames. Tunable via
// ROUTER_RESPONSES_OUTPUT_STALL_TIMEOUT_SECONDS.
var DefaultResponsesOutputStallTimeout = idleTimeoutFromEnv("ROUTER_RESPONSES_OUTPUT_STALL_TIMEOUT_SECONDS", 240*time.Second)

// DefaultOutputStallTimeout is the Chat-Completions analogue of
// DefaultResponsesOutputStallTimeout, for the generic OpenAI-compatible
// adapter (OpenRouter/Fireworks/DeepInfra/Bedrock). DefaultSSEIdleTimeout
// resets on any byte, so a provider that stays byte-alive via keepalive
// comments or empty/role-only deltas while emitting zero output content would
// otherwise ride to the request cap (prod incident 2026-06-19: a DeepInfra
// stream did this for ~10min, then the client retry hit a 404). Fed by the
// OpenAI→Anthropic SSE translator on output-bearing deltas only; unlike the
// Responses budget, streamed reasoning_content counts here since OSS models
// emit it as real rendered tokens. Tunable via ROUTER_OUTPUT_STALL_TIMEOUT_SECONDS.
var DefaultOutputStallTimeout = idleTimeoutFromEnv("ROUTER_OUTPUT_STALL_TIMEOUT_SECONDS", 240*time.Second)

// Minimum-throughput watchdog defaults. The byte-idle and output-stall
// watchdogs catch a stream that STOPS producing; neither catches one that
// keeps producing but at a crawl (prod incident 2026-06-25: a deepseek stream
// dribbled ~13 events/s for ~132s, riding to the request cap without ever
// being classified retryable). This watchdog measures OUTPUT-event throughput
// over a rolling window, fed by the same mark as the output-stall watchdog,
// and aborts with ErrUpstreamSlowThroughput once below-floor after warmup.
//
// Defaults are conservative so a legitimately slow "thinking" model is never
// killed: floor is <8 deltas/20s (~0.4/s, >30x below the incident's rate, so
// it targets only the near-frozen tail); the 20s window absorbs brief
// mid-stream pauses; the 90s warmup exempts prefill/initial reasoning.
// All three tunable via env (floor 0 disables the watchdog).
var (
	// DefaultThroughputWindow is the rolling window over which output-delta
	// throughput is measured. Tunable via ROUTER_THROUGHPUT_WINDOW_SECONDS.
	DefaultThroughputWindow = idleTimeoutFromEnv("ROUTER_THROUGHPUT_WINDOW_SECONDS", 20*time.Second)

	// DefaultThroughputMinElapsed is the warmup period during which the
	// throughput watchdog is inert (no abort), giving prefill/initial-reasoning
	// a grace period. Tunable via ROUTER_THROUGHPUT_MIN_ELAPSED_SECONDS.
	DefaultThroughputMinElapsed = idleTimeoutFromEnv("ROUTER_THROUGHPUT_MIN_ELAPSED_SECONDS", 90*time.Second)

	// DefaultMinThroughputDeltasPerWindow is the minimum output-bearing deltas
	// required within DefaultThroughputWindow post-warmup, else the stream
	// aborts as ErrUpstreamSlowThroughput. <= 0 disables the watchdog. Tunable
	// via ROUTER_MIN_THROUGHPUT_DELTAS_PER_WINDOW.
	DefaultMinThroughputDeltasPerWindow = intFromEnv("ROUTER_MIN_THROUGHPUT_DELTAS_PER_WINDOW", 8)
)

// intFromEnv reads a whole-number override from envVar, falling back to
// fallback when unset or unparsable. Unlike idleTimeoutFromEnv it permits 0
// (and negatives) through so an operator can disable a count-based guard.
func intFromEnv(envVar string, fallback int) int {
	v := os.Getenv(envVar)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

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
// KeepAlive=30s guards against AWS NAT-GW/NLB/VPC-endpoint reapers (350s fixed
// idle) by keeping the TCP connection live at the network layer.
// ResponseHeaderTimeout only guards time-to-first-byte; per-read inactivity is
// enforced separately by StreamBody's watchdog.
func NewTransport(dialTimeout, tlsTimeout time.Duration) *http.Transport {
	return NewTransportWithResponseHeaderTimeout(dialTimeout, tlsTimeout, DefaultResponseHeaderTimeout)
}

// NewTransportWithResponseHeaderTimeout is NewTransport with a caller-chosen
// time-to-first-byte guard. Pass a larger value for upstreams whose first byte
// can legitimately arrive later than 30s (e.g. gpt-5.x high-effort reasoning
// via Responses API). Streaming inactivity is still bounded separately by
// StreamBody's idle watchdog, so this can't reintroduce an unbounded hang.
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

// StartIdleWatchdog cancels ctx with ErrUpstreamIdleTimeout once idleTimeout
// elapses without a mark() call. Call stop() via defer to terminate the
// goroutine. idleTimeout <= 0 or cancel == nil disables the watchdog (no-op).
func StartIdleWatchdog(ctx context.Context, cancel context.CancelCauseFunc, idleTimeout time.Duration) (mark func(), stop func()) {
	return StartIdleWatchdogCause(ctx, cancel, idleTimeout, ErrUpstreamIdleTimeout)
}

// StartIdleWatchdogCause is StartIdleWatchdog with a caller-chosen cancel
// cause, letting a second independent watchdog run on the same context for a
// different progress signal. Whichever trips first wins — cancel causes are
// set once, so a later cancel from the other watchdog is a no-op.
// idleTimeout <= 0 or cancel == nil disables the watchdog (no-op).
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

// StartThroughputWatchdog cancels ctx with cause when the upstream keeps
// producing output but at a sustained rate below minDeltas per window (after
// minElapsed warmup). Unlike the idle watchdogs, which measure time since the
// last event, this measures the COUNT of mark() calls in a rolling window —
// call mark() on every output-bearing delta, and stop() via defer.
// minDeltas <= 0, window <= 0, or cancel == nil disables the watchdog (no-op).
// minElapsed <= 0 evaluates as soon as a full window of data exists.
func StartThroughputWatchdog(ctx context.Context, cancel context.CancelCauseFunc, window, minElapsed time.Duration, minDeltas int, cause error) (mark func(), stop func()) {
	if minDeltas <= 0 || window <= 0 || cancel == nil {
		return func() {}, func() {}
	}
	var count atomic.Int64
	start := time.Now()
	// Normalized rate (not raw count) so the tick interval doesn't distort throughput.
	floorRate := float64(minDeltas) / window.Seconds()
	done := make(chan struct{})
	go func() {
		// Tick several times per window to catch a sub-floor stretch promptly.
		interval := min(max(window/4, 50*time.Millisecond), 5*time.Second)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		// Anchors the trailing window: marks-in-window = current - snapshot.
		var windowStartCount int64
		windowStart := start
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				now := time.Now()
				if now.Sub(start) < minElapsed {
					// Warmup: don't evaluate, and keep the window anchored at
					// "now" so the first post-warmup evaluation reflects only
					// post-warmup traffic, not the slow prefill.
					windowStartCount = count.Load()
					windowStart = now
					continue
				}
				elapsed := now.Sub(windowStart)
				if elapsed < window {
					// Not yet a full window of post-warmup data to judge on.
					continue
				}
				cur := count.Load()
				rate := float64(cur-windowStartCount) / elapsed.Seconds()
				if rate < floorRate {
					cancel(cause)
					return
				}
				// Slide the window forward.
				windowStartCount = cur
				windowStart = now
			}
		}
	}()
	return func() { count.Add(1) },
		sync.OnceFunc(func() { close(done) })
}

// StreamBody reads r chunk-by-chunk into w, flushing after each write.
// Returns UpstreamStatusError when status is non-2xx. When idleTimeout > 0, a
// watchdog cancels ctx with ErrUpstreamIdleTimeout on stall and StreamBody
// returns that error directly; ctx must be wrapped via context.WithCancelCause
// so the cause survives to the error site.
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
			// A different watchdog sharing this ctx may have cancelled it; surface
			// its sentinel so the caller can classify retryable instead of context.Canceled.
			if cause := context.Cause(ctx); errors.Is(cause, ErrUpstreamIdleTimeout) || errors.Is(cause, ErrUpstreamOutputStall) || errors.Is(cause, ErrUpstreamSlowThroughput) {
				return cause
			}
			return readErr
		}
	}
}
