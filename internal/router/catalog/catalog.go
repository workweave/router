// Package catalog is the single source of truth for per-model data: tier,
// per-provider upstream IDs, per-provider pricing. Adding a model is one
// struct literal here. Every model carries an ordered Providers list; the
// first binding whose Provider is in the deploy's available set is chosen,
// letting SOC-2 direct-provider rows append an OpenRouter fallback. Pure
// inner-ring, no I/O.
package catalog

import (
	"workweave/router/internal/providers"
)

// Tier is the coarse capability bucket. Higher is stronger; integer
// ordering is load-bearing (planner compares freshTier > pinTier).
type Tier int

const (
	TierUnknown Tier = iota // Zero value; absent from table.
	TierLow
	TierMid
	TierHigh
)

// String returns a snake_case label for logs and OTel attrs.
func (t Tier) String() string {
	switch t {
	case TierLow:
		return "low"
	case TierMid:
		return "mid"
	case TierHigh:
		return "high"
	default:
		return "unknown"
	}
}

// Pricing holds the per-1M-token USD costs for a single (provider, model)
// binding.
type Pricing struct {
	InputUSDPer1M  float64
	OutputUSDPer1M float64
	// CacheReadMultiplier is the cost of a cache hit relative to the base
	// input price (e.g. 0.10 for Anthropic, 0.50 for OpenAI). Zero means
	// "unspecified — use DefaultCacheReadMultiplier".
	CacheReadMultiplier float64
}

// DefaultCacheReadMultiplier is the fallback multiplier for bindings
// without published cache pricing. 0.5 is conservative: high enough to not
// treat unknown providers as free caching, low enough to not block switches.
const DefaultCacheReadMultiplier = 0.5

// EffectiveCacheReadMultiplier returns CacheReadMultiplier if set, else
// DefaultCacheReadMultiplier.
func (p Pricing) EffectiveCacheReadMultiplier() float64 {
	if p.CacheReadMultiplier > 0 {
		return p.CacheReadMultiplier
	}
	return DefaultCacheReadMultiplier
}

// ProviderBinding is one (provider, upstream-model-ID, price) tuple for a
// logical model. Ordered list per Model — the scorer picks the first whose
// Provider name is wired in the running deploy.
type ProviderBinding struct {
	// Provider is one of the providers.Provider* constants.
	Provider string
	// UpstreamID is the model ID the upstream API expects. Empty means
	// "same as Model.ID" (no rewrite). Non-empty is fed to the
	// openaicompat client's modelIDMap so the body's "model" field is
	// rewritten at proxy time (e.g. Bedrock's dot-form, DeepInfra's
	// HuggingFace form).
	UpstreamID string
	// Price is the per-provider pricing for this binding.
	Price Pricing
}

// ToolUseQuality marks a model's reliability under has_tools=true turns.
// ToolUseUnknown (zero value) = no concerns recorded; ToolUseLow flags models
// that hallucinate tool calls, emit malformed tool_use blocks, or loop on the
// same tool. The scorer excludes ToolUseLow models from argmax on tool-bearing
// requests, falling back to the unfiltered pool only if that would empty it.
type ToolUseQuality int

const (
	ToolUseUnknown ToolUseQuality = iota
	ToolUseLow
)

// AgenticUse marks whether a model can reliably drive an agentic harness (the
// multi-step skill/tool-orchestration loop in Claude Code, opencode, etc).
// Stricter than ToolUseQuality: a model can emit well-formed tool calls yet
// still fail to run the harness (e.g. minimax-m3 grepped the filesystem for a
// skill instead of invoking it). AgenticLow flags models the scorer drops
// from has_tools turns, so the price dial can demote Opus to a cheaper
// harness-capable model instead of stranding the turn on the cheapest one.
type AgenticUse int

const (
	AgenticUnknown AgenticUse = iota
	AgenticLow
)

// ImageInput marks whether a model accepts image content parts.
// ImageInputUnknown (zero value) = no restriction; first-party models default
// here since they're all multimodal. ImageInputUnsupported flags text-only
// models that 4xx on image parts (e.g. DeepInfra's GLM-5.1). The scorer
// excludes the ImageInputUnsupported set from image-bearing requests.
type ImageInput int

const (
	ImageInputUnknown ImageInput = iota
	ImageInputUnsupported
)

// Model is one logical model — the unit the router decides on.
type Model struct {
	// ID is the public slash-form (or bare) model ID exposed to clients,
	// e.g. "claude-opus-4-7" or "deepseek/deepseek-v4-pro".
	ID string
	// Tier is the coarse capability bucket. TierUnknown means the model
	// is not deployable as a routing target (passthrough only).
	Tier Tier
	// ContextWindow is the model's total input+output token budget in tokens.
	// 0 means use catalog.DefaultContextWindow.
	ContextWindow int
	// ToolUseQuality: default ToolUseUnknown; set ToolUseLow to remove the
	// model from agentic argmax pools.
	ToolUseQuality ToolUseQuality
	// AgenticUse: default AgenticUnknown; set AgenticLow to keep the model
	// out of the price dial's agentic demotion ladder.
	AgenticUse AgenticUse
	// ImageInput: default ImageInputUnknown; set ImageInputUnsupported on
	// text-only models so the scorer keeps image-bearing turns off them.
	ImageInput ImageInput
	// ThinkTagReasoning marks a model that streams inline <think>...</think>
	// instead of reasoning_content; the Anthropic translator reroutes a
	// leading <think> block into Anthropic thinking. Default false.
	ThinkTagReasoning bool
	// Providers is the ordered fallback list. First binding whose
	// Provider name is in the available set wins. Must be non-empty.
	Providers []ProviderBinding
}

// PrimaryProvider returns the first binding's provider name. Callers that
// don't yet thread provider through (OTel emitter, billing debit hook)
// look up pricing by this.
func (m Model) PrimaryProvider() string {
	if len(m.Providers) == 0 {
		return ""
	}
	return m.Providers[0].Provider
}

// Models is the source of truth, one struct literal per model, grouped by
// family and tier. To add a model: append a Model{} below, and if the deploy
// should route to it, list it in the cluster bundle's model_registry.json
// (that file controls which versions route to it; this catalog controls
// pricing/dispatch). No other files need to change.
var Models = []Model{
	// --- Anthropic ---
	{ID: "claude-haiku-4-5", Tier: TierLow, ContextWindow: 200_000, Providers: []ProviderBinding{
		{Provider: providers.ProviderAnthropic, Price: Pricing{InputUSDPer1M: 0.80, OutputUSDPer1M: 4.00, CacheReadMultiplier: 0.10}},
	}},
	{ID: "claude-sonnet-4-5", Tier: TierMid, ContextWindow: 200_000, Providers: []ProviderBinding{
		{Provider: providers.ProviderAnthropic, Price: Pricing{InputUSDPer1M: 3.00, OutputUSDPer1M: 15.00, CacheReadMultiplier: 0.10}},
	}},
	{ID: "claude-sonnet-4-6", Tier: TierMid, ContextWindow: 200_000, Providers: []ProviderBinding{
		{Provider: providers.ProviderAnthropic, Price: Pricing{InputUSDPer1M: 3.00, OutputUSDPer1M: 15.00, CacheReadMultiplier: 0.10}},
	}},
	// 1M context is behind the context-1m beta (catalog carries 200K like the
	// rest of Sonnet). Priced at standard $3/$15, not the $2/$10 introductory
	// rate (through 2026-08-31) — avoids a compile-time price going stale.
	{ID: "claude-sonnet-5", Tier: TierMid, ContextWindow: 200_000, Providers: []ProviderBinding{
		{Provider: providers.ProviderAnthropic, Price: Pricing{InputUSDPer1M: 3.00, OutputUSDPer1M: 15.00, CacheReadMultiplier: 0.10}},
	}},
	// Legacy Opus IDs kept passthrough-priced (no Tier — not a routing
	// target; see gpt-4o below for the same pattern) so BYOK/direct-model
	// requests billing-debit at real cost instead of catalog.PrimaryPriceFor
	// silently returning $0. Prices per the opus-4-6 comment below: 4.1 and
	// earlier were $15/$75; 4.5 is the first $5/$25 release.
	{ID: "claude-opus-4-0", ContextWindow: 200_000, Providers: []ProviderBinding{
		{Provider: providers.ProviderAnthropic, Price: Pricing{InputUSDPer1M: 15.00, OutputUSDPer1M: 75.00, CacheReadMultiplier: 0.10}},
	}},
	{ID: "claude-opus-4-1", ContextWindow: 200_000, Providers: []ProviderBinding{
		{Provider: providers.ProviderAnthropic, Price: Pricing{InputUSDPer1M: 15.00, OutputUSDPer1M: 75.00, CacheReadMultiplier: 0.10}},
	}},
	{ID: "claude-opus-4-5", ContextWindow: 200_000, Providers: []ProviderBinding{
		{Provider: providers.ProviderAnthropic, Price: Pricing{InputUSDPer1M: 5.00, OutputUSDPer1M: 25.00, CacheReadMultiplier: 0.10}},
	}},
	// Opus 4.5+ is $5/$25 per MTok (down from $15/$75 on 4.1 and earlier).
	// 4.6+/4.7+/4.8 support 1M context via the context-1m-2025-08-07 beta; the
	// catalog reports 200K and the pre-filter expands to 1M when the beta
	// header is present (contextWindowForRequest in proxy/service.go).
	{ID: "claude-opus-4-6", Tier: TierHigh, ContextWindow: 200_000, Providers: []ProviderBinding{
		{Provider: providers.ProviderAnthropic, Price: Pricing{InputUSDPer1M: 5.00, OutputUSDPer1M: 25.00, CacheReadMultiplier: 0.10}},
	}},
	{ID: "claude-opus-4-7", Tier: TierHigh, ContextWindow: 200_000, Providers: []ProviderBinding{
		{Provider: providers.ProviderAnthropic, Price: Pricing{InputUSDPer1M: 5.00, OutputUSDPer1M: 25.00, CacheReadMultiplier: 0.10}},
	}},
	{ID: "claude-opus-4-8", Tier: TierHigh, ContextWindow: 200_000, Providers: []ProviderBinding{
		{Provider: providers.ProviderAnthropic, Price: Pricing{InputUSDPer1M: 5.00, OutputUSDPer1M: 25.00, CacheReadMultiplier: 0.10}},
	}},
	// Ships 1M context by default (no beta header needed), unlike Opus 4.6+.
	// Safety classifiers can return stop_reason "refusal" (HTTP 200); see
	// mapStopReason in translate.
	{ID: "claude-fable-5", Tier: TierHigh, ContextWindow: 1_000_000, Providers: []ProviderBinding{
		{Provider: providers.ProviderAnthropic, Price: Pricing{InputUSDPer1M: 10.00, OutputUSDPer1M: 50.00, CacheReadMultiplier: 0.10}},
	}},

	// --- OpenAI GPT-4.x (legacy) ---
	{ID: "gpt-4.1-nano", Tier: TierLow, ContextWindow: 1_047_576, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 0.10, OutputUSDPer1M: 0.40, CacheReadMultiplier: 0.25}},
	}},
	{ID: "gpt-4.1-mini", Tier: TierLow, ContextWindow: 1_047_576, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 0.40, OutputUSDPer1M: 1.60, CacheReadMultiplier: 0.25}},
	}},
	{ID: "gpt-4.1", Tier: TierMid, ContextWindow: 1_047_576, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 2.00, OutputUSDPer1M: 8.00, CacheReadMultiplier: 0.25}},
	}},
	// gpt-4o family: priced for passthrough, not a routing target.
	{ID: "gpt-4o-mini", ContextWindow: 128_000, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 0.15, OutputUSDPer1M: 0.60, CacheReadMultiplier: 0.50}},
	}},
	{ID: "gpt-4o", ContextWindow: 128_000, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 2.50, OutputUSDPer1M: 10.00, CacheReadMultiplier: 0.50}},
	}},

	// --- OpenAI GPT-5 ---
	{ID: "gpt-5-nano", ContextWindow: 400_000, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 0.10, OutputUSDPer1M: 0.40, CacheReadMultiplier: 0.10}},
	}},
	{ID: "gpt-5-mini", ContextWindow: 400_000, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 0.50, OutputUSDPer1M: 2.00, CacheReadMultiplier: 0.10}},
	}},
	{ID: "gpt-5", Tier: TierHigh, ContextWindow: 400_000, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 2.50, OutputUSDPer1M: 10.00, CacheReadMultiplier: 0.10}},
	}},
	{ID: "gpt-5-chat", ContextWindow: 400_000, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 2.50, OutputUSDPer1M: 10.00, CacheReadMultiplier: 0.10}},
	}},

	// --- OpenAI GPT-5.4 ---
	{ID: "gpt-5.4-nano", Tier: TierMid, ContextWindow: 1_000_000, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 0.10, OutputUSDPer1M: 0.40, CacheReadMultiplier: 0.10}},
	}},
	{ID: "gpt-5.4-mini", Tier: TierMid, ContextWindow: 400_000, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 0.40, OutputUSDPer1M: 1.60, CacheReadMultiplier: 0.10}},
	}},
	{ID: "gpt-5.4", Tier: TierHigh, ContextWindow: 1_000_000, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 3.00, OutputUSDPer1M: 12.00, CacheReadMultiplier: 0.10}},
	}},
	{ID: "gpt-5.4-pro", Tier: TierHigh, ContextWindow: 1_000_000, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 20.00, OutputUSDPer1M: 80.00, CacheReadMultiplier: 1.0}},
	}},

	// --- OpenAI GPT-5.5 ---
	{ID: "gpt-5.5-nano", Tier: TierMid, ContextWindow: 1_000_000, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 0.15, OutputUSDPer1M: 0.60, CacheReadMultiplier: 0.10}},
	}},
	{ID: "gpt-5.5-mini", Tier: TierMid, ContextWindow: 1_000_000, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 0.50, OutputUSDPer1M: 2.50, CacheReadMultiplier: 0.10}},
	}},
	{ID: "gpt-5.5", Tier: TierHigh, ContextWindow: 1_050_000, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 5.00, OutputUSDPer1M: 40.00, CacheReadMultiplier: 0.10}},
	}},
	{ID: "gpt-5.5-pro", Tier: TierHigh, ContextWindow: 1_000_000, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 30.00, OutputUSDPer1M: 120.00, CacheReadMultiplier: 1.0}},
	}},

	// --- Google Gemini 2.x ---
	{ID: "gemini-2.0-flash-lite", ContextWindow: 1_048_576, Providers: []ProviderBinding{
		{Provider: providers.ProviderGoogle, Price: Pricing{InputUSDPer1M: 0.075, OutputUSDPer1M: 0.30, CacheReadMultiplier: 0.25}},
	}},
	{ID: "gemini-2.0-flash", ContextWindow: 1_048_576, Providers: []ProviderBinding{
		{Provider: providers.ProviderGoogle, Price: Pricing{InputUSDPer1M: 0.10, OutputUSDPer1M: 0.40, CacheReadMultiplier: 0.25}},
	}},
	{ID: "gemini-2.5-flash-lite", ContextWindow: 1_048_576, Providers: []ProviderBinding{
		{Provider: providers.ProviderGoogle, Price: Pricing{InputUSDPer1M: 0.10, OutputUSDPer1M: 0.40, CacheReadMultiplier: 0.10}},
	}},
	{ID: "gemini-2.5-flash", Tier: TierLow, ContextWindow: 1_048_576, Providers: []ProviderBinding{
		{Provider: providers.ProviderGoogle, Price: Pricing{InputUSDPer1M: 0.30, OutputUSDPer1M: 1.20, CacheReadMultiplier: 0.10}},
	}},
	{ID: "gemini-2.5-pro", ContextWindow: 1_048_576, Providers: []ProviderBinding{
		{Provider: providers.ProviderGoogle, Price: Pricing{InputUSDPer1M: 1.25, OutputUSDPer1M: 5.00, CacheReadMultiplier: 0.10}},
	}},

	// --- Google Gemini 3.x ---
	{ID: "gemini-3.1-flash-lite-preview", Tier: TierLow, ContextWindow: 1_048_576, AgenticUse: AgenticLow, Providers: []ProviderBinding{
		{Provider: providers.ProviderGoogle, Price: Pricing{InputUSDPer1M: 0.10, OutputUSDPer1M: 0.40, CacheReadMultiplier: 0.10}},
	}},
	{ID: "gemini-3-flash-preview", Tier: TierMid, ContextWindow: 1_048_576, Providers: []ProviderBinding{
		{Provider: providers.ProviderGoogle, Price: Pricing{InputUSDPer1M: 0.50, OutputUSDPer1M: 2.00, CacheReadMultiplier: 0.10}},
	}},
	{ID: "gemini-3-pro-preview", Tier: TierHigh, ContextWindow: 1_048_576, Providers: []ProviderBinding{
		{Provider: providers.ProviderGoogle, Price: Pricing{InputUSDPer1M: 2.00, OutputUSDPer1M: 8.00, CacheReadMultiplier: 0.10}},
	}},
	{ID: "gemini-3.1-pro-preview", Tier: TierHigh, ContextWindow: 1_048_576, Providers: []ProviderBinding{
		{Provider: providers.ProviderGoogle, Price: Pricing{InputUSDPer1M: 2.00, OutputUSDPer1M: 8.00, CacheReadMultiplier: 0.10}},
	}},
	{ID: "gemini-3.5-flash", Tier: TierMid, ContextWindow: 1_048_576, Providers: []ProviderBinding{
		{Provider: providers.ProviderGoogle, Price: Pricing{InputUSDPer1M: 1.50, OutputUSDPer1M: 9.00, CacheReadMultiplier: 0.10}},
	}},
	{ID: "google/gemma-4-26b-a4b-it", Tier: TierLow, ContextWindow: 262_144, Providers: []ProviderBinding{
		{Provider: providers.ProviderGoogle, UpstreamID: "gemma-4-26b-a4b-it", Price: Pricing{InputUSDPer1M: 0.15, OutputUSDPer1M: 0.60, CacheReadMultiplier: 0.10}},
	}},

	// --- OSS pool ---
	//
	// Each row carries an ordered Providers list. Managed-prod ships only the
	// SOC-2 primary key (Fireworks/DeepInfra/Bedrock/OpenAI/Anthropic/Google)
	// and drops the trailing OpenRouter binding; self-hosters with only an
	// OpenRouter key route every OSS model via that fallback.
	//
	// qwen/qwen3-235b-a22b-2507: Bedrock primary (verified 2026-05-22) after
	// OpenRouter showed non-SSE responses routing Qwen through Google hosting
	// (silent CC stalls). ToolUseLow: the non-thinking Instruct-2507 variant
	// underperforms Thinking on tool use (Qwen model card, arxiv 2604.02155)
	// and was observed returning narrative text with zero tool_use blocks in
	// prod traffic (2026-05-23) — excluded from agentic pools until Thinking
	// lands.
	{ID: "qwen/qwen3-235b-a22b-2507", Tier: TierMid, ContextWindow: 262_144, ToolUseQuality: ToolUseLow, ImageInput: ImageInputUnsupported, Providers: []ProviderBinding{
		{Provider: providers.ProviderBedrock, UpstreamID: "qwen.qwen3-235b-a22b-2507",
			Price: Pricing{InputUSDPer1M: 0.2266, OutputUSDPer1M: 0.9064}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.071, OutputUSDPer1M: 0.463}},
	}},
	{ID: "qwen/qwen3-coder-next", Tier: TierMid, ContextWindow: 262_144, ImageInput: ImageInputUnsupported, Providers: []ProviderBinding{
		{Provider: providers.ProviderBedrock, UpstreamID: "qwen.qwen3-coder-next",
			Price: Pricing{InputUSDPer1M: 0.500, OutputUSDPer1M: 1.200}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.070, OutputUSDPer1M: 0.300}},
	}},
	{ID: "qwen/qwen3-next-80b-a3b-instruct", Tier: TierMid, ContextWindow: 262_144, ImageInput: ImageInputUnsupported, AgenticUse: AgenticLow, Providers: []ProviderBinding{
		{Provider: providers.ProviderBedrock, UpstreamID: "qwen.qwen3-next-80b-a3b-instruct",
			Price: Pricing{InputUSDPer1M: 0.150, OutputUSDPer1M: 1.200}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.090, OutputUSDPer1M: 1.100}},
	}},
	// DeepSeek V4 natively serves 1,048,576 tokens; the 131_072 carried over
	// from V3.2 was filtering requests over ~128K (excludeContextOverflowModels
	// in proxy/service.go).
	{ID: "deepseek/deepseek-v4-flash", Tier: TierLow, ContextWindow: 1_048_576, ImageInput: ImageInputUnsupported, AgenticUse: AgenticLow, Providers: []ProviderBinding{
		{Provider: providers.ProviderMakora, UpstreamID: "deepseek-ai/DeepSeek-V4-Flash",
			Price: Pricing{InputUSDPer1M: 0.1134, OutputUSDPer1M: 0.2791, CacheReadMultiplier: 0.20}},
		{Provider: providers.ProviderDeepInfra, UpstreamID: "deepseek-ai/DeepSeek-V4-Flash",
			Price: Pricing{InputUSDPer1M: 0.140, OutputUSDPer1M: 0.280, CacheReadMultiplier: 0.20}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.140, OutputUSDPer1M: 0.280, CacheReadMultiplier: 0.10}},
	}},
	{ID: "deepseek/deepseek-v4-pro", Tier: TierHigh, ContextWindow: 1_048_576, ImageInput: ImageInputUnsupported, Providers: []ProviderBinding{
		// Makora primary for cost ($1.318/$2.636 vs Together's $1.74/$3.48).
		// Together ranks ahead of Fireworks as fallback: #1 AA throughput
		// (~209 t/s vs Fireworks ~120) at the same price.
		{Provider: providers.ProviderMakora, UpstreamID: "deepseek-ai/DeepSeek-V4-Pro",
			Price: Pricing{InputUSDPer1M: 1.3180, OutputUSDPer1M: 2.6361, CacheReadMultiplier: 0.10}},
		{Provider: providers.ProviderTogether, UpstreamID: "deepseek-ai/DeepSeek-V4-Pro",
			Price: Pricing{InputUSDPer1M: 1.740, OutputUSDPer1M: 3.480, CacheReadMultiplier: 0.20 / 1.740}},
		{Provider: providers.ProviderFireworks, UpstreamID: "accounts/fireworks/models/deepseek-v4-pro",
			Price: Pricing{InputUSDPer1M: 1.740, OutputUSDPer1M: 3.480, CacheReadMultiplier: 0.0862}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.435, OutputUSDPer1M: 0.870, CacheReadMultiplier: 0.10}},
	}},
	{ID: "moonshotai/kimi-k2.5", Tier: TierHigh, ContextWindow: 262_144, ImageInput: ImageInputUnsupported, Providers: []ProviderBinding{
		{Provider: providers.ProviderBedrock, UpstreamID: "moonshotai.kimi-k2.5",
			Price: Pricing{InputUSDPer1M: 0.600, OutputUSDPer1M: 3.000}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.440, OutputUSDPer1M: 2.000}},
	}},
	{ID: "moonshotai/kimi-k2.6", Tier: TierHigh, ContextWindow: 262_144, ImageInput: ImageInputUnsupported, Providers: []ProviderBinding{
		{Provider: providers.ProviderFireworks, UpstreamID: "accounts/fireworks/models/kimi-k2p6",
			Price: Pricing{InputUSDPer1M: 0.950, OutputUSDPer1M: 4.000, CacheReadMultiplier: 0.1684}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.950, OutputUSDPer1M: 4.000, CacheReadMultiplier: 0.10}},
	}},
	// kimi-k2.7 "Code" variant: same rates as k2.6, ~30% less thinking-token
	// usage. Fireworks-only — not yet on OpenRouter, so no fallback binding.
	{ID: "moonshotai/kimi-k2.7", Tier: TierHigh, ContextWindow: 262_144, ImageInput: ImageInputUnsupported, Providers: []ProviderBinding{
		{Provider: providers.ProviderFireworks, UpstreamID: "accounts/fireworks/models/kimi-k2p7-code",
			Price: Pricing{InputUSDPer1M: 0.950, OutputUSDPer1M: 4.000, CacheReadMultiplier: 0.20}},
	}},
	// AA top-performer additions (2026-05-18): ranked by composite of quality
	// (Intelligence Index v4.0), cost (blended 3:1), and effective time per
	// 2k-token query.
	//
	// xiaomi/mimo-v2.5 (base) was removed 2026-05-23 after sustained
	// tool-calling failures in real Claude Code sessions (malformed empty-input
	// tool_use, hallucinated tool names, repeat-args loops — matches OpenCode
	// #24095 and Crush #1699). The pro variant doesn't show the instability.
	{ID: "xiaomi/mimo-v2.5-pro", Tier: TierHigh, ContextWindow: 1_048_576, ImageInput: ImageInputUnsupported, ThinkTagReasoning: true, Providers: []ProviderBinding{
		{Provider: providers.ProviderDeepInfra, UpstreamID: "XiaomiMiMo/MiMo-V2.5-Pro",
			Price: Pricing{InputUSDPer1M: 1.000, OutputUSDPer1M: 3.000}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 1.000, OutputUSDPer1M: 3.000, CacheReadMultiplier: 0.10}},
	}},
	// TierLow despite MoE size: active-parameter budget + AA Coding Index put
	// it below v4-flash.
	{ID: "qwen/qwen3.6-35b-a3b", Tier: TierLow, ContextWindow: 262_144, ImageInput: ImageInputUnsupported, Providers: []ProviderBinding{
		{Provider: providers.ProviderMakora, UpstreamID: "unsloth/Qwen3.6-35B-A3B-NVFP4",
			Price: Pricing{InputUSDPer1M: 0.1720, OutputUSDPer1M: 1.2002, CacheReadMultiplier: 0.75}},
		{Provider: providers.ProviderDeepInfra, UpstreamID: "Qwen/Qwen3.6-35B-A3B",
			Price: Pricing{InputUSDPer1M: 0.150, OutputUSDPer1M: 0.950}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.150, OutputUSDPer1M: 1.000, CacheReadMultiplier: 0.10}},
	}},
	// Context window is 204,800 on Fireworks and OpenRouter despite MiniMax's
	// "1M" marketing — do NOT raise without re-confirming the served cap, or
	// requests over ~205K will hard-400 with no failover.
	{ID: "minimax/minimax-m2.7", Tier: TierHigh, ContextWindow: 204_800, ImageInput: ImageInputUnsupported, Providers: []ProviderBinding{
		// Together dominates on M2.7: AA clocks ~399 t/s vs Fireworks' ~95 t/s
		// at the identical $0.30/$1.20 list price.
		{Provider: providers.ProviderTogether, UpstreamID: "MiniMaxAI/MiniMax-M2.7",
			Price: Pricing{InputUSDPer1M: 0.300, OutputUSDPer1M: 1.200, CacheReadMultiplier: 0.06 / 0.300}},
		{Provider: providers.ProviderFireworks, UpstreamID: "accounts/fireworks/models/minimax-m2p7",
			Price: Pricing{InputUSDPer1M: 0.300, OutputUSDPer1M: 1.200}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.279, OutputUSDPer1M: 1.200, CacheReadMultiplier: 0.10}},
	}},
	// Fireworks serves 512k context — the model's headline 1M is not what the
	// endpoint exposes. Unlike m2.7 it accepts images, so ImageInput stays default.
	{ID: "minimax/minimax-m3", Tier: TierHigh, ContextWindow: 512_000, AgenticUse: AgenticLow, Providers: []ProviderBinding{
		{Provider: providers.ProviderFireworks, UpstreamID: "accounts/fireworks/models/minimax-m3",
			Price: Pricing{InputUSDPer1M: 0.300, OutputUSDPer1M: 1.200, CacheReadMultiplier: 0.20}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.300, OutputUSDPer1M: 1.200, CacheReadMultiplier: 0.10}},
	}},
	// Fireworks primary: DeepInfra's FP8 GLM serving is an order of magnitude
	// slower (~33 t/s, ~90s TTFT vs Fireworks' ~180 t/s), the dominant timeout source.
	{ID: "z-ai/glm-5", Tier: TierHigh, ContextWindow: 202_752, ImageInput: ImageInputUnsupported, Providers: []ProviderBinding{
		{Provider: providers.ProviderFireworks, UpstreamID: "accounts/fireworks/models/glm-5",
			Price: Pricing{InputUSDPer1M: 1.000, OutputUSDPer1M: 3.200, CacheReadMultiplier: 0.20}},
		{Provider: providers.ProviderDeepInfra, UpstreamID: "zai-org/GLM-5",
			Price: Pricing{InputUSDPer1M: 0.600, OutputUSDPer1M: 2.080}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.600, OutputUSDPer1M: 1.920, CacheReadMultiplier: 0.10}},
	}},
	// GLM-5.1 ships the streaming tool-call fix GLM-5 lacks (tool_stream=true);
	// emit_openai injects tool_stream + disables thinking for this slug.
	{ID: "z-ai/glm-5.1", Tier: TierHigh, ContextWindow: 202_752, ImageInput: ImageInputUnsupported, Providers: []ProviderBinding{
		// Together edges out Fireworks: AA ranks it #1 in throughput (~213 t/s
		// vs ~180) and TTFT, at the same $1.40/$4.40 list price.
		{Provider: providers.ProviderTogether, UpstreamID: "zai-org/GLM-5.1",
			Price: Pricing{InputUSDPer1M: 1.400, OutputUSDPer1M: 4.400, CacheReadMultiplier: 0.26 / 1.400}},
		{Provider: providers.ProviderFireworks, UpstreamID: "accounts/fireworks/models/glm-5p1",
			Price: Pricing{InputUSDPer1M: 1.400, OutputUSDPer1M: 4.400, CacheReadMultiplier: 0.26 / 1.40}},
		{Provider: providers.ProviderDeepInfra, UpstreamID: "zai-org/GLM-5.1",
			Price: Pricing{InputUSDPer1M: 1.050, OutputUSDPer1M: 3.500}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.980, OutputUSDPer1M: 3.080, CacheReadMultiplier: 0.18 / 0.98}},
	}},
	// ContextWindow held at glm-family 202_752 pending confirmation of the
	// Fireworks served window — overstating triggers hard 400s (cf. the
	// minimax 1M->204800 incident).
	{ID: "z-ai/glm-5.2", Tier: TierHigh, ContextWindow: 202_752, ImageInput: ImageInputUnsupported, Providers: []ProviderBinding{
		// Together leads: #1 AA throughput (~382 t/s vs Fireworks ~347) at the
		// same $1.40/$4.40 price.
		{Provider: providers.ProviderTogether, UpstreamID: "zai-org/GLM-5.2",
			Price: Pricing{InputUSDPer1M: 1.400, OutputUSDPer1M: 4.400, CacheReadMultiplier: 0.26 / 1.400}},
		{Provider: providers.ProviderFireworks, UpstreamID: "accounts/fireworks/models/glm-5p2",
			Price: Pricing{InputUSDPer1M: 1.400, OutputUSDPer1M: 4.400, CacheReadMultiplier: 0.20}},
	}},
	// Fireworks-dedicated rows below carry an OpenRouter trailing binding so
	// managed-prod deploys without a Fireworks key can still resolve them.
	{ID: "mistralai/mistral-small-2603", Tier: TierMid, ContextWindow: 262_144, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.200, OutputUSDPer1M: 0.600, CacheReadMultiplier: 0.10}},
	}},
	{ID: "qwen/qwen3-30b-a3b-instruct-2507", Tier: TierMid, ContextWindow: 262_144, ImageInput: ImageInputUnsupported, Providers: []ProviderBinding{
		{Provider: providers.ProviderFireworks, UpstreamID: "accounts/fireworks/models/qwen3-30b-a3b-instruct-2507",
			Price: Pricing{InputUSDPer1M: 0.150, OutputUSDPer1M: 0.600, CacheReadMultiplier: 0.1684}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.100, OutputUSDPer1M: 0.300, CacheReadMultiplier: 0.10}},
	}},
	{ID: "qwen/qwen3-coder", Tier: TierHigh, ContextWindow: 262_144, ImageInput: ImageInputUnsupported, Providers: []ProviderBinding{
		{Provider: providers.ProviderFireworks, UpstreamID: "accounts/fireworks/models/qwen3-coder-480b-a35b-instruct",
			Price: Pricing{InputUSDPer1M: 0.900, OutputUSDPer1M: 2.700, CacheReadMultiplier: 0.1684}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 1.000, OutputUSDPer1M: 5.000, CacheReadMultiplier: 0.10}},
	}},
	{ID: "qwen/qwen3.5-flash-02-23", Tier: TierLow, ContextWindow: 1_000_000, ImageInput: ImageInputUnsupported, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.050, OutputUSDPer1M: 0.150, CacheReadMultiplier: 0.10}},
	}},
	// Fireworks-only: the closed Alibaba API surface is deliberately avoided
	// (Fireworks is SOC-2, keeps prompts off Alibaba); OpenRouter's route for
	// this model forwards to Alibaba, so it's skipped.
	{ID: "qwen/qwen3.7-plus", Tier: TierHigh, ContextWindow: 262_144, Providers: []ProviderBinding{
		{Provider: providers.ProviderFireworks, UpstreamID: "accounts/fireworks/models/qwen3p7-plus",
			Price: Pricing{InputUSDPer1M: 0.400, OutputUSDPer1M: 1.600, CacheReadMultiplier: 0.20}},
	}},
}
