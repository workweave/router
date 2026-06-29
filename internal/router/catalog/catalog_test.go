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
		providers.ProviderMakora:     {},
		providers.ProviderTogether:   {},
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

func TestAgenticLowSet_IncludesHarnessIncapableModels(t *testing.T) {
	// These models emit valid tool calls (so they are not ToolUseLow) but can't
	// sustain an agentic harness loop, so they must be dropped from has_tools
	// turns — otherwise a price-leaning dial demotes Opus straight onto one of
	// them (minimax-m3 grepping for a skill instead of running it is the
	// canonical failure). If this fires, a row was unflagged or renamed.
	set := AgenticLowSet()
	for _, id := range []string{
		"minimax/minimax-m3",
		"qwen/qwen3-next-80b-a3b-instruct",
		"gemini-3.1-flash-lite-preview",
		"deepseek/deepseek-v4-flash",
	} {
		_, found := set[id]
		assert.Truef(t, found, "%s must be marked AgenticLow", id)
	}
}

func TestAgenticLowSet_OmitsHarnessCapableModels(t *testing.T) {
	// The harness-capable demotion ladder must stay eligible on has_tools turns:
	// Opus -> Sonnet -> the cheaper capable coders. haiku stays eligible too (it
	// is a legitimate cheap tool model), so it must NOT be flagged.
	set := AgenticLowSet()
	for _, id := range []string{
		"claude-opus-4-8",
		"claude-sonnet-4-6",
		"z-ai/glm-5.2",
		"deepseek/deepseek-v4-pro",
		"moonshotai/kimi-k2.6",
		"qwen/qwen3-coder-next",
		"claude-haiku-4-5",
	} {
		_, found := set[id]
		assert.Falsef(t, found, "%s must NOT be in the AgenticLow set", id)
	}
}

func TestModel_AgenticUseDefaultsToUnknown(t *testing.T) {
	// Zero-value safety: an unset AgenticUse must default to AgenticUnknown so
	// the scorer treats the model as harness-capable until proven otherwise.
	// Guards against a future iota reorder flipping every row to AgenticLow.
	var m Model
	assert.Equal(t, AgenticUnknown, m.AgenticUse)
}

func TestImageUnsupportedSet_IncludesTextOnlyModels(t *testing.T) {
	// Text-only OSS models reject image content parts with a 4xx (DeepInfra
	// 405 "does not accept image input" on GLM-5.1 is the canonical case).
	// They must be flagged so the scorer keeps image-bearing turns off them.
	set := ImageUnsupportedSet()
	for _, id := range []string{"z-ai/glm-5.1", "z-ai/glm-5", "deepseek/deepseek-v4-pro", "moonshotai/kimi-k2.6", "qwen/qwen3-coder"} {
		_, found := set[id]
		assert.Truef(t, found, "%s must be flagged ImageInputUnsupported", id)
	}
}

func TestImageUnsupportedSet_OmitsMultimodalModels(t *testing.T) {
	// First-party models (Anthropic / OpenAI / Google) are all multimodal and
	// must keep the default. mistral-small-2603 is a multimodal OSS row and is
	// deliberately left unflagged.
	set := ImageUnsupportedSet()
	for _, id := range []string{"claude-opus-4-7", "gpt-5.5", "gemini-3.1-pro-preview", "mistralai/mistral-small-2603"} {
		_, found := set[id]
		assert.Falsef(t, found, "%s must NOT be flagged ImageInputUnsupported", id)
	}
}

func TestAcceptsImages(t *testing.T) {
	assert.False(t, AcceptsImages("z-ai/glm-5.1"), "text-only model rejects images")
	assert.True(t, AcceptsImages("claude-opus-4-7"), "multimodal model accepts images")
	// Unknown models default to image-capable so an unrecognized passthrough or
	// force-model target is never wrongly evicted from an image-bearing turn.
	assert.True(t, AcceptsImages("some-future-model"), "unknown model defaults to image-capable")
}

func TestModel_ImageInputDefaultsToUnknown(t *testing.T) {
	// Zero-value safety: an unset ImageInput must default to ImageInputUnknown
	// (treated as image-capable) so a new first-party model is never silently
	// excluded from image turns.
	var m Model
	assert.Equal(t, ImageInputUnknown, m.ImageInput)
}

func TestContextWindowFor_KnownModels(t *testing.T) {
	// Anthropic models report 200K in the catalog; they support 1M via context-1m-2025-08-07
	// beta when explicitly requested by the client (see contextWindowForRequest in proxy/service.go).
	assert.Equal(t, 200_000, ContextWindowFor("claude-opus-4-8"))
	assert.Equal(t, 200_000, ContextWindowFor("claude-sonnet-4-6"))
	assert.Equal(t, 200_000, ContextWindowFor("claude-haiku-4-5"))
	// GPT-5 family has large context windows.
	assert.Equal(t, 400_000, ContextWindowFor("gpt-5"))
	assert.Equal(t, 1_000_000, ContextWindowFor("gpt-5.4"))
	assert.Equal(t, 1_050_000, ContextWindowFor("gpt-5.5"))
	// GPT-4.1 family has 1M context.
	assert.Equal(t, 1_047_576, ContextWindowFor("gpt-4.1"))
	// Gemini models have 1M context.
	assert.Equal(t, 1_048_576, ContextWindowFor("gemini-3.5-flash"))
	// DeepSeek V4 (Flash + Pro) serves the full 1,048,576-token window.
	assert.Equal(t, 1_048_576, ContextWindowFor("deepseek/deepseek-v4-pro"))
	assert.Equal(t, 1_048_576, ContextWindowFor("deepseek/deepseek-v4-flash"))
	// Most OSS models serve a 256K window (Qwen3 / Kimi families).
	assert.Equal(t, 262_144, ContextWindowFor("moonshotai/kimi-k2.5"))
	// GLM-5 serves ~200K (max_position_embeddings 202752); MiniMax M2.7 is 204800.
	assert.Equal(t, 202_752, ContextWindowFor("z-ai/glm-5"))
	assert.Equal(t, 204_800, ContextWindowFor("minimax/minimax-m2.7"))
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
