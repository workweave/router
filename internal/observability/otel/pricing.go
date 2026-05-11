// Package otel provides the OTel span emitter and model pricing.
//
//go:generate go run ../../../cmd/genprices
package otel

// Pricing holds the per-1M-token USD costs for a single model.
type Pricing struct {
	InputUSDPer1M  float64
	OutputUSDPer1M float64
}

// TODO: Unify all model configuration that is indexed by model name
// (this pricing table, model_registry.json, heuristic config, etc.)
// into a single shared model registry so additions/removals stay in sync.

// AllPricing returns a copy of the full pricing table keyed by model name.
func AllPricing() map[string]Pricing {
	out := make(map[string]Pricing, len(pricingTable))
	for k, v := range pricingTable {
		out[k] = v
	}
	return out
}

// Values from provider pricing pages. Output costs are typically 4-5× input.
var pricingTable = map[string]Pricing{
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

	// OpenRouter — DeepSeek (estimates based on OpenRouter pricing; verify against openrouter.ai/models)
	"deepseek/deepseek-v4-flash": {InputUSDPer1M: 0.27, OutputUSDPer1M: 1.10},
	"deepseek/deepseek-v4-pro":   {InputUSDPer1M: 0.55, OutputUSDPer1M: 2.19},

	// OpenRouter — Qwen3 (estimates)
	"qwen/qwen3-235b-a22b-2507":        {InputUSDPer1M: 0.40, OutputUSDPer1M: 1.20},
	"qwen/qwen3-30b-a3b-instruct-2507": {InputUSDPer1M: 0.10, OutputUSDPer1M: 0.30},
	"qwen/qwen3-coder-next":            {InputUSDPer1M: 0.30, OutputUSDPer1M: 1.20},
	"qwen/qwen3-next-80b-a3b-instruct": {InputUSDPer1M: 0.20, OutputUSDPer1M: 0.60},
	"qwen/qwen3.5-flash-02-23":         {InputUSDPer1M: 0.10, OutputUSDPer1M: 0.30},
	"qwen/qwen3-coder":                 {InputUSDPer1M: 0.30, OutputUSDPer1M: 1.20},

	// OpenRouter — Kimi (estimates)
	"moonshotai/kimi-k2.5": {InputUSDPer1M: 0.15, OutputUSDPer1M: 2.00},

	// OpenRouter — Mistral (estimates)
	"mistralai/mistral-small-2603": {InputUSDPer1M: 0.10, OutputUSDPer1M: 0.30},
}

// Lookup returns pricing for the given model. If the exact name is not found,
// it retries after stripping a trailing 8-digit suffix (e.g. "-20251001")
// so dated model variants resolve to their canonical pricing.
// Zero-value for completely unknown models.
func Lookup(model string) Pricing {
	if p, ok := pricingTable[model]; ok {
		return p
	}
	if normalized := stripDateSuffix(model); normalized != model {
		if p, ok := pricingTable[normalized]; ok {
			return p
		}
	}
	return Pricing{}
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
