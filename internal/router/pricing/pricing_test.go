package pricing_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/router/pricing"
)

func TestFor_KnownModel(t *testing.T) {
	// claude-opus-4-7 is a representative entry in the source-of-truth table.
	// The numbers here mirror Anthropic's published per-1M-token prices and
	// must stay in sync with the OTel attribute emission downstream.
	p, ok := pricing.For("claude-opus-4-7")
	require.True(t, ok, "claude-opus-4-7 should be present in the pricing table")
	assert.Equal(t, 15.00, p.InputUSDPer1M)
	assert.Equal(t, 75.00, p.OutputUSDPer1M)
	assert.Greater(t, p.InputUSDPer1M, 0.0)
	assert.Greater(t, p.OutputUSDPer1M, 0.0)
}

func TestFor_DateSuffixStripped(t *testing.T) {
	// Dated model variants (Anthropic's "-YYYYMMDD" snapshot names) must
	// resolve to the same canonical pricing as their undated counterpart.
	canonical, okCanonical := pricing.For("claude-opus-4-7")
	require.True(t, okCanonical)

	dated, okDated := pricing.For("claude-opus-4-7-20251001")
	require.True(t, okDated, "dated variant should resolve via suffix stripping")
	assert.Equal(t, canonical, dated)
}

func TestFor_UnknownModel(t *testing.T) {
	p, ok := pricing.For("totally-fake-model-name")
	assert.False(t, ok)
	assert.Equal(t, pricing.Pricing{}, p)
}

func TestFor_UnknownModelWithDateSuffix(t *testing.T) {
	// Stripping a date suffix from an unknown stem still yields not-found.
	p, ok := pricing.For("totally-fake-model-20251001")
	assert.False(t, ok)
	assert.Equal(t, pricing.Pricing{}, p)
}

func TestFor_EmptyString(t *testing.T) {
	p, ok := pricing.For("")
	assert.False(t, ok)
	assert.Equal(t, pricing.Pricing{}, p)
}

func TestCacheMultipliers_Positive(t *testing.T) {
	// Sanity-check the Anthropic-published multipliers that the planner will
	// consume. If any of these flip non-positive, the EV math breaks.
	assert.Greater(t, pricing.CacheReadMultiplier, 0.0)
	assert.Less(t, pricing.CacheReadMultiplier, 1.0,
		"cache reads should be cheaper than base input")
	assert.Greater(t, pricing.CacheWriteMultiplier5Min, 1.0,
		"5-min cache writes should cost more than base input")
	assert.Greater(t, pricing.CacheWriteMultiplier1Hour, pricing.CacheWriteMultiplier5Min,
		"1-hour cache writes should cost more than 5-min cache writes")
}

func TestAll_ReturnsCopy(t *testing.T) {
	// Mutating the returned map must not affect subsequent lookups.
	m := pricing.All()
	require.NotEmpty(t, m)
	delete(m, "claude-opus-4-7")

	p, ok := pricing.For("claude-opus-4-7")
	require.True(t, ok, "internal table must be unaffected by caller mutation")
	assert.Equal(t, 15.00, p.InputUSDPer1M)
}
