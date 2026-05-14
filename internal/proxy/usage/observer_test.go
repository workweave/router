package usage_test

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"workweave/router/internal/proxy/usage"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShouldBypassRouting_FeatureDisabled(t *testing.T) {
	o := usage.NewObserver(0.95, 10*time.Minute, false)
	o.RecordObservation("k", usage.Observation{FiveHourUtil: 0.99, WeeklyUtil: 0.99})
	assert.True(t, o.ShouldBypassRouting("k"), "disabled observer must always bypass routing")
}

func TestShouldBypassRouting_EmptyKey(t *testing.T) {
	o := usage.NewObserver(0.95, 10*time.Minute, true)
	assert.True(t, o.ShouldBypassRouting(""))
}

func TestShouldBypassRouting_ColdStart(t *testing.T) {
	o := usage.NewObserver(0.95, 10*time.Minute, true)
	assert.True(t, o.ShouldBypassRouting("never-recorded"))
}

func TestShouldBypassRouting_BothBelowThreshold(t *testing.T) {
	o := usage.NewObserver(0.95, 10*time.Minute, true)
	o.RecordObservation("k", usage.Observation{FiveHourUtil: 0.5, WeeklyUtil: 0.5})
	assert.True(t, o.ShouldBypassRouting("k"))
}

func TestShouldBypassRouting_FiveHourAtThreshold(t *testing.T) {
	o := usage.NewObserver(0.95, 10*time.Minute, true)
	o.RecordObservation("k", usage.Observation{FiveHourUtil: 0.95, WeeklyUtil: 0.1})
	assert.False(t, o.ShouldBypassRouting("k"), "5h at threshold engages routing")
}

func TestShouldBypassRouting_WeeklyAboveThreshold(t *testing.T) {
	o := usage.NewObserver(0.95, 10*time.Minute, true)
	o.RecordObservation("k", usage.Observation{FiveHourUtil: 0.1, WeeklyUtil: 0.96})
	assert.False(t, o.ShouldBypassRouting("k"), "weekly above threshold engages routing")
}

func TestShouldBypassRouting_MissingSignalsTreatedAsBelow(t *testing.T) {
	o := usage.NewObserver(0.95, 10*time.Minute, true)
	// 5h known low, weekly missing on the most recent response.
	o.RecordObservation("k", usage.Observation{FiveHourUtil: 0.2, WeeklyUtil: -1})
	assert.True(t, o.ShouldBypassRouting("k"))
}

func TestRecord_IgnoresNoSignalObservations(t *testing.T) {
	o := usage.NewObserver(0.95, 10*time.Minute, true)
	o.RecordObservation("k", usage.Observation{FiveHourUtil: 0.99, WeeklyUtil: 0.99})
	require.False(t, o.ShouldBypassRouting("k"))
	// A subsequent response that dropped the headers entirely must NOT erase
	// the prior near-limit reading; otherwise a single bad response would
	// silently re-open the bypass.
	o.RecordObservation("k", usage.Observation{FiveHourUtil: -1, WeeklyUtil: -1})
	assert.False(t, o.ShouldBypassRouting("k"), "no-signal record must not overwrite prior near-limit observation")
}

func TestObserver_TTLExpiry(t *testing.T) {
	o := usage.NewObserver(0.95, 25*time.Millisecond, true)
	o.RecordObservation("k", usage.Observation{FiveHourUtil: 0.99, WeeklyUtil: 0.99})
	require.False(t, o.ShouldBypassRouting("k"))
	time.Sleep(50 * time.Millisecond)
	assert.True(t, o.ShouldBypassRouting("k"), "stale observation must fall back to cold-start bypass")
}

func TestParseObservation(t *testing.T) {
	h := http.Header{}
	h.Set(usage.HeaderFiveHourUtil, "0.42")
	h.Set(usage.HeaderWeeklyUtil, "0.87")
	got := usage.ParseObservation(h)
	assert.InDelta(t, 0.42, got.FiveHourUtil, 1e-9)
	assert.InDelta(t, 0.87, got.WeeklyUtil, 1e-9)
	assert.True(t, got.HasSignal())
}

func TestParseObservation_MissingAndMalformed(t *testing.T) {
	h := http.Header{}
	h.Set(usage.HeaderWeeklyUtil, "garbage")
	got := usage.ParseObservation(h)
	assert.Equal(t, -1.0, got.FiveHourUtil)
	assert.Equal(t, -1.0, got.WeeklyUtil)
	assert.False(t, got.HasSignal())
}

func TestCredentialKey_StableAndDistinct(t *testing.T) {
	a := usage.CredentialKey([]byte("sk-ant-key-A"))
	b := usage.CredentialKey([]byte("sk-ant-key-B"))
	require.NotEmpty(t, a)
	require.NotEqual(t, a, b, "different credentials must hash to different keys")
	assert.Equal(t, a, usage.CredentialKey([]byte("sk-ant-key-A")), "same input must hash stably")
	assert.Empty(t, usage.CredentialKey(nil))
}

func TestObserver_ConcurrentRecordAndBypass(t *testing.T) {
	o := usage.NewObserver(0.95, 10*time.Minute, true)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			o.RecordObservation("k", usage.Observation{FiveHourUtil: float64(i%100) / 100.0, WeeklyUtil: 0.5})
		}(i)
		go func() {
			defer wg.Done()
			_ = o.ShouldBypassRouting("k")
		}()
	}
	wg.Wait()
}
