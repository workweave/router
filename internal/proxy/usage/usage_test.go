package usage_test

import (
	"net/http"
	"testing"
	"time"

	"workweave/router/internal/proxy/usage"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCodexHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "40")
	h.Set("x-codex-primary-window-minutes", "300")
	h.Set("x-codex-secondary-used-percent", "12.5")
	h.Set("x-codex-secondary-window-minutes", "10080")

	snap, ok := usage.ParseCodexHeaders(h)
	require.True(t, ok)
	assert.InDelta(t, 0.40, snap.Primary.UsedPercent, 1e-9)
	assert.Equal(t, 300, snap.Primary.WindowMinutes)
	assert.InDelta(t, 0.125, snap.Secondary.UsedPercent, 1e-9)
	assert.Equal(t, 10080, snap.Secondary.WindowMinutes)
}

func TestParseCodexHeaders_LowUsageNotMisread(t *testing.T) {
	// "1" means 1% used (max headroom), not 100% — must normalize to 0.01 so the
	// subsidy isn't silently wiped out at the moment the window is freshest.
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "1")
	h.Set("x-codex-primary-window-minutes", "300")
	snap, ok := usage.ParseCodexHeaders(h)
	require.True(t, ok)
	assert.InDelta(t, 0.01, snap.Primary.UsedPercent, 1e-9)
	// And that low usage yields a near-epsilon cost factor (covered model ~free).
	assert.Less(t, snap.CostFactor(0.05, 2.0), 0.06)
	// A value above 100 clamps to fully used.
	h.Set("x-codex-primary-used-percent", "150")
	snap, _ = usage.ParseCodexHeaders(h)
	assert.InDelta(t, 1.0, snap.Primary.UsedPercent, 1e-9)
}

func TestParseCodexHeaders_NoneReportsFalse(t *testing.T) {
	_, ok := usage.ParseCodexHeaders(http.Header{})
	assert.False(t, ok)
}

// When Codex reports used-percent but omits window-minutes, the parser must
// still supply the known window length (primary ~5h, secondary weekly) so a
// near-cap reading stays authoritative for the window's life instead of aging
// out after the short ttl floor and re-subsidizing a still-capped credential.
func TestParseCodexHeaders_DefaultsWindowWhenOmitted(t *testing.T) {
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "80")
	h.Set("x-codex-secondary-used-percent", "97")
	snap, ok := usage.ParseCodexHeaders(h)
	require.True(t, ok)
	assert.Equal(t, 300, snap.Primary.WindowMinutes, "primary defaults to ~5h")
	assert.Equal(t, 10080, snap.Secondary.WindowMinutes, "secondary defaults to weekly")
}

func TestParseAnthropicUnified_FromRemainingLimit(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-limit", "1000")
	h.Set("anthropic-ratelimit-unified-5h-remaining", "250") // 75% used
	h.Set("anthropic-ratelimit-unified-weekly-limit", "100000")
	h.Set("anthropic-ratelimit-unified-weekly-remaining", "90000") // 10% used

	snap, ok := usage.ParseAnthropicUnifiedHeaders(h)
	require.True(t, ok)
	assert.InDelta(t, 0.75, snap.Primary.UsedPercent, 1e-9)
	assert.InDelta(t, 0.10, snap.Secondary.UsedPercent, 1e-9)
}

func TestParseAnthropicUnified_PrefersUtilization(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-utilization", "63")
	snap, ok := usage.ParseAnthropicUnifiedHeaders(h)
	require.True(t, ok)
	assert.InDelta(t, 0.63, snap.Primary.UsedPercent, 1e-9)
}

func TestCostFactor_SlackIsCheap_BindingIsFullPrice(t *testing.T) {
	const eps, gamma = 0.05, 2.0

	// No data → no subsidy (full price), so we never subsidize blind.
	assert.Equal(t, 1.0, usage.Snapshot{}.CostFactor(eps, gamma))

	// Lots of slack → near epsilon (covered model ~free).
	slack := usage.Snapshot{Primary: usage.Window{UsedPercent: 0.05, WindowMinutes: 300}}
	assert.Less(t, slack.CostFactor(eps, gamma), 0.10)

	// Fully consumed → full catalog price (no subsidy as the cap binds).
	binding := usage.Snapshot{Primary: usage.Window{UsedPercent: 1.0, WindowMinutes: 300}}
	assert.InDelta(t, 1.0, binding.CostFactor(eps, gamma), 1e-9)

	// Monotonic non-decreasing in utilization.
	prev := 0.0
	for _, u := range []float64{0, 0.2, 0.4, 0.6, 0.8, 1.0} {
		f := usage.Snapshot{Primary: usage.Window{UsedPercent: u, WindowMinutes: 300}}.CostFactor(eps, gamma)
		assert.GreaterOrEqual(t, f, prev)
		assert.GreaterOrEqual(t, f, eps)
		assert.LessOrEqual(t, f, 1.0)
		prev = f
	}
}

func TestCostFactor_TighterWindowGoverns(t *testing.T) {
	const eps, gamma = 0.05, 2.0
	// Weekly nearly exhausted even though the 5h window is fresh → high factor.
	s := usage.Snapshot{
		Primary:   usage.Window{UsedPercent: 0.10, WindowMinutes: 300},
		Secondary: usage.Window{UsedPercent: 0.95, WindowMinutes: 10080},
	}
	assert.Greater(t, s.CostFactor(eps, gamma), 0.8)
}

func TestObserver_RecordGetTTL(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	clock := func() time.Time { return now }
	o := usage.NewObserver([]byte("salt"), 10*time.Minute, clock)

	key := o.Key([]byte("sk-ant-oat01-abc"))
	o.Record(key, usage.Snapshot{Primary: usage.Window{UsedPercent: 0.5, WindowMinutes: 300}})

	got, ok := o.Snapshot(key)
	require.True(t, ok)
	assert.InDelta(t, 0.5, got.Primary.UsedPercent, 1e-9)

	// Empty observation must not be stored / must not clobber.
	o.Record(key, usage.Snapshot{})
	got, ok = o.Snapshot(key)
	require.True(t, ok)
	assert.InDelta(t, 0.5, got.Primary.UsedPercent, 1e-9)

	// A short idle gap (past the 10-min floor) must NOT drop a reading whose
	// quota window (5h) is still open — its headroom is still authoritative.
	now = now.Add(11 * time.Minute)
	_, ok = o.Snapshot(key)
	assert.True(t, ok, "a 5h-window reading survives a short idle gap")

	// Past the binding window → quota has reset → expired, dropped.
	now = now.Add(300 * time.Minute)
	_, ok = o.Snapshot(key)
	assert.False(t, ok)
}

// TestObserver_NearCapDoesNotResetToOptimistic is the regression for the
// reviewer-flagged bug: a credential observed near its cap must not age out after
// a short idle gap and then read as cold-start slack (which would re-subsidize a
// still-capped subscription). Its near-1.0 factor must persist for the life of
// the binding window, and only after that window resets should the entry drop so
// the cold-start path can legitimately treat it as never-observed again.
func TestObserver_NearCapDoesNotResetToOptimistic(t *testing.T) {
	const eps, gamma = 0.05, 2.0
	now := time.Unix(2_000_000, 0)
	clock := func() time.Time { return now }
	o := usage.NewObserver([]byte("salt"), 10*time.Minute, clock)
	key := o.Key([]byte("sk-ant-oat01-capped"))

	// Weekly window nearly exhausted.
	o.Record(key, usage.Snapshot{Secondary: usage.Window{UsedPercent: 0.98, WindowMinutes: 10080}})

	// 11 minutes later (past the old flat TTL): still observed, still ~full price.
	now = now.Add(11 * time.Minute)
	snap, ok := o.Snapshot(key)
	require.True(t, ok, "a near-cap reading must survive past the 10-min floor")
	assert.Greater(t, snap.CostFactor(eps, gamma), 0.9, "still near full price, not optimistic epsilon")

	// After the weekly window elapses, the quota has reset → drop → cold start.
	now = now.Add(10080 * time.Minute)
	_, ok = o.Snapshot(key)
	assert.False(t, ok, "after the binding window resets, the reading is no longer authoritative")
}

// TestObserver_LongWindowOutlivesShortBindingWindow guards the case where the 5h
// primary window is the more-utilized (binding) one but the weekly window is also
// near cap: the entry must survive past the 5h window so it does not reset to
// optimistic epsilon while weekly quota is still exhausted.
func TestObserver_LongWindowOutlivesShortBindingWindow(t *testing.T) {
	now := time.Unix(3_000_000, 0)
	clock := func() time.Time { return now }
	o := usage.NewObserver([]byte("salt"), 10*time.Minute, clock)
	key := o.Key([]byte("tok"))
	o.Record(key, usage.Snapshot{
		Primary:   usage.Window{UsedPercent: 0.99, WindowMinutes: 300},   // binds CostFactor
		Secondary: usage.Window{UsedPercent: 0.90, WindowMinutes: 10080}, // also near cap
	})

	// 6h later: past the 5h primary window, but the weekly window still binds.
	now = now.Add(6 * 60 * time.Minute)
	_, ok := o.Snapshot(key)
	assert.True(t, ok, "a near-cap weekly window keeps the entry alive past the 5h primary")
}

// TestObserver_SlackWindowDoesNotStrand is the converse: a 5h-capped reading whose
// weekly window is slack must expire at ~5h, not be held at full price for the
// (much longer) weekly window — otherwise a recovered primary quota would be
// stranded on cash/OSS for a week.
func TestObserver_SlackWindowDoesNotStrand(t *testing.T) {
	now := time.Unix(4_000_000, 0)
	clock := func() time.Time { return now }
	o := usage.NewObserver([]byte("salt"), 10*time.Minute, clock)
	key := o.Key([]byte("tok"))
	o.Record(key, usage.Snapshot{
		Primary:   usage.Window{UsedPercent: 0.99, WindowMinutes: 300},   // capped, 5h
		Secondary: usage.Window{UsedPercent: 0.05, WindowMinutes: 10080}, // slack
	})

	// Just past the 5h primary window: the slack weekly window must not keep the
	// stale primary-capped reading alive.
	now = now.Add(301 * time.Minute)
	_, ok := o.Snapshot(key)
	assert.False(t, ok, "a slack long window must not strand a recovered short-window quota")
}

func TestObserver_RecordMergesWindows(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	o := usage.NewObserver([]byte("salt"), 10*time.Minute, func() time.Time { return now })
	key := o.Key([]byte("tok"))

	// First response reports both windows; weekly is nearly exhausted.
	o.Record(key, usage.Snapshot{
		Primary:   usage.Window{UsedPercent: 0.10, WindowMinutes: 300},
		Secondary: usage.Window{UsedPercent: 0.95, WindowMinutes: 10080},
	})
	// A later response reports ONLY the primary window (secondary omitted).
	o.Record(key, usage.Snapshot{Primary: usage.Window{UsedPercent: 0.20, WindowMinutes: 300}})

	got, ok := o.Snapshot(key)
	require.True(t, ok)
	assert.InDelta(t, 0.20, got.Primary.UsedPercent, 1e-9, "primary updates")
	assert.InDelta(t, 0.95, got.Secondary.UsedPercent, 1e-9,
		"omitted secondary window must NOT be erased to slack")
}

func TestObserver_DistinctTokensDistinctKeys(t *testing.T) {
	o := usage.NewObserver([]byte("salt"), time.Minute, func() time.Time { return time.Unix(1, 0) })
	assert.NotEqual(t, o.Key([]byte("token-a")), o.Key([]byte("token-b")))
	assert.Equal(t, o.Key([]byte("token-a")), o.Key([]byte("token-a")))
}

func TestSnapshot_Exhausted(t *testing.T) {
	t.Run("no data is never exhausted", func(t *testing.T) {
		assert.False(t, usage.Snapshot{}.Exhausted(),
			"absence of a reading is cold-start slack, not a spent plan")
	})
	t.Run("slack windows are not exhausted", func(t *testing.T) {
		s := usage.Snapshot{
			Primary:   usage.Window{UsedPercent: 0.50, WindowMinutes: 300},
			Secondary: usage.Window{UsedPercent: 0.95, WindowMinutes: 10080},
		}
		assert.False(t, s.Exhausted(), "95% still has headroom — the token can still serve")
	})
	t.Run("weekly window at cap is exhausted", func(t *testing.T) {
		s := usage.Snapshot{
			Primary:   usage.Window{UsedPercent: 0.10, WindowMinutes: 300},
			Secondary: usage.Window{UsedPercent: 1.0, WindowMinutes: 10080},
		}
		assert.True(t, s.Exhausted(), "a bound weekly window means the upstream 429s")
	})
	t.Run("primary (5h) window at cap is exhausted", func(t *testing.T) {
		s := usage.Snapshot{Primary: usage.Window{UsedPercent: 1.0, WindowMinutes: 300}}
		assert.True(t, s.Exhausted())
	})
	t.Run("rounding just under 1.0 still reads exhausted", func(t *testing.T) {
		s := usage.Snapshot{Secondary: usage.Window{UsedPercent: 0.999, WindowMinutes: 10080}}
		assert.True(t, s.Exhausted(),
			"integer-percent rounding at the cap must not read as headroom")
	})
}

func TestParseAnthropicUnifiedHeaders_ResetAt(t *testing.T) {
	t.Run("RFC3339 reset", func(t *testing.T) {
		h := http.Header{}
		h.Set("anthropic-ratelimit-unified-weekly-utilization", "100")
		h.Set("anthropic-ratelimit-unified-weekly-reset", "2026-06-28T03:00:00Z")
		snap, ok := usage.ParseAnthropicUnifiedHeaders(h)
		require.True(t, ok)
		assert.Equal(t, "2026-06-28T03:00:00Z", snap.Secondary.ResetAt.Format(time.RFC3339))
	})
	t.Run("unix-seconds reset fallback", func(t *testing.T) {
		h := http.Header{}
		h.Set("anthropic-ratelimit-unified-5h-utilization", "90")
		h.Set("anthropic-ratelimit-unified-5h-reset", "1782702000")
		snap, ok := usage.ParseAnthropicUnifiedHeaders(h)
		require.True(t, ok)
		assert.Equal(t, int64(1782702000), snap.Primary.ResetAt.Unix())
	})
	t.Run("absent reset leaves zero", func(t *testing.T) {
		h := http.Header{}
		h.Set("anthropic-ratelimit-unified-weekly-utilization", "50")
		snap, ok := usage.ParseAnthropicUnifiedHeaders(h)
		require.True(t, ok)
		assert.True(t, snap.Secondary.ResetAt.IsZero())
	})
}

// TestObserver_ResetAtExpiresBeforeWindowLength is the failover re-probe fix: an
// exhausted weekly reading whose upstream reset is hours away must expire at that
// reset, NOT a full 7-day window length from when it was observed — otherwise the
// exhaustion suppression strands the subscription on the Weave key for days after
// the plan has already refilled.
func TestObserver_ResetAtExpiresBeforeWindowLength(t *testing.T) {
	base := time.Unix(1_800_000_000, 0).UTC()
	clock := base
	o := usage.NewObserver([]byte("salt"), 10*time.Minute, func() time.Time { return clock })
	key := o.Key([]byte("tok"))

	// Exhausted weekly window, but the plan resets in 2 hours.
	o.Record(key, usage.Snapshot{
		Secondary: usage.Window{UsedPercent: 1.0, WindowMinutes: 10080, ResetAt: base.Add(2 * time.Hour)},
	})

	// 1 hour later (before reset): still authoritative → still exhausted.
	clock = base.Add(1 * time.Hour)
	snap, ok := o.Snapshot(key)
	require.True(t, ok, "reading must survive until its reset")
	assert.True(t, snap.Exhausted())

	// 3 hours later (past reset): evicted, so the credential reads as never-observed
	// and the next turn re-probes on the subscription instead of staying suppressed.
	clock = base.Add(3 * time.Hour)
	_, ok = o.Snapshot(key)
	assert.False(t, ok, "reading must expire at the reset, not 7 days after observation")
}

func TestObserver_NoResetFallsBackToWindowLength(t *testing.T) {
	base := time.Unix(1_800_000_000, 0).UTC()
	clock := base
	o := usage.NewObserver([]byte("salt"), 10*time.Minute, func() time.Time { return clock })
	key := o.Key([]byte("tok"))
	// Exhausted weekly window, no reset reported → retained for the full week.
	o.Record(key, usage.Snapshot{Secondary: usage.Window{UsedPercent: 1.0, WindowMinutes: 10080}})

	clock = base.Add(6 * 24 * time.Hour) // 6 days: still inside the 7-day window
	_, ok := o.Snapshot(key)
	assert.True(t, ok, "with no reset header the window-length horizon still applies")
}
