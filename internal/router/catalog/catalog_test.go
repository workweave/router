package catalog

import (
	"testing"

	"workweave/router/internal/providers"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCatalog_NoDuplicateIDs(t *testing.T) {
	seen := make(map[string]struct{}, len(Models))
	for _, m := range Models {
		_, dup := seen[m.ID]
		require.False(t, dup, "duplicate model ID %q in catalog", m.ID)
		seen[m.ID] = struct{}{}
	}
}

func TestCatalog_EveryModelHasAtLeastOneBinding(t *testing.T) {
	for _, m := range Models {
		require.NotEmpty(t, m.Providers, "model %q has empty Providers list", m.ID)
	}
}

func TestCatalog_BindingsReferenceCanonicalProviders(t *testing.T) {
	known := map[string]struct{}{
		providers.ProviderAnthropic:  {},
		providers.ProviderOpenAI:     {},
		providers.ProviderGoogle:     {},
		providers.ProviderOpenRouter: {},
		providers.ProviderFireworks:  {},
		providers.ProviderDeepInfra:  {},
		providers.ProviderBedrock:    {},
	}
	for _, m := range Models {
		for i, b := range m.Providers {
			_, ok := known[b.Provider]
			require.Truef(t, ok, "model %q binding %d uses unknown provider %q", m.ID, i, b.Provider)
		}
	}
}

func TestCatalog_BindingsHavePositivePrice(t *testing.T) {
	for _, m := range Models {
		for i, b := range m.Providers {
			assert.Greaterf(t, b.Price.InputUSDPer1M, 0.0, "%s binding %d (%s) has non-positive InputUSDPer1M", m.ID, i, b.Provider)
			assert.Greaterf(t, b.Price.OutputUSDPer1M, 0.0, "%s binding %d (%s) has non-positive OutputUSDPer1M", m.ID, i, b.Provider)
		}
	}
}

func TestByID_DateStrippedFallback(t *testing.T) {
	// claude-opus-4-7-20251001 should resolve to claude-opus-4-7.
	m, ok := ByID("claude-opus-4-7-20251001")
	require.True(t, ok)
	assert.Equal(t, "claude-opus-4-7", m.ID)
}

func TestByID_UnknownReturnsFalse(t *testing.T) {
	_, ok := ByID("definitely-not-a-model")
	assert.False(t, ok)
}

func TestPriceFor_UnknownProviderForKnownModel(t *testing.T) {
	// claude-opus-4-7 is anthropic-only — asking for openai must miss.
	_, ok := PriceFor(providers.ProviderOpenAI, "claude-opus-4-7")
	assert.False(t, ok)
}

func TestPriceFor_KnownPair(t *testing.T) {
	p, ok := PriceFor(providers.ProviderAnthropic, "claude-opus-4-7")
	require.True(t, ok)
	assert.Equal(t, 5.00, p.InputUSDPer1M)
	assert.Equal(t, 0.10, p.CacheReadMultiplier)
}

func TestResolveBinding_PicksFirstAvailable(t *testing.T) {
	// Hypothetical: build a synthetic model with ordered fallback to
	// verify the resolver respects ordering. Use a real one: claude-opus-4-7
	// only has anthropic — should only resolve when anthropic is available.
	avail := map[string]struct{}{providers.ProviderAnthropic: {}}
	b, ok := ResolveBinding("claude-opus-4-7", avail)
	require.True(t, ok)
	assert.Equal(t, providers.ProviderAnthropic, b.Provider)

	availNoAnthropic := map[string]struct{}{providers.ProviderOpenAI: {}}
	_, ok = ResolveBinding("claude-opus-4-7", availNoAnthropic)
	assert.False(t, ok)
}

func TestTierFor_KnownAndUnknown(t *testing.T) {
	assert.Equal(t, TierHigh, TierFor("claude-opus-4-7"))
	assert.Equal(t, TierLow, TierFor("claude-haiku-4-5"))
	assert.Equal(t, TierUnknown, TierFor("definitely-not-a-model"))
}

func TestAllowedAtOrBelow_FiltersOutUnknownTier(t *testing.T) {
	allowed := AllowedAtOrBelow(TierMid)
	// claude-haiku-4-5 (Low) and claude-sonnet-4-5 (Mid) should be in.
	_, low := allowed["claude-haiku-4-5"]
	_, mid := allowed["claude-sonnet-4-5"]
	_, high := allowed["claude-opus-4-7"]
	assert.True(t, low)
	assert.True(t, mid)
	assert.False(t, high)
}

func TestToolUseLowSet_IncludesQwen3_235BInstruct(t *testing.T) {
	// Production traffic on 2026-05-23 saw Instruct-2507 emit narrative
	// "I edited the file" without tool_use blocks — flagged ToolUseLow so
	// the cluster scorer excludes it from agentic argmax. If this assertion
	// fires, either the entry was downgraded back to ToolUseUnknown or the
	// catalog row was renamed.
	set := ToolUseLowSet()
	_, found := set["qwen/qwen3-235b-a22b-2507"]
	assert.True(t, found, "qwen/qwen3-235b-a22b-2507 must be marked ToolUseLow")
}

func TestToolUseLowSet_OmitsHealthyModels(t *testing.T) {
	set := ToolUseLowSet()
	for _, id := range []string{"claude-opus-4-7", "deepseek/deepseek-v4-pro", "moonshotai/kimi-k2.5"} {
		_, found := set[id]
		assert.Falsef(t, found, "%s must NOT be in the ToolUseLow set", id)
	}
}

func TestModel_ToolUseQualityDefaultsToUnknown(t *testing.T) {
	// Zero-value safety: an unset ToolUseQuality must default to
	// ToolUseUnknown so the scorer treats the model as healthy until
	// proven otherwise. Guards against a future iota reorder that would
	// silently flip every catalog row to ToolUseLow.
	var m Model
	assert.Equal(t, ToolUseUnknown, m.ToolUseQuality)
}

func TestContextWindowFor_KnownModels(t *testing.T) {
	// Anthropic models have 200K context.
	assert.Equal(t, 200_000, ContextWindowFor("claude-opus-4-8"))
	assert.Equal(t, 200_000, ContextWindowFor("claude-haiku-4-5"))
	// GPT-4.1 family has 1M context.
	assert.Equal(t, 1_047_576, ContextWindowFor("gpt-4.1"))
	// OSS models have 128K context.
	assert.Equal(t, 131_072, ContextWindowFor("deepseek/deepseek-v4-pro"))
	assert.Equal(t, 131_072, ContextWindowFor("moonshotai/kimi-k2.5"))
	// Unknown model falls back to DefaultContextWindow.
	assert.Equal(t, DefaultContextWindow, ContextWindowFor("not-a-real-model"))
}

func TestValidateDeployed_FlagsMissingAndUntiered(t *testing.T) {
	err := ValidateDeployed([]string{"claude-opus-4-7"})
	assert.NoError(t, err)

	err = ValidateDeployed([]string{"definitely-not-a-model"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "definitely-not-a-model")

	// gpt-4o is priced but has no tier (passthrough only); flag it.
	err = ValidateDeployed([]string{"gpt-4o"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "gpt-4o")
}
