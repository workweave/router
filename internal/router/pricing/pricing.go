// Package pricing exposes per-model input/output USD pricing for inner-ring
// consumers (planner's EV math, OTel attributes). Pure data + lookup
// helpers; no I/O.
package pricing

import "maps"

// Pricing holds the per-1M-token USD costs for a single model.
type Pricing struct {
	InputUSDPer1M  float64
	OutputUSDPer1M float64
	// CacheReadMultiplier is the per-model cost of a cache hit relative to
	// base input price (e.g. 0.10 for Anthropic, 0.50 for OpenAI).
	// Zero means "unspecified — use DefaultCacheReadMultiplier".
	CacheReadMultiplier float64
}

// DefaultCacheReadMultiplier is the fallback multiplier for models without
// published cache pricing. 0.5 is conservative: high enough to not treat
// unknown providers as free caching, low enough to not block switches.
const DefaultCacheReadMultiplier = 0.5

// EffectiveCacheReadMultiplier returns CacheReadMultiplier if set, else
// DefaultCacheReadMultiplier.
func (p Pricing) EffectiveCacheReadMultiplier() float64 {
	if p.CacheReadMultiplier > 0 {
		return p.CacheReadMultiplier
	}
	return DefaultCacheReadMultiplier
}

// All returns a copy of the full pricing table keyed by model name.
func All() map[string]Pricing {
	out := make(map[string]Pricing, len(table))
	maps.Copy(out, table)
	return out
}

var table = map[string]Pricing{
	// Anthropic (cache reads at 10% of base)
	"claude-opus-4-7":   {InputUSDPer1M: 15.00, OutputUSDPer1M: 75.00, CacheReadMultiplier: 0.10},
	"claude-sonnet-4-5": {InputUSDPer1M: 3.00, OutputUSDPer1M: 15.00, CacheReadMultiplier: 0.10},
	"claude-haiku-4-5":  {InputUSDPer1M: 0.80, OutputUSDPer1M: 4.00, CacheReadMultiplier: 0.10},

	// OpenAI GPT-5.5 (cache reads at 50% of base across the GPT-4.x/5.x line)
	"gpt-5.5":      {InputUSDPer1M: 5.00, OutputUSDPer1M: 40.00, CacheReadMultiplier: 0.50},
	"gpt-5.5-pro":  {InputUSDPer1M: 30.00, OutputUSDPer1M: 120.00, CacheReadMultiplier: 0.50},
	"gpt-5.5-mini": {InputUSDPer1M: 0.50, OutputUSDPer1M: 2.50, CacheReadMultiplier: 0.50},
	"gpt-5.5-nano": {InputUSDPer1M: 0.15, OutputUSDPer1M: 0.60, CacheReadMultiplier: 0.50},

	// OpenAI GPT-5.4
	"gpt-5.4":      {InputUSDPer1M: 3.00, OutputUSDPer1M: 12.00, CacheReadMultiplier: 0.50},
	"gpt-5.4-pro":  {InputUSDPer1M: 20.00, OutputUSDPer1M: 80.00, CacheReadMultiplier: 0.50},
	"gpt-5.4-mini": {InputUSDPer1M: 0.40, OutputUSDPer1M: 1.60, CacheReadMultiplier: 0.50},
	"gpt-5.4-nano": {InputUSDPer1M: 0.10, OutputUSDPer1M: 0.40, CacheReadMultiplier: 0.50},

	// OpenAI GPT-5
	"gpt-5":      {InputUSDPer1M: 2.50, OutputUSDPer1M: 10.00, CacheReadMultiplier: 0.50},
	"gpt-5-chat": {InputUSDPer1M: 2.50, OutputUSDPer1M: 10.00, CacheReadMultiplier: 0.50},
	"gpt-5-mini": {InputUSDPer1M: 0.50, OutputUSDPer1M: 2.00, CacheReadMultiplier: 0.50},
	"gpt-5-nano": {InputUSDPer1M: 0.10, OutputUSDPer1M: 0.40, CacheReadMultiplier: 0.50},

	// OpenAI GPT-4.x (legacy)
	"gpt-4.1":      {InputUSDPer1M: 2.00, OutputUSDPer1M: 8.00, CacheReadMultiplier: 0.50},
	"gpt-4.1-mini": {InputUSDPer1M: 0.40, OutputUSDPer1M: 1.60, CacheReadMultiplier: 0.50},
	"gpt-4.1-nano": {InputUSDPer1M: 0.10, OutputUSDPer1M: 0.40, CacheReadMultiplier: 0.50},
	"gpt-4o":       {InputUSDPer1M: 2.50, OutputUSDPer1M: 10.00, CacheReadMultiplier: 0.50},
	"gpt-4o-mini":  {InputUSDPer1M: 0.15, OutputUSDPer1M: 0.60, CacheReadMultiplier: 0.50},

	// Google Gemini 3.x (implicit caching at 25% of base)
	"gemini-3-pro-preview":          {InputUSDPer1M: 2.00, OutputUSDPer1M: 8.00, CacheReadMultiplier: 0.25},
	"gemini-3.1-pro-preview":        {InputUSDPer1M: 2.00, OutputUSDPer1M: 8.00, CacheReadMultiplier: 0.25},
	"gemini-3-flash-preview":        {InputUSDPer1M: 0.50, OutputUSDPer1M: 2.00, CacheReadMultiplier: 0.25},
	"gemini-3.1-flash-lite-preview": {InputUSDPer1M: 0.10, OutputUSDPer1M: 0.40, CacheReadMultiplier: 0.25},

	// Google Gemini 2.x (legacy)
	"gemini-2.5-pro":        {InputUSDPer1M: 1.25, OutputUSDPer1M: 5.00, CacheReadMultiplier: 0.25},
	"gemini-2.5-flash":      {InputUSDPer1M: 0.30, OutputUSDPer1M: 1.20, CacheReadMultiplier: 0.25},
	"gemini-2.5-flash-lite": {InputUSDPer1M: 0.10, OutputUSDPer1M: 0.40, CacheReadMultiplier: 0.25},
	"gemini-2.0-flash":      {InputUSDPer1M: 0.10, OutputUSDPer1M: 0.40, CacheReadMultiplier: 0.25},
	"gemini-2.0-flash-lite": {InputUSDPer1M: 0.075, OutputUSDPer1M: 0.30, CacheReadMultiplier: 0.25},

	// OpenRouter OSS pool
	"qwen/qwen3-235b-a22b-2507":        {InputUSDPer1M: 0.071, OutputUSDPer1M: 0.463},
	"qwen/qwen3-30b-a3b-instruct-2507": {InputUSDPer1M: 0.080, OutputUSDPer1M: 0.330},
	"qwen/qwen3-coder-next":            {InputUSDPer1M: 0.070, OutputUSDPer1M: 0.300},
	"qwen/qwen3-next-80b-a3b-instruct": {InputUSDPer1M: 0.090, OutputUSDPer1M: 1.100},

	// OpenRouter OSS pool: v0.25 expansion
	"qwen/qwen3.5-flash-02-23":     {InputUSDPer1M: 0.065, OutputUSDPer1M: 0.260},
	"qwen/qwen3-coder":             {InputUSDPer1M: 0.220, OutputUSDPer1M: 1.800},
	"deepseek/deepseek-v4-flash":   {InputUSDPer1M: 0.140, OutputUSDPer1M: 0.280, CacheReadMultiplier: 0.10},
	"deepseek/deepseek-v4-pro":     {InputUSDPer1M: 0.435, OutputUSDPer1M: 0.870, CacheReadMultiplier: 0.10},
	"moonshotai/kimi-k2.5":         {InputUSDPer1M: 0.440, OutputUSDPer1M: 2.000},
	"mistralai/mistral-small-2603": {InputUSDPer1M: 0.150, OutputUSDPer1M: 0.600},
}

// For returns pricing for the given model. If the exact name isn't found,
// retries after stripping an 8-digit date suffix (e.g. "-20251001").
func For(model string) (Pricing, bool) {
	if p, ok := table[model]; ok {
		return p, true
	}
	if normalized := stripDateSuffix(model); normalized != model {
		if p, ok := table[normalized]; ok {
			return p, true
		}
	}
	return Pricing{}, false
}

// stripDateSuffix removes a trailing "-XXXXXXXX" (hyphen + exactly 8 digits).
func stripDateSuffix(model string) string {
	if len(model) < 10 {
		return model
	}
	tail := model[len(model)-9:]
	if tail[0] != '-' {
		return model
	}
	for _, c := range tail[1:] {
		if c < '0' || c > '9' {
			return model
		}
	}
	return model[:len(model)-9]
}
