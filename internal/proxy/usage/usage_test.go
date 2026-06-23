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

func TestParseCodexHeaders_NoneReportsFalse(t *testing.T) {
	_, ok := usage.ParseCodexHeaders(http.Header{})
	assert.False(t, ok)
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

	// Past TTL → expired, dropped.
	now = now.Add(11 * time.Minute)
	_, ok = o.Snapshot(key)
	assert.False(t, ok)
}

func TestObserver_DistinctTokensDistinctKeys(t *testing.T) {
	o := usage.NewObserver([]byte("salt"), time.Minute, func() time.Time { return time.Unix(1, 0) })
	assert.NotEqual(t, o.Key([]byte("token-a")), o.Key([]byte("token-b")))
	assert.Equal(t, o.Key([]byte("token-a")), o.Key([]byte("token-a")))
}
