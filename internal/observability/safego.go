package observability

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// SafeGo runs fn in a background goroutine bounded by timeout, recovering
// any panic so a bug in a best-effort side effect (telemetry insert, cache
// invalidation fanout, billing signal) drops the operation instead of taking
// down the process. fn receives a fresh context.Background()-derived
// context rather than a caller-supplied one, since these operations must
// outlive the request that triggered them (response already written,
// caller's ctx may already be canceled). fn is responsible for logging its
// own failure with whatever level/fields fit that operation; SafeGo only
// guards the goroutine boundary.
//
// Counterpart for boot-time, long-running goroutines (Pub/Sub listeners,
// sweep loops) that must run until the parent ctx cancels rather than a
// bounded timeout: cmd/router/main.go's safeGo.
func SafeGo(log *slog.Logger, timeout time.Duration, name string, fn func(ctx context.Context)) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("Background goroutine panicked", "goroutine", name, "panic", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		fn(ctx)
	}()
}

// TrackedGroup is a WaitGroup for SafeGo-style background work that must not
// be dropped at shutdown (e.g. billing debits). Create with NewTrackedGroup,
// which wires a cancellable context so shutdown can abort in-flight work
// rather than waiting on its individual timeouts.
type TrackedGroup struct {
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
}

// NewTrackedGroup returns a group whose operations share a cancellable
// context.
func NewTrackedGroup() *TrackedGroup {
	ctx, cancel := context.WithCancel(context.Background())
	return &TrackedGroup{ctx: ctx, cancel: cancel}
}

// Cancel aborts every in-flight operation at its next context check (e.g. the
// pgx call returns early). Safe to call exactly once.
func (g *TrackedGroup) Cancel() {
	g.once.Do(g.cancel)
}

// Context returns a per-operation deadline derived from the group context.
func (g *TrackedGroup) Context(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(g.ctx, timeout)
}

// SafeGoTracked runs fn exactly like SafeGo but registers it on g so a
// graceful shutdown can drain in-flight work before closing shared resources
// like the DB pool. The operation context derives from the group's cancellable
// context (so Cancel aborts it) with a generous per-operation timeout so one
// slow debit can't hold the drain open.
func SafeGoTracked(g *TrackedGroup, log *slog.Logger, timeout time.Duration, name string, fn func(ctx context.Context)) {
	SafeGoTrackedWithContext(g.ctx, g, log, timeout, name, fn)
}

// SafeGoTrackedWithContext is SafeGoTracked with the operation context bound
// to opCtx instead of the group context.
func SafeGoTrackedWithContext(opCtx context.Context, g *TrackedGroup, log *slog.Logger, timeout time.Duration, name string, fn func(ctx context.Context)) {
	g.wg.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("Background goroutine panicked", "goroutine", name, "panic", r)
			}
		}()
		ctx, cancel := context.WithTimeout(opCtx, timeout)
		defer cancel()
		fn(ctx)
	})
}

// Wait blocks until every goroutine launched through SafeGoTracked has
// finished. Each carries its own bounded timeout, so this cannot hang past
// the longest of them. For shutdown use Cancel + WaitWithContext instead.
func (g *TrackedGroup) Wait() {
	g.wg.Wait()
}

// WaitWithContext blocks until the tracked work is done or ctx expires —
// whichever comes first, so the drain is bounded by the shutdown budget even
// if a single operation is overrunning. Call Cancel first to stop overruns at
// their next context check.
func (g *TrackedGroup) WaitWithContext(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		g.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
