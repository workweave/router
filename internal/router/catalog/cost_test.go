package catalog_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"workweave/router/internal/providers"
	"workweave/router/internal/router/catalog"
)

// opusPricing mirrors the catalog's Claude 4.8 binding: $5/1M input with a 0.10
// cache-read multiplier — the case Andrew's switch-cost example turns on.
var opusPricing = catalog.Pricing{InputUSDPer1M: 5.00, OutputUSDPer1M: 25.00, CacheReadMultiplier: 0.10}

func TestCounterfactualInputCost_NonSwitchEqualsEffective(t *testing.T) {
	// Off a switch turn the counterfactual must be byte-identical to the plain
	// effective cost — the correction only ever applies to the transition turn.
	for _, prov := range []string{providers.ProviderAnthropic, providers.ProviderOpenAI} {
		in, cc, cr := 102_000, 100_000, 1_000
		want := catalog.EffectiveInputCost(in, cc, cr, opusPricing.InputUSDPer1M, opusPricing, prov)
		got := catalog.CounterfactualInputCost(in, cc, cr, opusPricing.InputUSDPer1M, opusPricing, prov, false)
		assert.Equal(t, want, got, "provider %s", prov)
	}
}

func TestCounterfactualInputCost_SwitchRepricesPrefillAsCacheRead(t *testing.T) {
	// Anthropic reports input_tokens as fresh-only, so cacheCreation is separate.
	// A cold switch turn: 100k prefill + 2k genuinely-new fresh tokens.
	freshOnly, cc, cr := 2_000, 100_000, 0

	// Actual (cold): fresh + prefill at 1.25x.
	cold := catalog.EffectiveInputCost(freshOnly, cc, cr, opusPricing.InputUSDPer1M, opusPricing, providers.ProviderAnthropic)
	assert.InDelta(t, 0.635, cold, 1e-9)

	// Counterfactual (baseline stayed warm): prefill repriced at the 0.10 read
	// multiplier, so the savings baseline no longer eats the router's switch cost.
	warm := catalog.CounterfactualInputCost(freshOnly, cc, cr, opusPricing.InputUSDPer1M, opusPricing, providers.ProviderAnthropic, true)
	assert.InDelta(t, 0.060, warm, 1e-9)
	assert.Less(t, warm, cold, "counterfactual must fall below the cold-served cost on a switch turn")
}

func TestCounterfactualInputCost_SwitchOpenAIProviderMatchesAnthropic(t *testing.T) {
	// OpenAI/Gemini fold cached tokens into input_tokens; EffectiveInputCost
	// subtracts them back out. Repricing the prefill must land on the same number
	// regardless of which token-accounting shape the served provider used.
	anthropic := catalog.CounterfactualInputCost(2_000, 100_000, 0, opusPricing.InputUSDPer1M, opusPricing, providers.ProviderAnthropic, true)
	openai := catalog.CounterfactualInputCost(102_000, 100_000, 0, opusPricing.InputUSDPer1M, opusPricing, providers.ProviderOpenAI, true)
	assert.InDelta(t, anthropic, openai, 1e-9)
}

func TestCounterfactualInputCost_SwitchWithNoPrefillIsNoOp(t *testing.T) {
	// A transition turn that happened to carry no cacheCreation (e.g. cache still
	// warm on switch-back) has nothing to reprice — equals the effective cost.
	in, cc, cr := 3_000, 0, 50_000
	want := catalog.EffectiveInputCost(in, cc, cr, opusPricing.InputUSDPer1M, opusPricing, providers.ProviderAnthropic)
	got := catalog.CounterfactualInputCost(in, cc, cr, opusPricing.InputUSDPer1M, opusPricing, providers.ProviderAnthropic, true)
	assert.Equal(t, want, got)
}
