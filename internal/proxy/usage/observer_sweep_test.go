package usage

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestObserver_EvictsStaleEntriesOnWrite guards the regression where the
// entries map grew without bound: Latest treated stale entries as cold
// start but never deleted them, so a long-running router accumulated one
// entry per distinct credential it had ever seen.
func TestObserver_EvictsStaleEntriesOnWrite(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	o := NewObserver(0.95, 1*time.Minute, true)
	o.now = func() time.Time { return now }

	o.RecordObservation("stale", Observation{FiveHourUtil: 0.5, WeeklyUtil: 0.5})
	now = now.Add(2 * time.Minute)
	o.RecordObservation("fresh", Observation{FiveHourUtil: 0.1, WeeklyUtil: 0.1})

	o.mu.RLock()
	defer o.mu.RUnlock()
	_, staleStillPresent := o.entries["stale"]
	_, freshPresent := o.entries["fresh"]
	assert.False(t, staleStillPresent, "stale entry must be evicted on the next write past TTL")
	assert.True(t, freshPresent, "fresh entry must remain")
	assert.Equal(t, 1, len(o.entries), "entries map must not grow unbounded")
}
