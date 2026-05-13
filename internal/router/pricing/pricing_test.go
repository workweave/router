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

func TestCacheReadMultiplier_PerProvider(t *testing.T) {
	// Per-provider published cache-read multipliers reach the planner via the
	// Pricing table. Cross-provider switches use these to compute the right
	// EV math; a regression here would silently miscompute non-Anthropic
	// switches by the multiplier ratio (e.g. ~5× for Anthropic vs OpenAI).
	cases := []struct {
		model    string
		expected float64
		provider string
	}{
		{"claude-opus-4-7", 0.10, "Anthropic"},
		{"gpt-5", 0.50, "OpenAI"},
		{"gpt-4.1", 0.50, "OpenAI legacy"},
		{"gemini-3.1-pro-preview", 0.25, "Google"},
		{"deepseek/deepseek-v4-pro", 0.10, "DeepSeek"},
	}
	for _, tc := range cases {
		p, ok := pricing.For(tc.model)
		require.True(t, ok, tc.model)
		assert.InDelta(t, tc.expected, p.CacheReadMultiplier, 1e-9,
			"%s cache-read multiplier should be %.2f", tc.provider, tc.expected)
		assert.InDelta(t, tc.expected, p.EffectiveCacheReadMultiplier(), 1e-9,
			"EffectiveCacheReadMultiplier should return the explicit value when set")
	}
}

func TestEffectiveCacheReadMultiplier_FallsBackToDefault(t *testing.T) {
	// Models without published cache pricing leave CacheReadMultiplier zero
	// and must fall back to the package default so eviction cost in the EV
	// math never zeroes out (which would make every switch look free).
	p, ok := pricing.For("moonshotai/kimi-k2.5")
	require.True(t, ok)
	assert.Zero(t, p.CacheReadMultiplier,
		"OSS model with no published cache pricing should leave multiplier zero")
	assert.InDelta(t, pricing.DefaultCacheReadMultiplier,
		p.EffectiveCacheReadMultiplier(), 1e-9,
		"EffectiveCacheReadMultiplier should return DefaultCacheReadMultiplier")
}

func TestDefaultCacheReadMultiplier_InValidRange(t *testing.T) {
	// Must be positive (else eviction cost zeroes out and switches look free)
	// and strictly less than 1.0 (else cache reads aren't cheaper than base).
	assert.Greater(t, pricing.DefaultCacheReadMultiplier, 0.0)
	assert.Less(t, pricing.DefaultCacheReadMultiplier, 1.0,
		"default cache-read multiplier must be < 1.0 to model a real cache discount")
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
