package router

import "regexp"

// ModelCapability identifies a feature that only a subset of models support.
type ModelCapability string

const (
	// CapAdaptiveThinking marks models that accept thinking.type = "adaptive".
	CapAdaptiveThinking ModelCapability = "adaptive_thinking"
	// CapExtendedThinking marks models that accept thinking.type = "enabled" with budget_tokens.
	CapExtendedThinking ModelCapability = "extended_thinking"
	// CapReasoning marks models that accept the reasoning_effort parameter (OpenAI o-series, GPT-5.x).
	CapReasoning ModelCapability = "reasoning"
)

// ModelSpec describes what a model supports. Zero value is safe: an empty
// spec reports Supports == false for every capability, which causes
// provider adapters to strip all capability-gated fields.
type ModelSpec struct {
	capabilities map[ModelCapability]struct{}
}

// Supports reports whether the model advertises cap.
func (s ModelSpec) Supports(cap ModelCapability) bool {
	_, ok := s.capabilities[cap]
	return ok
}

func NewSpec(caps ...ModelCapability) ModelSpec {
	s := ModelSpec{capabilities: make(map[ModelCapability]struct{}, len(caps))}
	for _, c := range caps {
		s.capabilities[c] = struct{}{}
	}
	return s
}

// dateSuffix matches trailing date stamps on model IDs.
// Handles both Anthropic format (-20251001) and OpenAI format (-2024-08-06).
var dateSuffix = regexp.MustCompile(`-\d{4}-?\d{2}-?\d{2}$`)

// Lookup returns the spec for a known model ID. Dated variants
// (e.g. "claude-haiku-4-5-20251001") fall back to the base model.
// Unknown models get a zero-value ModelSpec (no capabilities).
func Lookup(model string) ModelSpec {
	if s, ok := registry[model]; ok {
		return s
	}
	if base := dateSuffix.ReplaceAllString(model, ""); base != model {
		if s, ok := registry[base]; ok {
			return s
		}
	}
	return ModelSpec{}
}

// Anthropic capability sets
var (
	anthropicFull     = NewSpec(CapAdaptiveThinking, CapExtendedThinking)
	anthropicExtended = NewSpec(CapExtendedThinking)
)

// OpenAI capability sets
var (
	openaiReasoning = NewSpec(CapReasoning)
	openaiBase      = NewSpec()
)

// Google capability sets. Gemini's OpenAI-compatible endpoint accepts
// chat-completions shape but does not honor reasoning_effort or any
// Anthropic thinking fields — so the base spec strips both.
var (
	googleBase = NewSpec()
)

// OpenAI-compatible aggregator / vLLM capability sets.
var (
	openAICompatBase = NewSpec()
)

var registry = map[string]ModelSpec{
	// ── Anthropic: current (adaptive + extended) ──
	"claude-opus-4-7":   anthropicFull,
	"claude-sonnet-4-6": anthropicFull,
	"claude-opus-4-6":   anthropicFull,

	// ── Anthropic: extended only, no adaptive ──
	// Empirical (2026-04-30): claude-haiku-4-5 returns 400
	// "adaptive thinking is not supported on this model" when a Claude
	// Code body with thinking.type=adaptive is forwarded through the
	// router after a downgrade from claude-opus-4-7. Extended thinking
	// per Anthropic's docs.
	"claude-haiku-4-5": anthropicExtended,
	"claude-opus-4-5":  anthropicExtended,
	"claude-opus-4-1":  anthropicExtended,
	"claude-opus-4-0":  anthropicExtended,

	// ── Anthropic: previous (no thinking) ──
	"claude-sonnet-4-5": NewSpec(),
	"claude-sonnet-4-0": NewSpec(),

	// ── OpenAI: GPT-5.5 frontier (April 2026 release; reasoning) ──
	"gpt-5.5":      openaiReasoning,
	"gpt-5.5-pro":  openaiReasoning,
	"gpt-5.5-mini": openaiReasoning,
	"gpt-5.5-nano": openaiReasoning,

	// ── OpenAI: GPT-5.4 (Q1 2026; reasoning) ──
	"gpt-5.4":      openaiReasoning,
	"gpt-5.4-pro":  openaiReasoning,
	"gpt-5.4-mini": openaiReasoning,
	"gpt-5.4-nano": openaiReasoning,

	// ── OpenAI: GPT-5.x earlier (reasoning) ──
	"gpt-5.3":     openaiReasoning,
	"gpt-5.2":     openaiReasoning,
	"gpt-5.2-pro": openaiReasoning,
	"gpt-5.1":     openaiReasoning,
	"gpt-5":       openaiReasoning,
	"gpt-5-chat":  openaiReasoning,
	"gpt-5-pro":   openaiReasoning,
	"gpt-5-mini":  openaiReasoning,
	"gpt-5-nano":  openaiReasoning,

	// ── OpenAI: o-series (reasoning) ──
	"o3":      openaiReasoning,
	"o3-pro":  openaiReasoning,
	"o3-mini": openaiReasoning,
	"o4-mini": openaiReasoning,
	"o1":      openaiReasoning,
	"o1-pro":  openaiReasoning,
	"o1-mini": openaiReasoning,

	// ── OpenAI: GPT-4.x and older (no reasoning) ──
	"gpt-4.1":      openaiBase,
	"gpt-4.1-mini": openaiBase,
	"gpt-4.1-nano": openaiBase,
	"gpt-4o":       openaiBase,
	"gpt-4o-mini":  openaiBase,
	"gpt-4-turbo":  openaiBase,
	"gpt-4":        openaiBase,

	// ── Google: Gemini 3.x frontier (April 2026; OpenAI-compatible API) ──
	// Frontier 3.x family is preview-only as of 2026-04-30; the
	// `-preview` suffix is the canonical model ID Google's OpenAI-compat
	// endpoint accepts. Gemini 3 Pro Preview is itself deprecated
	// 2026-03-09 in favor of gemini-3.1-pro-preview.
	"gemini-3-pro-preview":          googleBase,
	"gemini-3.1-pro-preview":        googleBase,
	"gemini-3-flash-preview":        googleBase,
	"gemini-3.1-flash-lite-preview": googleBase,
	"gemini-3.1-flash-live-preview": googleBase,

	// ── Google: Gemini 2.x families (still routable; bench-trained) ──
	"gemini-2.5-pro":        googleBase,
	"gemini-2.5-flash":      googleBase,
	"gemini-2.5-flash-lite": googleBase,
	"gemini-2.0-flash":      googleBase,
	"gemini-2.0-flash-lite": googleBase,

	// -- OpenRouter / OpenAI-compatible OSS pool: Qwen3 R2-Router clone --
	"qwen/qwen3-235b-a22b-2507":        openAICompatBase,
	"qwen/qwen3-30b-a3b-instruct-2507": openAICompatBase,
	"qwen/qwen3-coder-next":            openAICompatBase,
	"qwen/qwen3-next-80b-a3b-instruct": openAICompatBase,
}
