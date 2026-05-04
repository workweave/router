package otel_test

import (
	"context"
	"testing"

	"workweave/router/internal/observability/otel"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTimingRoundTripViaContext(t *testing.T) {
	ctx := context.Background()
	ctx, tm := otel.WithTiming(ctx)
	require.NotNil(t, tm)

	got := otel.TimingFrom(ctx)
	assert.Equal(t, tm, got)
}

func TestTimingFromEmptyContext(t *testing.T) {
	got := otel.TimingFrom(context.Background())
	assert.Nil(t, got)
}

func TestStampIsIdempotent(t *testing.T) {
	tm := &otel.Timing{}
	tm.StampEntry()
	first := tm.EntryNanos.Load()
	require.NotZero(t, first)

	tm.StampEntry()
	assert.Equal(t, first, tm.EntryNanos.Load(), "second stamp must not overwrite")
}

func TestStampNilReceiver(t *testing.T) {
	var tm *otel.Timing
	assert.NotPanics(t, func() { tm.StampEntry() })
	assert.NotPanics(t, func() { tm.StampUpstreamRequest() })
	assert.NotPanics(t, func() { tm.StampUpstreamHeaders() })
	assert.NotPanics(t, func() { tm.StampUpstreamFirstByte() })
	assert.NotPanics(t, func() { tm.StampUpstreamEOF() })
}

func TestMsReturnsZeroWhenUnstamped(t *testing.T) {
	tm := &otel.Timing{}
	tm.StampEntry()
	assert.Zero(t, tm.Ms(&tm.EntryNanos, &tm.UpstreamRequestNanos), "unstamped 'to' field should yield 0")
	assert.Zero(t, tm.Ms(&tm.UpstreamRequestNanos, &tm.EntryNanos), "unstamped 'from' field should yield 0")
}

func TestMsNilReceiver(t *testing.T) {
	var tm *otel.Timing
	dummy := &otel.Timing{}
	assert.Zero(t, tm.Ms(&dummy.EntryNanos, &dummy.UpstreamRequestNanos))
}

func TestMsSinceNilReceiver(t *testing.T) {
	var tm *otel.Timing
	dummy := &otel.Timing{}
	assert.Zero(t, tm.MsSince(&dummy.EntryNanos))
}

func TestMsSinceReturnsZeroWhenUnstamped(t *testing.T) {
	tm := &otel.Timing{}
	assert.Zero(t, tm.MsSince(&tm.EntryNanos))
}

func TestMsBetweenStampedFields(t *testing.T) {
	tm := &otel.Timing{}
	tm.EntryNanos.Store(1_000_000_000)
	tm.UpstreamRequestNanos.Store(1_050_000_000)

	got := tm.Ms(&tm.EntryNanos, &tm.UpstreamRequestNanos)
	assert.Equal(t, int64(50), got)
}
