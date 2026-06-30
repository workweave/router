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

// ErrUpstreamSlowThroughput re-exports providers.ErrUpstreamSlowThroughput —
// the cause set when the minimum-throughput watchdog (StartThroughputWatchdog)
// trips because the stream IS producing output, but at a sustained rate below
// the configured floor over the rolling window.
var ErrUpstreamSlowThroughput = providers.ErrUpstreamSlowThroughput

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

// Minimum-throughput watchdog defaults. The byte-idle (45s) and output-stall
// (240s) watchdogs both catch a stream that STOPS producing — bytes or output
// respectively. Neither catches a stream that keeps producing output but at a
// crawl: a clean 200 that dribbles output-bearing deltas steadily enough to
// reset both marks yet so slowly the turn takes minutes and the client appears
// frozen, riding toward the 600s request cap with no failover.
//
// Prod incident 2026-06-25: a deepseek/deepseek-v4-flash stream emitted ~1774
// output deltas over ~132s (~13 events/s) — a steadily-flowing 200 that tripped
// none of the existing watchdogs and was never classified retryable. This
// watchdog measures sustained OUTPUT-event throughput over a rolling window and,
// once a warmup has passed, aborts with ErrUpstreamSlowThroughput (retryable)
// when the rate stays below the floor.
//
// The mark is the SAME output-progress signal fed to the output-stall watchdog
// (one mark per output-bearing delta: assistant text, streamed reasoning,
// tool-call args, terminal finish — never keepalives/empty deltas). One delta
// is a small chunk of tokens, so delta-rate is a usable proxy for token-rate
// without threading byte counts through every translator.
//
// CONSERVATIVE BY DESIGN — these defaults err strongly on the side of NOT
// aborting, so a legitimately slow "thinking" model is never killed:
//
//   - Floor (DefaultMinThroughputDeltasPerWindow / DefaultThroughputWindow):
//     fewer than 8 output deltas in a 20s rolling window (~0.4 deltas/s) is the
//     abort condition. The prod dribble ran ~13 deltas/s — over 30x this floor —
//     so this would NOT have fired on it on rate alone; it targets the strictly
//     worse "near-frozen but technically advancing" tail. A healthy generation
//     streams many deltas per second, orders of magnitude above the floor.
//   - Window (DefaultThroughputWindow): 20s rolling window. Long enough that a
//     brief mid-stream pause (a slow tool-arg chunk, a reasoning gap) does not
//     trip it; the rate is averaged over the window, not instantaneous.
//   - Warmup (DefaultThroughputMinElapsed): the watchdog is INERT for the first
//     90s of the stream. A large-context prefill, a long initial reasoning phase,
//     or a slow-to-warm provider gets a full grace period before throughput is
//     ever evaluated. Combined with the rolling window, an abort requires the
//     stream to be both past warmup AND sustaining sub-floor output.
//
// All three are tunable via env so the floor can be relaxed (or the watchdog
// disabled by setting the floor to 0) without a redeploy.
var (
	// DefaultThroughputWindow is the rolling window over which output-delta
	// throughput is measured. Tunable via ROUTER_THROUGHPUT_WINDOW_SECONDS.
	DefaultThroughputWindow = idleTimeoutFromEnv("ROUTER_THROUGHPUT_WINDOW_SECONDS", 20*time.Second)

	// DefaultThroughputMinElapsed is the warmup period during which the
	// throughput watchdog is inert (no abort), giving prefill/initial-reasoning
	// a grace period. Tunable via ROUTER_THROUGHPUT_MIN_ELAPSED_SECONDS.
	DefaultThroughputMinElapsed = idleTimeoutFromEnv("ROUTER_THROUGHPUT_MIN_ELAPSED_SECONDS", 90*time.Second)

	// DefaultMinThroughputDeltasPerWindow is the minimum number of output-bearing
	// deltas that must arrive within DefaultThroughputWindow once warmup has
	// passed; below this the stream is aborted as ErrUpstreamSlowThroughput. A
	// value <= 0 disables the watchdog entirely. Tunable via
	// ROUTER_MIN_THROUGHPUT_DELTAS_PER_WINDOW.
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

// StartThroughputWatchdog starts a goroutine that cancels ctx with cause when
// the upstream IS producing output but at a sustained rate below minDeltas per
// window, once minElapsed has passed since the watchdog started. The returned
// mark must be invoked on every output-bearing delta (the SAME signal fed to
// the output-progress watchdog); the returned stop terminates the goroutine and
// must be called via defer.
//
// Unlike the idle watchdogs (which measure time-since-last-event), this measures
// the COUNT of marks within a rolling window: a stream that keeps marking but
// too slowly is the failure mode here. The watchdog is inert until minElapsed
// has elapsed, so prefill / initial reasoning is never penalized; thereafter, on
// each tick it counts marks observed in the trailing window and trips if that
// count is below minDeltas.
//
// minDeltas <= 0, window <= 0, or cancel == nil disables the watchdog
// (no-op mark/stop). minElapsed <= 0 means evaluate as soon as a full window
// of data exists.
func StartThroughputWatchdog(ctx context.Context, cancel context.CancelCauseFunc, window, minElapsed time.Duration, minDeltas int, cause error) (mark func(), stop func()) {
	if minDeltas <= 0 || window <= 0 || cancel == nil {
		return func() {}, func() {}
	}
	var count atomic.Int64
	start := time.Now()
	// floorRate is the minimum marks-per-second the stream must sustain
	// (minDeltas spread over one window). Evaluating a normalized RATE rather
	// than a raw count over the exact window lets the watchdog tick at its own
	// cadence without the tick interval distorting the measured throughput.
	floorRate := float64(minDeltas) / window.Seconds()
	done := make(chan struct{})
	go func() {
		// Tick several times per window so a sub-floor stretch is caught
		// promptly; cap small so a short test window still evaluates within it,
		// and floor so a large production window doesn't spin.
		interval := min(max(window/4, 50*time.Millisecond), 5*time.Second)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		// windowStartCount / windowStart anchor the trailing measurement window.
		// Marks-in-window = current - snapshot, over now-windowStart seconds.
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
			if cause := context.Cause(ctx); errors.Is(cause, ErrUpstreamIdleTimeout) || errors.Is(cause, ErrUpstreamOutputStall) || errors.Is(cause, ErrUpstreamSlowThroughput) {
				return cause
			}
			return readErr
		}
	}
}
