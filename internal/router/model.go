package router

import "regexp"

// ModelCapability identifies a feature that only a subset of models support.
type ModelCapability string

const (
	CapAdaptiveThinking ModelCapability = "adaptive_thinking"
	CapExtendedThinking ModelCapability = "extended_thinking"
	CapReasoning        ModelCapability = "reasoning"
)

// ModelSpec describes what a model supports. Zero value is safe: provider
// adapters strip all capability-gated fields when Supports reports false.
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

// dateSuffix matches Anthropic (-20251001) and OpenAI (-2024-08-06) trailing date stamps.
var dateSuffix = regexp.MustCompile(`-\d{4}-?\d{2}-?\d{2}$`)

// Lookup returns the spec for a known model ID. Dated variants fall back
// to the base model. Unknown models get a zero-value ModelSpec.
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

var (
	anthropicFull     = NewSpec(CapAdaptiveThinking, CapExtendedThinking)
	anthropicExtended = NewSpec(CapExtendedThinking)
)

var (
	openaiReasoning = NewSpec(CapReasoning)
	openaiBase      = NewSpec()
)

// Gemini's OpenAI-compatible endpoint does not honor reasoning_effort or
// Anthropic thinking fields, so the base spec strips both.
var googleBase = NewSpec()

var openAICompatBase = NewSpec()

var registry = map[string]ModelSpec{
	// Anthropic: adaptive + extended
	"claude-opus-4-7":   anthropicFull,
	"claude-sonnet-4-6": anthropicFull,
	"claude-opus-4-6":   anthropicFull,

	// Anthropic: extended only.
	// Empirical (2026-04-30): claude-haiku-4-5 returns 400
	// "adaptive thinking is not supported on this model" when a Claude
	// Code body with thinking.type=adaptive is forwarded through the
	// router after a downgrade from claude-opus-4-7.
	"claude-haiku-4-5": anthropicExtended,
	"claude-opus-4-5":  anthropicExtended,
	"claude-opus-4-1":  anthropicExtended,
	"claude-opus-4-0":  anthropicExtended,

	// Anthropic: no thinking
	"claude-sonnet-4-5": NewSpec(),
	"claude-sonnet-4-0": NewSpec(),

	// OpenAI: GPT-5.5
	"gpt-5.5":      openaiReasoning,
	"gpt-5.5-pro":  openaiReasoning,
	"gpt-5.5-mini": openaiReasoning,
	"gpt-5.5-nano": openaiReasoning,

	// OpenAI: GPT-5.4
	"gpt-5.4":      openaiReasoning,
	"gpt-5.4-pro":  openaiReasoning,
	"gpt-5.4-mini": openaiReasoning,
	"gpt-5.4-nano": openaiReasoning,

	// OpenAI: GPT-5.x earlier
	"gpt-5.3":     openaiReasoning,
	"gpt-5.2":     openaiReasoning,
	"gpt-5.2-pro": openaiReasoning,
	"gpt-5.1":     openaiReasoning,
	"gpt-5":       openaiReasoning,
	"gpt-5-chat":  openaiReasoning,
	"gpt-5-pro":   openaiReasoning,
	"gpt-5-mini":  openaiReasoning,
	"gpt-5-nano":  openaiReasoning,

	// OpenAI: o-series
	"o3":      openaiReasoning,
	"o3-pro":  openaiReasoning,
	"o3-mini": openaiReasoning,
	"o4-mini": openaiReasoning,
	"o1":      openaiReasoning,
	"o1-pro":  openaiReasoning,
	"o1-mini": openaiReasoning,

	// OpenAI: GPT-4.x and older
	"gpt-4.1":      openaiBase,
	"gpt-4.1-mini": openaiBase,
	"gpt-4.1-nano": openaiBase,
	"gpt-4o":       openaiBase,
	"gpt-4o-mini":  openaiBase,
	"gpt-4-turbo":  openaiBase,
	"gpt-4":        openaiBase,

	// Google: Gemini 3.x frontier. Preview-only as of 2026-04-30; the
	// `-preview` suffix is the canonical model ID. gemini-3-pro-preview
	// is deprecated 2026-03-09 in favor of gemini-3.1-pro-preview.
	"gemini-3-pro-preview":          googleBase,
	"gemini-3.1-pro-preview":        googleBase,
	"gemini-3-flash-preview":        googleBase,
	"gemini-3.1-flash-lite-preview": googleBase,
	"gemini-3.1-flash-live-preview": googleBase,

	// Google: Gemini 2.x
	"gemini-2.5-pro":        googleBase,
	"gemini-2.5-flash":      googleBase,
	"gemini-2.5-flash-lite": googleBase,
	"gemini-2.0-flash":      googleBase,
	"gemini-2.0-flash-lite": googleBase,

	// OpenRouter / OpenAI-compatible OSS pool
	"qwen/qwen3-235b-a22b-2507":        openAICompatBase,
	"qwen/qwen3-30b-a3b-instruct-2507": openAICompatBase,
	"qwen/qwen3-coder-next":            openAICompatBase,
	"qwen/qwen3-next-80b-a3b-instruct": openAICompatBase,
}
