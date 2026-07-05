package observability

import (
	"context"
	"log/slog"
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
