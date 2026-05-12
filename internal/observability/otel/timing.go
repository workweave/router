package otel

import (
	"context"
	"sync/atomic"
	"time"
)

// Timing carries per-request latency timestamps that provider adapters and
// middleware stamp as work progresses. All fields are set via the Stamp*
// methods (first-writer-wins CAS) and are safe for concurrent use. A nil
// *Timing is valid — all methods no-op, mirroring Buffer's nil safety.
type Timing struct {
	EntryNanos             atomic.Int64
	UpstreamRequestNanos   atomic.Int64
	UpstreamHeadersNanos   atomic.Int64
	UpstreamFirstByteNanos atomic.Int64
	UpstreamEOFNanos       atomic.Int64
}

// stampOnce stores time.Now().UnixNano() into field if it has not been stamped
// yet (first-writer-wins).
func stampOnce(field *atomic.Int64) {
	field.CompareAndSwap(0, time.Now().UnixNano())
}

// StampEntry records the request-entry timestamp.
func (t *Timing) StampEntry() {
	if t == nil {
		return
	}
	stampOnce(&t.EntryNanos)
}

// StampUpstreamRequest records the instant just before the upstream HTTP request
// is dispatched.
func (t *Timing) StampUpstreamRequest() {
	if t == nil {
		return
	}
	stampOnce(&t.UpstreamRequestNanos)
}

// StampUpstreamHeaders records the instant the upstream HTTP response headers
// arrive (TTFB-headers).
func (t *Timing) StampUpstreamHeaders() {
	if t == nil {
		return
	}
	stampOnce(&t.UpstreamHeadersNanos)
}

// StampUpstreamFirstByte records the instant the first body byte is read from
// the upstream response (TTFB-body).
func (t *Timing) StampUpstreamFirstByte() {
	if t == nil {
		return
	}
	stampOnce(&t.UpstreamFirstByteNanos)
}

// StampUpstreamEOF records the instant the upstream response body reaches EOF.
func (t *Timing) StampUpstreamEOF() {
	if t == nil {
		return
	}
	stampOnce(&t.UpstreamEOFNanos)
}

// Ms returns the millisecond delta between two stamped fields, or 0 when either
// field is unstamped.
func (t *Timing) Ms(from, to *atomic.Int64) int64 {
	if t == nil {
		return 0
	}
	f, tt := from.Load(), to.Load()
	if f == 0 || tt == 0 {
		return 0
	}
	return (tt - f) / int64(time.Millisecond)
}

// MsSince returns the millisecond delta between the stamped field and now, or 0
// when the field is unstamped.
func (t *Timing) MsSince(from *atomic.Int64) int64 {
	if t == nil {
		return 0
	}
	f := from.Load()
	if f == 0 {
		return 0
	}
	return (time.Now().UnixNano() - f) / int64(time.Millisecond)
}

type timingKey struct{}

// WithTiming creates a Timing, stashes it in ctx, and returns both.
func WithTiming(ctx context.Context) (context.Context, *Timing) {
	t := &Timing{}
	return context.WithValue(ctx, timingKey{}, t), t
}

// TimingFrom retrieves the *Timing from ctx, or nil when none was attached.
// All Timing methods are nil-safe, so callers can stamp unconditionally.
func TimingFrom(ctx context.Context) *Timing {
	t, _ := ctx.Value(timingKey{}).(*Timing)
	return t
}
