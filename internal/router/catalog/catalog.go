// Package catalog is the single source of truth for per-model data used
// by the router: capability tier, per-provider upstream IDs, per-provider
// pricing. Adding a new model is one struct literal here; pricing,
// capability, install-script generation, and the cluster scorer all read
// through this package.
//
// Multi-provider: every model carries an ordered Providers list. The
// first binding whose Provider name is in the deploy's available set is
// chosen. Today every entry is a single-element list; the schema is in
// place so SOC-2 direct-provider rows can append an OpenRouter fallback
// without touching call sites.
//
// Pure inner-ring. No I/O.
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

// Model is one logical model — the unit the router decides on.
type Model struct {
	// ID is the public slash-form (or bare) model ID exposed to clients,
	// e.g. "claude-opus-4-7" or "deepseek/deepseek-v4-pro".
	ID string
	// Tier is the coarse capability bucket. TierUnknown means the model
	// is not deployable as a routing target (passthrough only).
	Tier Tier
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

// Models is the source of truth. One struct literal per model, grouped by
// family and tier. To add a model:
//
//  1. Append a Model{} entry below.
//  2. If the deploy needs to route to it, list it in the cluster artifact
//     bundle's model_registry.json (the cluster scorer's per-version
//     candidate list — model_registry.json controls which versions can
//     route to the model, this catalog controls how it's priced and
//     dispatched).
//
// No other files need to change.
var Models = []Model{
	// --- Anthropic ---
	{ID: "claude-haiku-4-5", Tier: TierLow, Providers: []ProviderBinding{
		{Provider: providers.ProviderAnthropic, Price: Pricing{InputUSDPer1M: 0.80, OutputUSDPer1M: 4.00, CacheReadMultiplier: 0.10}},
	}},
	{ID: "claude-sonnet-4-5", Tier: TierMid, Providers: []ProviderBinding{
		{Provider: providers.ProviderAnthropic, Price: Pricing{InputUSDPer1M: 3.00, OutputUSDPer1M: 15.00, CacheReadMultiplier: 0.10}},
	}},
	{ID: "claude-sonnet-4-6", Tier: TierMid, Providers: []ProviderBinding{
		{Provider: providers.ProviderAnthropic, Price: Pricing{InputUSDPer1M: 3.00, OutputUSDPer1M: 15.00, CacheReadMultiplier: 0.10}},
	}},
	{ID: "claude-opus-4-6", Tier: TierHigh, Providers: []ProviderBinding{
		{Provider: providers.ProviderAnthropic, Price: Pricing{InputUSDPer1M: 15.00, OutputUSDPer1M: 75.00, CacheReadMultiplier: 0.10}},
	}},
	{ID: "claude-opus-4-7", Tier: TierHigh, Providers: []ProviderBinding{
		{Provider: providers.ProviderAnthropic, Price: Pricing{InputUSDPer1M: 15.00, OutputUSDPer1M: 75.00, CacheReadMultiplier: 0.10}},
	}},

	// --- OpenAI GPT-4.x (legacy) ---
	{ID: "gpt-4.1-nano", Tier: TierLow, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 0.10, OutputUSDPer1M: 0.40, CacheReadMultiplier: 0.50}},
	}},
	{ID: "gpt-4.1-mini", Tier: TierLow, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 0.40, OutputUSDPer1M: 1.60, CacheReadMultiplier: 0.50}},
	}},
	{ID: "gpt-4.1", Tier: TierMid, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 2.00, OutputUSDPer1M: 8.00, CacheReadMultiplier: 0.50}},
	}},
	// gpt-4o family: priced for passthrough, not a routing target.
	{ID: "gpt-4o-mini", Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 0.15, OutputUSDPer1M: 0.60, CacheReadMultiplier: 0.50}},
	}},
	{ID: "gpt-4o", Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 2.50, OutputUSDPer1M: 10.00, CacheReadMultiplier: 0.50}},
	}},

	// --- OpenAI GPT-5 ---
	{ID: "gpt-5-nano", Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 0.10, OutputUSDPer1M: 0.40, CacheReadMultiplier: 0.50}},
	}},
	{ID: "gpt-5-mini", Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 0.50, OutputUSDPer1M: 2.00, CacheReadMultiplier: 0.50}},
	}},
	{ID: "gpt-5", Tier: TierHigh, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 2.50, OutputUSDPer1M: 10.00, CacheReadMultiplier: 0.50}},
	}},
	{ID: "gpt-5-chat", Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 2.50, OutputUSDPer1M: 10.00, CacheReadMultiplier: 0.50}},
	}},

	// --- OpenAI GPT-5.4 ---
	{ID: "gpt-5.4-nano", Tier: TierMid, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 0.10, OutputUSDPer1M: 0.40, CacheReadMultiplier: 0.50}},
	}},
	{ID: "gpt-5.4-mini", Tier: TierMid, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 0.40, OutputUSDPer1M: 1.60, CacheReadMultiplier: 0.50}},
	}},
	{ID: "gpt-5.4", Tier: TierHigh, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 3.00, OutputUSDPer1M: 12.00, CacheReadMultiplier: 0.50}},
	}},
	{ID: "gpt-5.4-pro", Tier: TierHigh, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 20.00, OutputUSDPer1M: 80.00, CacheReadMultiplier: 0.50}},
	}},

	// --- OpenAI GPT-5.5 ---
	{ID: "gpt-5.5-nano", Tier: TierMid, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 0.15, OutputUSDPer1M: 0.60, CacheReadMultiplier: 0.50}},
	}},
	{ID: "gpt-5.5-mini", Tier: TierMid, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 0.50, OutputUSDPer1M: 2.50, CacheReadMultiplier: 0.50}},
	}},
	{ID: "gpt-5.5", Tier: TierHigh, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 5.00, OutputUSDPer1M: 40.00, CacheReadMultiplier: 0.50}},
	}},
	{ID: "gpt-5.5-pro", Tier: TierHigh, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenAI, Price: Pricing{InputUSDPer1M: 30.00, OutputUSDPer1M: 120.00, CacheReadMultiplier: 0.50}},
	}},

	// --- Google Gemini 2.x ---
	{ID: "gemini-2.0-flash-lite", Providers: []ProviderBinding{
		{Provider: providers.ProviderGoogle, Price: Pricing{InputUSDPer1M: 0.075, OutputUSDPer1M: 0.30, CacheReadMultiplier: 0.25}},
	}},
	{ID: "gemini-2.0-flash", Providers: []ProviderBinding{
		{Provider: providers.ProviderGoogle, Price: Pricing{InputUSDPer1M: 0.10, OutputUSDPer1M: 0.40, CacheReadMultiplier: 0.25}},
	}},
	{ID: "gemini-2.5-flash-lite", Providers: []ProviderBinding{
		{Provider: providers.ProviderGoogle, Price: Pricing{InputUSDPer1M: 0.10, OutputUSDPer1M: 0.40, CacheReadMultiplier: 0.25}},
	}},
	{ID: "gemini-2.5-flash", Tier: TierLow, Providers: []ProviderBinding{
		{Provider: providers.ProviderGoogle, Price: Pricing{InputUSDPer1M: 0.30, OutputUSDPer1M: 1.20, CacheReadMultiplier: 0.25}},
	}},
	{ID: "gemini-2.5-pro", Providers: []ProviderBinding{
		{Provider: providers.ProviderGoogle, Price: Pricing{InputUSDPer1M: 1.25, OutputUSDPer1M: 5.00, CacheReadMultiplier: 0.25}},
	}},

	// --- Google Gemini 3.x ---
	{ID: "gemini-3.1-flash-lite-preview", Tier: TierLow, Providers: []ProviderBinding{
		{Provider: providers.ProviderGoogle, Price: Pricing{InputUSDPer1M: 0.10, OutputUSDPer1M: 0.40, CacheReadMultiplier: 0.25}},
	}},
	{ID: "gemini-3-flash-preview", Tier: TierMid, Providers: []ProviderBinding{
		{Provider: providers.ProviderGoogle, Price: Pricing{InputUSDPer1M: 0.50, OutputUSDPer1M: 2.00, CacheReadMultiplier: 0.25}},
	}},
	{ID: "gemini-3-pro-preview", Tier: TierHigh, Providers: []ProviderBinding{
		{Provider: providers.ProviderGoogle, Price: Pricing{InputUSDPer1M: 2.00, OutputUSDPer1M: 8.00, CacheReadMultiplier: 0.25}},
	}},
	{ID: "gemini-3.1-pro-preview", Tier: TierHigh, Providers: []ProviderBinding{
		{Provider: providers.ProviderGoogle, Price: Pricing{InputUSDPer1M: 2.00, OutputUSDPer1M: 8.00, CacheReadMultiplier: 0.25}},
	}},
	{ID: "gemini-3.5-flash", Tier: TierMid, Providers: []ProviderBinding{
		{Provider: providers.ProviderGoogle, Price: Pricing{InputUSDPer1M: 1.50, OutputUSDPer1M: 9.00, CacheReadMultiplier: 0.25}},
	}},

	// --- OSS pool ---
	//
	// Each row carries an ordered Providers list. Managed-prod deploys ship
	// only the SOC-2-compliant primary key (Fireworks / DeepInfra / Bedrock /
	// OpenAI / Anthropic / Google) and silently drop the trailing OpenRouter
	// binding at boot. Self-hosters with only an OpenRouter key get every OSS
	// model routed via that trailing binding.
	//
	// Verified against each upstream's live catalog 2026-05-17, re-checked
	// on 2026-05-20 when the v0.55 bundle reintroduced the dedicated-only
	// Qwen rows:
	// - qwen/qwen3-30b-a3b-instruct-2507 — dedicated-only on Fireworks,
	//   absent from DeepInfra + Bedrock. Managed-prod resolves via the
	//   trailing OpenRouter binding.
	// - qwen/qwen3-coder (480B-A35B) — dedicated-only on Fireworks, absent
	//   from DeepInfra + Bedrock us-east-1. Managed-prod resolves via the
	//   trailing OpenRouter binding.
	// - qwen/qwen3-235b-a22b-2507 — AWS published the Instruct-2507 variant
	//   on bedrock-mantle in all major regions (verified 2026-05-22 against
	//   the Bedrock model card). Primary moves to Bedrock; OpenRouter
	//   stays as a trailing fallback for self-hosters without an AWS key.
	//   The OR primary was dropped because we observed non-SSE responses
	//   when OR routed Qwen through Google's hosting (silent CC stalls).
	{ID: "qwen/qwen3-235b-a22b-2507", Tier: TierMid, Providers: []ProviderBinding{
		{Provider: providers.ProviderBedrock, UpstreamID: "qwen.qwen3-235b-a22b-2507",
			Price: Pricing{InputUSDPer1M: 0.2266, OutputUSDPer1M: 0.9064}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.071, OutputUSDPer1M: 0.463}},
	}},
	{ID: "qwen/qwen3-coder-next", Tier: TierMid, Providers: []ProviderBinding{
		{Provider: providers.ProviderBedrock, UpstreamID: "qwen.qwen3-coder-next",
			Price: Pricing{InputUSDPer1M: 0.500, OutputUSDPer1M: 1.200}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.070, OutputUSDPer1M: 0.300}},
	}},
	{ID: "qwen/qwen3-next-80b-a3b-instruct", Tier: TierMid, Providers: []ProviderBinding{
		{Provider: providers.ProviderBedrock, UpstreamID: "qwen.qwen3-next-80b-a3b",
			Price: Pricing{InputUSDPer1M: 0.150, OutputUSDPer1M: 1.200}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.090, OutputUSDPer1M: 1.100}},
	}},
	{ID: "deepseek/deepseek-v4-flash", Tier: TierLow, Providers: []ProviderBinding{
		{Provider: providers.ProviderDeepInfra, UpstreamID: "deepseek-ai/DeepSeek-V4-Flash",
			Price: Pricing{InputUSDPer1M: 0.140, OutputUSDPer1M: 0.280, CacheReadMultiplier: 0.20}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.140, OutputUSDPer1M: 0.280, CacheReadMultiplier: 0.10}},
	}},
	{ID: "deepseek/deepseek-v4-pro", Tier: TierHigh, Providers: []ProviderBinding{
		{Provider: providers.ProviderFireworks, UpstreamID: "accounts/fireworks/models/deepseek-v4-pro",
			Price: Pricing{InputUSDPer1M: 1.740, OutputUSDPer1M: 3.480, CacheReadMultiplier: 0.0862}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.435, OutputUSDPer1M: 0.870, CacheReadMultiplier: 0.10}},
	}},
	{ID: "moonshotai/kimi-k2.5", Tier: TierHigh, Providers: []ProviderBinding{
		{Provider: providers.ProviderBedrock, UpstreamID: "moonshotai.kimi-k2.5",
			Price: Pricing{InputUSDPer1M: 0.600, OutputUSDPer1M: 3.000}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.440, OutputUSDPer1M: 2.000}},
	}},
	{ID: "moonshotai/kimi-k2.6", Tier: TierHigh, Providers: []ProviderBinding{
		{Provider: providers.ProviderFireworks, UpstreamID: "accounts/fireworks/models/kimi-k2p6",
			Price: Pricing{InputUSDPer1M: 0.950, OutputUSDPer1M: 4.000, CacheReadMultiplier: 0.1684}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.950, OutputUSDPer1M: 4.000, CacheReadMultiplier: 0.10}},
	}},
	// AA top-performer additions (2026-05-18).
	//
	// Selection ranked OSS models on the artificialanalysis.ai API by a
	// composite of quality (Intelligence Index v4.0), cost (blended
	// 3:1 input:output), and effective time per 2k-token query
	// (median TTFT + 2000/TPS). Provider availability verified against
	// per-model "API providers" pages and OpenRouter's v1/models API.
	//
	// xiaomi/mimo-v2.5 — DeepInfra now hosts the base variant at parity
	// pricing with OpenRouter ($0.40 in / $2.00 out / $0.08 cached per 1M,
	// verified 2026-05-22). Moving primary off OpenRouter because OR's
	// stream:true honoring is provider-dependent (observed non-SSE
	// responses on the Google-hosted Qwen line). DeepInfra terminates SSE
	// itself so the AnthropicSSETranslator sees real chunks.
	{ID: "xiaomi/mimo-v2.5", Tier: TierMid, Providers: []ProviderBinding{
		{Provider: providers.ProviderDeepInfra, UpstreamID: "XiaomiMiMo/MiMo-V2.5",
			Price: Pricing{InputUSDPer1M: 0.400, OutputUSDPer1M: 2.000, CacheReadMultiplier: 0.20}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.400, OutputUSDPer1M: 2.000, CacheReadMultiplier: 0.10}},
	}},
	{ID: "xiaomi/mimo-v2.5-pro", Tier: TierHigh, Providers: []ProviderBinding{
		{Provider: providers.ProviderDeepInfra, UpstreamID: "XiaomiMiMo/MiMo-V2.5-Pro",
			Price: Pricing{InputUSDPer1M: 1.000, OutputUSDPer1M: 3.000}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 1.000, OutputUSDPer1M: 3.000, CacheReadMultiplier: 0.10}},
	}},
	// qwen3.6-35b-a3b is a 35B-A3B MoE — Intel 44 at ~13s wall-clock per
	// 2k tokens on DeepInfra FP8, the speed/cost end of the new Qwen3.6
	// family. TierLow despite the MoE size because the active parameter
	// budget + AA's Coding Index put it below v4-flash.
	{ID: "qwen/qwen3.6-35b-a3b", Tier: TierLow, Providers: []ProviderBinding{
		{Provider: providers.ProviderDeepInfra, UpstreamID: "Qwen/Qwen3.6-35B-A3B",
			Price: Pricing{InputUSDPer1M: 0.150, OutputUSDPer1M: 0.950}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.150, OutputUSDPer1M: 1.000, CacheReadMultiplier: 0.10}},
	}},
	// minimax-m2.7 sits in an unusual quality/cost spot: Intel 50 at
	// $0.52 blended, cheaper than every TierMid model. Letting the
	// trainer find its niche rather than pinning a tier by price alone.
	{ID: "minimax/minimax-m2.7", Tier: TierHigh, Providers: []ProviderBinding{
		{Provider: providers.ProviderFireworks, UpstreamID: "accounts/fireworks/models/minimax-m2p7",
			Price: Pricing{InputUSDPer1M: 0.300, OutputUSDPer1M: 1.200}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.279, OutputUSDPer1M: 1.200, CacheReadMultiplier: 0.10}},
	}},
	{ID: "z-ai/glm-5", Tier: TierHigh, Providers: []ProviderBinding{
		{Provider: providers.ProviderDeepInfra, UpstreamID: "zai-org/GLM-5",
			Price: Pricing{InputUSDPer1M: 0.600, OutputUSDPer1M: 2.080}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.600, OutputUSDPer1M: 1.920, CacheReadMultiplier: 0.10}},
	}},
	// v0.55 bundle additions (2026-05-20). Fireworks-dedicated rows carry
	// an OpenRouter trailing binding so managed-prod deploys without a
	// Fireworks key can still resolve them; pricing reflects the
	// OpenRouter list price for the public model card on 2026-05-20.
	{ID: "mistralai/mistral-small-2603", Tier: TierMid, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.200, OutputUSDPer1M: 0.600, CacheReadMultiplier: 0.10}},
	}},
	{ID: "qwen/qwen3-30b-a3b-instruct-2507", Tier: TierMid, Providers: []ProviderBinding{
		{Provider: providers.ProviderFireworks, UpstreamID: "accounts/fireworks/models/qwen3-30b-a3b-instruct-2507",
			Price: Pricing{InputUSDPer1M: 0.150, OutputUSDPer1M: 0.600, CacheReadMultiplier: 0.1684}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.100, OutputUSDPer1M: 0.300, CacheReadMultiplier: 0.10}},
	}},
	{ID: "qwen/qwen3-coder", Tier: TierHigh, Providers: []ProviderBinding{
		{Provider: providers.ProviderFireworks, UpstreamID: "accounts/fireworks/models/qwen3-coder-480b-a35b-instruct",
			Price: Pricing{InputUSDPer1M: 0.900, OutputUSDPer1M: 2.700, CacheReadMultiplier: 0.1684}},
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 1.000, OutputUSDPer1M: 5.000, CacheReadMultiplier: 0.10}},
	}},
	{ID: "qwen/qwen3.5-flash-02-23", Tier: TierLow, Providers: []ProviderBinding{
		{Provider: providers.ProviderOpenRouter, Price: Pricing{InputUSDPer1M: 0.050, OutputUSDPer1M: 0.150, CacheReadMultiplier: 0.10}},
	}},
}
