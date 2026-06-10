package httputil

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workweave/router/internal/observability/otel"
	"workweave/router/internal/providers"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pacedReader emits bytes from chunks on a per-chunk delay schedule. A delay of
// 0 means "stall until ctx cancels" — mirrors how a real net.Conn unblocks Read
// when its parent http.Request context is canceled.
type pacedReader struct {
	ctx    context.Context
	chunks []string
	delays []time.Duration
	i      int
	closed atomic.Bool
}

func (p *pacedReader) Read(buf []byte) (int, error) {
	if p.closed.Load() {
		return 0, io.EOF
	}
	if p.i >= len(p.chunks) {
		return 0, io.EOF
	}
	d := p.delays[p.i]
	if d == 0 {
		<-p.ctx.Done()
		return 0, p.ctx.Err()
	}
	select {
	case <-p.ctx.Done():
		return 0, p.ctx.Err()
	case <-time.After(d):
	}
	n := copy(buf, p.chunks[p.i])
	p.i++
	return n, nil
}

func TestStreamBody_NoWatchdogPath(t *testing.T) {
	ctx := context.Background()
	r := &pacedReader{
		ctx:    ctx,
		chunks: []string{"hello ", "world"},
		delays: []time.Duration{1 * time.Millisecond, 1 * time.Millisecond},
	}
	w := httptest.NewRecorder()

	err := StreamBody(ctx, nil, 0, r, 200, w, &otel.Timing{})
	require.NoError(t, err)
	assert.Equal(t, "hello world", w.Body.String())
}

func TestStreamBody_WatchdogDoesNotFireOnLivelyStream(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	r := &pacedReader{
		ctx:    ctx,
		chunks: []string{"a", "b", "c", "d"},
		delays: []time.Duration{20 * time.Millisecond, 20 * time.Millisecond, 20 * time.Millisecond, 20 * time.Millisecond},
	}
	w := httptest.NewRecorder()

	err := StreamBody(ctx, cancel, 200*time.Millisecond, r, 200, w, &otel.Timing{})
	require.NoError(t, err)
	assert.Equal(t, "abcd", w.Body.String())
}

func TestStreamBody_WatchdogFiresOnStall(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	// Emits one chunk then stalls forever — watchdog should fire shortly after
	// the stall begins. The second chunk is unreachable because delays[1]=0
	// holds the reader on ctx.Done until the watchdog cancels.
	r := &pacedReader{
		ctx:    ctx,
		chunks: []string{"hello", "unreachable"},
		delays: []time.Duration{1 * time.Millisecond, 0},
	}
	w := httptest.NewRecorder()

	start := time.Now()
	err := StreamBody(ctx, cancel, 150*time.Millisecond, r, 200, w, &otel.Timing{})
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUpstreamIdleTimeout)
	// Generous upper bound (watchdog ticks at idleTimeout/3 = 50ms).
	assert.Less(t, elapsed, 500*time.Millisecond, "stall should surface well before any deadlock")
	assert.GreaterOrEqual(t, elapsed, 100*time.Millisecond, "stall should not surface before idleTimeout")
	assert.Equal(t, "hello", w.Body.String(), "bytes received before the stall must still be flushed")
}

func TestStreamBody_WatchdogFiresWithZeroPriorBytes(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	r := &pacedReader{
		ctx:    ctx,
		chunks: []string{"unused"},
		delays: []time.Duration{0},
	}
	w := httptest.NewRecorder()

	err := StreamBody(ctx, cancel, 100*time.Millisecond, r, 200, w, &otel.Timing{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUpstreamIdleTimeout)
}

func TestStreamBody_NonStreamingStatus(t *testing.T) {
	r := strings.NewReader("upstream error body")
	w := httptest.NewRecorder()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	err := StreamBody(ctx, cancel, 200*time.Millisecond, r, 500, w, &otel.Timing{})
	var statusErr *providers.UpstreamStatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, 500, statusErr.Status)
}

func TestStartIdleWatchdog_NoOpOnZeroTimeout(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	mark, stop := StartIdleWatchdog(ctx, cancel, 0)
	mark()
	stop()
	// No assertion — just confirming the calls don't panic and don't deadlock.
	assert.NoError(t, context.Cause(ctx))
}

func TestStartIdleWatchdog_FiresOnInactivity(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	_, stop := StartIdleWatchdog(ctx, cancel, 80*time.Millisecond)
	defer stop()

	// Don't call mark — let the watchdog fire.
	deadline := time.NewTimer(500 * time.Millisecond)
	defer deadline.Stop()
	select {
	case <-ctx.Done():
	case <-deadline.C:
		t.Fatal("watchdog never fired")
	}
	assert.ErrorIs(t, context.Cause(ctx), ErrUpstreamIdleTimeout)
}

func TestStartIdleWatchdog_DoesNotFireWhenMarked(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	mark, stop := StartIdleWatchdog(ctx, cancel, 100*time.Millisecond)
	defer stop()

	// Call mark faster than the timeout for ~3 timeouts worth.
	tickEnd := time.Now().Add(350 * time.Millisecond)
	for time.Now().Before(tickEnd) {
		mark()
		time.Sleep(20 * time.Millisecond)
	}
	assert.NoError(t, ctx.Err(), "watchdog should not cancel while mark is being called")

	// After stop, the ctx should still be alive (no watchdog cancellation).
	stop()
	assert.NoError(t, context.Cause(ctx))
}

func TestSSEIdleTimeoutFromEnv_DefaultsTo45s(t *testing.T) {
	t.Setenv("ROUTER_SSE_IDLE_TIMEOUT_SECONDS", "")
	assert.Equal(t, 45*time.Second, idleTimeoutFromEnv("ROUTER_SSE_IDLE_TIMEOUT_SECONDS", 45*time.Second))
}

func TestSSEIdleTimeoutFromEnv_OverrideRespected(t *testing.T) {
	t.Setenv("ROUTER_SSE_IDLE_TIMEOUT_SECONDS", "10")
	assert.Equal(t, 10*time.Second, idleTimeoutFromEnv("ROUTER_SSE_IDLE_TIMEOUT_SECONDS", 45*time.Second))
}

func TestSSEIdleTimeoutFromEnv_BadValueFallsBack(t *testing.T) {
	t.Setenv("ROUTER_SSE_IDLE_TIMEOUT_SECONDS", "garbage")
	assert.Equal(t, 45*time.Second, idleTimeoutFromEnv("ROUTER_SSE_IDLE_TIMEOUT_SECONDS", 45*time.Second))
}

func TestResponsesSSEIdleTimeoutFromEnv_OverrideRespected(t *testing.T) {
	t.Setenv("ROUTER_RESPONSES_SSE_IDLE_TIMEOUT_SECONDS", "120")
	assert.Equal(t, 120*time.Second, idleTimeoutFromEnv("ROUTER_RESPONSES_SSE_IDLE_TIMEOUT_SECONDS", 90*time.Second))
}

// IsRetryable must see idle-timeout stalls as retryable through the alias —
// this is what lets dispatchWithFallback rescue a mid-stream stall on the
// next binding (prod incident 2026-06-09).
func TestErrUpstreamIdleTimeout_AliasIsRetryable(t *testing.T) {
	assert.True(t, errors.Is(ErrUpstreamIdleTimeout, providers.ErrUpstreamIdleTimeout))
	assert.True(t, providers.IsRetryable(ErrUpstreamIdleTimeout))
}

// Sanity guard: ensure the exported sentinel is actually used.
var _ = errors.Is(ErrUpstreamIdleTimeout, ErrUpstreamIdleTimeout)
