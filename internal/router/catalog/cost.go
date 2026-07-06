package catalog

import (
	"math"

	"workweave/router/internal/providers"
)

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

// USDToMicros rounds a float64 USD value to BIGINT micros (USD x 1e6) for
// persistence/debit math. NaN, Inf, and negative values collapse to 0 — we
// never want to write nonsense or debit/charge a negative amount.
//
// Single source of truth for the billing debit hook's notional-cost math
// and the telemetry write path's stored cost columns; both used to
// hand-roll this rounding independently.
func USDToMicros(f float64) int64 {
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0
	}
	return int64(math.Round(f * 1_000_000))
}
