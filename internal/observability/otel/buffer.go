package otel

import (
	"context"
	"sync"
)

// Buffer accumulates spans for a single request and flushes them to an
// Emitter in bulk. All methods are nil-safe (zero overhead when disabled).
type Buffer struct {
	emitter *Emitter
	traceID [16]byte
	mu      sync.Mutex
	spans   []Span
}

// NewBuffer creates a request-scoped span buffer. Returns nil when emitter
// is nil (OTel disabled).
func NewBuffer(emitter *Emitter) *Buffer {
	if emitter == nil {
		return nil
	}
	return &Buffer{
		emitter: emitter,
		traceID: generateTraceID(),
	}
}

// Record appends a span to the buffer. Safe for concurrent use.
func (b *Buffer) Record(s Span) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.spans = append(b.spans, s)
	b.mu.Unlock()
}

// Flush drains all buffered spans into the emitter's async export queue.
func (b *Buffer) Flush() {
	if b == nil {
		return
	}

	b.mu.Lock()
	pending := b.spans
	b.spans = nil
	b.mu.Unlock()

	for _, s := range pending {
		b.emitter.Enqueue(spanToProto(s, b.traceID))
	}
}

type bufferKey struct{}

// WithContext returns a child context carrying this buffer.
func (b *Buffer) WithContext(ctx context.Context) context.Context {
	if b == nil {
		return ctx
	}
	return context.WithValue(ctx, bufferKey{}, b)
}

// Record retrieves the Buffer from ctx and appends the span. No-op when
// the context does not carry a buffer (OTel disabled or pre-buffer middleware).
func Record(ctx context.Context, s Span) {
	if buf, ok := ctx.Value(bufferKey{}).(*Buffer); ok {
		buf.Record(s)
	}
}

// Flush retrieves the Buffer from ctx and flushes all buffered spans.
func Flush(ctx context.Context) {
	if buf, ok := ctx.Value(bufferKey{}).(*Buffer); ok {
		buf.Flush()
	}
}

