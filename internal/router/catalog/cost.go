package catalog

import "workweave/router/internal/providers"

// EffectiveInputCost returns the true USD input cost after applying cache
// pricing. Fresh tokens at base rate; cache-creation at 1.25x; cache-read
// at the binding's effective multiplier. upstreamProvider distinguishes
// Anthropic (input_tokens is fresh-only) from OpenAI / Gemini
// (prompt_tokens includes cached tokens — must subtract).
//
// Single source of truth for the proxy's OTel emitter, telemetry write
// path, and the billing debit hook.
func EffectiveInputCost(inputTokens, cacheCreation, cacheRead int, pricePer1M float64, p Pricing, upstreamProvider string) float64 {
	fresh := inputTokens
	if upstreamProvider != providers.ProviderAnthropic {
		fresh = inputTokens - cacheCreation - cacheRead
	}
	if fresh < 0 {
		fresh = 0
	}
	return (float64(fresh) +
		float64(cacheCreation)*1.25 +
		float64(cacheRead)*p.EffectiveCacheReadMultiplier()) / 1_000_000 * pricePer1M
}

// EffectiveOutputCost returns USD output cost for a call. Output tokens
// have no caching multipliers — straight tokens × per-1M price.
func EffectiveOutputCost(outputTokens int, pricePer1M float64) float64 {
	return float64(outputTokens) / 1_000_000 * pricePer1M
}

// CounterfactualInputCost is EffectiveInputCost for the savings baseline, except
// on a model-switch turn (switchPrefillPaid) it reprices the served model's
// cold-cache prefill as a cache read: a session that never switched would have
// kept that context warm, so charging the baseline the prefill overstates
// savings. Off the switch turn it is identical to EffectiveInputCost.
func CounterfactualInputCost(inputTokens, cacheCreation, cacheRead int, pricePer1M float64, p Pricing, upstreamProvider string, switchPrefillPaid bool) float64 {
	if !switchPrefillPaid {
		return EffectiveInputCost(inputTokens, cacheCreation, cacheRead, pricePer1M, p, upstreamProvider)
	}
	// New tokens for a single turn are negligible against the carried context, so
	// fold the whole prefill into cache reads rather than splitting new-vs-carried.
	return EffectiveInputCost(inputTokens, 0, cacheCreation+cacheRead, pricePer1M, p, upstreamProvider)
}
