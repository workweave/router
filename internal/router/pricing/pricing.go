// Package pricing exposes per-model input/output USD pricing for inner-ring
// consumers (e.g. the planner's EV math). Pure data + lookup helpers; no I/O.
// Source of truth for prices the OTel layer also emits as attributes.
package pricing

import "maps"

// Pricing holds the per-1M-token USD costs for a single model.
type Pricing struct {
	InputUSDPer1M  float64
	OutputUSDPer1M float64
}

// Anthropic-published prompt-cache pricing multipliers (relative to base input
// price). The planner uses these to compute the expected cost of a turn given
// a pin's accumulated cache state. Source: docs.anthropic.com/prompt-caching.
const (
	// CacheReadMultiplier is the multiplier applied to base input price for
	// tokens served from a cache hit (cache_read_input_tokens).
	CacheReadMultiplier = 0.1
	// CacheWriteMultiplier5Min is the multiplier applied to base input price
	// for tokens written into the 5-minute cache tier (cache_creation_input_tokens
	// with a 5-minute TTL).
	CacheWriteMultiplier5Min = 1.25
	// CacheWriteMultiplier1Hour is the multiplier applied to base input price
	// for tokens written into the 1-hour cache tier.
	CacheWriteMultiplier1Hour = 2.0
)

// TODO: Unify all model configuration that is indexed by model name
// (this pricing table, model_registry.json, heuristic config, etc.)
// into a single shared model registry so additions/removals stay in sync.

// All returns a copy of the full pricing table keyed by model name.
func All() map[string]Pricing {
	out := make(map[string]Pricing, len(table))
	maps.Copy(out, table)
	return out
}

// Values from provider pricing pages. Output costs are typically 4-5× input.
var table = map[string]Pricing{
	// Anthropic
	"claude-opus-4-7":   {InputUSDPer1M: 15.00, OutputUSDPer1M: 75.00},
	"claude-sonnet-4-5": {InputUSDPer1M: 3.00, OutputUSDPer1M: 15.00},
	"claude-haiku-4-5":  {InputUSDPer1M: 0.80, OutputUSDPer1M: 4.00},

	// OpenAI GPT-5.5
	"gpt-5.5":      {InputUSDPer1M: 5.00, OutputUSDPer1M: 40.00},
	"gpt-5.5-pro":  {InputUSDPer1M: 30.00, OutputUSDPer1M: 120.00},
	"gpt-5.5-mini": {InputUSDPer1M: 0.50, OutputUSDPer1M: 2.50},
	"gpt-5.5-nano": {InputUSDPer1M: 0.15, OutputUSDPer1M: 0.60},

	// OpenAI GPT-5.4
	"gpt-5.4":      {InputUSDPer1M: 3.00, OutputUSDPer1M: 12.00},
	"gpt-5.4-pro":  {InputUSDPer1M: 20.00, OutputUSDPer1M: 80.00},
	"gpt-5.4-mini": {InputUSDPer1M: 0.40, OutputUSDPer1M: 1.60},
	"gpt-5.4-nano": {InputUSDPer1M: 0.10, OutputUSDPer1M: 0.40},

	// OpenAI GPT-5
	"gpt-5":      {InputUSDPer1M: 2.50, OutputUSDPer1M: 10.00},
	"gpt-5-chat": {InputUSDPer1M: 2.50, OutputUSDPer1M: 10.00},
	"gpt-5-mini": {InputUSDPer1M: 0.50, OutputUSDPer1M: 2.00},
	"gpt-5-nano": {InputUSDPer1M: 0.10, OutputUSDPer1M: 0.40},

	// OpenAI GPT-4.x (legacy)
	"gpt-4.1":      {InputUSDPer1M: 2.00, OutputUSDPer1M: 8.00},
	"gpt-4.1-mini": {InputUSDPer1M: 0.40, OutputUSDPer1M: 1.60},
	"gpt-4.1-nano": {InputUSDPer1M: 0.10, OutputUSDPer1M: 0.40},
	"gpt-4o":       {InputUSDPer1M: 2.50, OutputUSDPer1M: 10.00},
	"gpt-4o-mini":  {InputUSDPer1M: 0.15, OutputUSDPer1M: 0.60},

	// Google Gemini 3.x
	"gemini-3-pro-preview":          {InputUSDPer1M: 2.00, OutputUSDPer1M: 8.00},
	"gemini-3.1-pro-preview":        {InputUSDPer1M: 2.00, OutputUSDPer1M: 8.00},
	"gemini-3-flash-preview":        {InputUSDPer1M: 0.50, OutputUSDPer1M: 2.00},
	"gemini-3.1-flash-lite-preview": {InputUSDPer1M: 0.10, OutputUSDPer1M: 0.40},

	// Google Gemini 2.x (legacy)
	"gemini-2.5-pro":        {InputUSDPer1M: 1.25, OutputUSDPer1M: 5.00},
	"gemini-2.5-flash":      {InputUSDPer1M: 0.30, OutputUSDPer1M: 1.20},
	"gemini-2.5-flash-lite": {InputUSDPer1M: 0.10, OutputUSDPer1M: 0.40},
	"gemini-2.0-flash":      {InputUSDPer1M: 0.10, OutputUSDPer1M: 0.40},
	"gemini-2.0-flash-lite": {InputUSDPer1M: 0.075, OutputUSDPer1M: 0.30},

	// OpenRouter OSS pool: Qwen3 R2-Router clone candidates
	"qwen/qwen3-235b-a22b-2507":        {InputUSDPer1M: 0.071, OutputUSDPer1M: 0.463},
	"qwen/qwen3-30b-a3b-instruct-2507": {InputUSDPer1M: 0.080, OutputUSDPer1M: 0.330},
	"qwen/qwen3-coder-next":            {InputUSDPer1M: 0.070, OutputUSDPer1M: 0.300},
	"qwen/qwen3-next-80b-a3b-instruct": {InputUSDPer1M: 0.090, OutputUSDPer1M: 1.100},

	// OpenRouter OSS pool: v0.25 expansion
	"qwen/qwen3.5-flash-02-23":     {InputUSDPer1M: 0.065, OutputUSDPer1M: 0.260},
	"qwen/qwen3-coder":             {InputUSDPer1M: 0.220, OutputUSDPer1M: 1.800},
	"deepseek/deepseek-v4-flash":   {InputUSDPer1M: 0.140, OutputUSDPer1M: 0.280},
	"deepseek/deepseek-v4-pro":     {InputUSDPer1M: 0.435, OutputUSDPer1M: 0.870},
	"moonshotai/kimi-k2.5":         {InputUSDPer1M: 0.440, OutputUSDPer1M: 2.000},
	"mistralai/mistral-small-2603": {InputUSDPer1M: 0.150, OutputUSDPer1M: 0.600},
}

// For returns pricing for the given model and a boolean indicating whether the
// model was found. If the exact name is not present, it retries after stripping
// a trailing 8-digit suffix (e.g. "-20251001") so dated model variants resolve
// to their canonical pricing.
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

// stripDateSuffix removes a trailing "-XXXXXXXX" (hyphen + exactly 8 digits)
// from model names. Returns the input unchanged if the pattern doesn't match.
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
