package usage_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/proxy/usage"
)

// clock is a controllable time source for exercising freshFor/expiry.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

const exhaustSalt = "salt"

func newObserverAt(start time.Time, ttl time.Duration) (*usage.Observer, *clock) {
	c := &clock{t: start}
	return usage.NewObserver([]byte(exhaustSalt), ttl, c.now), c
}

// A recorded exhaustion must read back as Exhausted so the failover engages —
// the whole point of the 429-as-signal path.
func TestRecordExhausted_MarksExhausted(t *testing.T) {
	obs, _ := newObserverAt(time.Unix(1_700_000_000, 0).UTC(), time.Minute)
	key := obs.Key([]byte("sk-ant-oat01-spent"))

	obs.RecordExhausted(key, time.Time{})

	snap, ok := obs.Snapshot(key)
	require.True(t, ok, "an exhaustion reading must be retained")
	assert.True(t, snap.Exhausted(), "a recorded exhaustion must report Exhausted")
}

// With no retry hint, the reading must outlive the short observer ttl (a
// header-less 429 would otherwise age out in a minute and re-read as slack,
// routing straight back into the spent token) but expire by the ~5h session
// window so a transient limit self-heals.
func TestRecordExhausted_FallbackWindowOutlivesTTL(t *testing.T) {
	obs, c := newObserverAt(time.Unix(1_700_000_000, 0).UTC(), time.Minute)
	key := obs.Key([]byte("sk-ant-oat01-spent"))
	obs.RecordExhausted(key, time.Time{})

	c.advance(2 * time.Hour) // past the 1m ttl, well within the 5h fallback
	snap, ok := obs.Snapshot(key)
	require.True(t, ok, "must survive far past the bare ttl on a header-less 429")
	assert.True(t, snap.Exhausted())

	c.advance(4 * time.Hour) // now past the 5h fallback window
	_, ok = obs.Snapshot(key)
	assert.False(t, ok, "must expire by the session window so a transient cap self-heals")
}

// Retry-After pins retention to the real reset: exhausted until the reset, slack
// (evicted) once it passes.
func TestRecordExhaustedFromHeaders_RetryAfter(t *testing.T) {
	obs, c := newObserverAt(time.Unix(1_700_000_000, 0).UTC(), time.Minute)
	key := obs.Key([]byte("sk-ant-oat01-spent"))

	h := http.Header{}
	h.Set("Retry-After", "1800") // 30 minutes
	obs.RecordExhaustedFromHeaders(key, h)

	c.advance(29 * time.Minute)
	snap, ok := obs.Snapshot(key)
	require.True(t, ok)
	assert.True(t, snap.Exhausted(), "still spent before Retry-After elapses")

	c.advance(2 * time.Minute) // now past the 30m reset
	_, ok = obs.Snapshot(key)
	assert.False(t, ok, "lifts once the Retry-After reset passes")
}

// An absolute unified-*-reset header (no Retry-After) is honored as the refill
// instant.
func TestRecordExhaustedFromHeaders_UnifiedReset(t *testing.T) {
	start := time.Unix(1_700_000_000, 0).UTC()
	obs, c := newObserverAt(start, time.Minute)
	key := obs.Key([]byte("sk-ant-oat01-spent"))

	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-reset", start.Add(45*time.Minute).Format(time.RFC3339))
	obs.RecordExhaustedFromHeaders(key, h)

	c.advance(44 * time.Minute)
	snap, ok := obs.Snapshot(key)
	require.True(t, ok)
	assert.True(t, snap.Exhausted())

	c.advance(2 * time.Minute)
	_, ok = obs.Snapshot(key)
	assert.False(t, ok, "lifts at the unified reset instant")
}

// Recording exhaustion must not erase a prior weekly-window reading — a 429
// reporting only the session cap should leave the weekly headroom intact.
func TestRecordExhausted_PreservesSecondaryWindow(t *testing.T) {
	start := time.Unix(1_700_000_000, 0).UTC()
	obs, _ := newObserverAt(start, time.Minute)
	key := obs.Key([]byte("sk-ant-oat01-spent"))

	// Prime a full snapshot: slack 5h, half-used weekly.
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-utilization", "10")
	h.Set("anthropic-ratelimit-unified-weekly-utilization", "50")
	snap, ok := usage.ParseAnthropicUnifiedHeaders(h)
	require.True(t, ok)
	obs.Record(key, snap)

	obs.RecordExhausted(key, time.Time{})

	got, ok := obs.Snapshot(key)
	require.True(t, ok)
	assert.True(t, got.Exhausted(), "primary is now spent")
	assert.InDelta(t, 0.5, got.Secondary.UsedPercent, 1e-9, "weekly reading must be preserved, not erased")
}
