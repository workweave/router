package router

import (
	"fmt"
	"regexp"
)

// ModelCapability identifies a feature that only a subset of models support.
type ModelCapability string

const (
	CapAdaptiveThinking ModelCapability = "adaptive_thinking"
	CapExtendedThinking ModelCapability = "extended_thinking"
	CapReasoning        ModelCapability = "reasoning"
	CapExtendedContext  ModelCapability = "extended_context"
	// CapXhighEffort marks models supporting effort "xhigh" (opus-4-7+ only;
	// sonnet-4-6 400s on it). Emit clamps to "max" when unsupported.
	CapXhighEffort ModelCapability = "xhigh_effort"
)

// ModelSpec describes what a model supports. Zero value is safe: provider
// adapters strip all capability-gated fields when Supports reports false.
type ModelSpec struct {
	capabilities map[ModelCapability]struct{}
	reasoning    ReasoningCapabilities
}

// ReasoningCapabilities declares the reasoning semantics a model can preserve.
// Levels are ordered from least to most expensive for deterministic clamping.
type ReasoningCapabilities struct {
	Levels         []string
	SupportsAuto   bool
	SupportsBudget bool
	AlwaysOn       bool
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

// NewSpecWithReasoning creates a model spec with explicit reasoning support.
func NewSpecWithReasoning(reasoning ReasoningCapabilities, caps ...ModelCapability) ModelSpec {
	s := NewSpec(caps...)
	s.reasoning = reasoning
	return s
}

// Reasoning returns a copy of the model's declared reasoning capabilities.
func (s ModelSpec) Reasoning() ReasoningCapabilities {
	levels := append([]string(nil), s.reasoning.Levels...)
	return ReasoningCapabilities{
		Levels: levels, SupportsAuto: s.reasoning.SupportsAuto,
		SupportsBudget: s.reasoning.SupportsBudget, AlwaysOn: s.reasoning.AlwaysOn,
	}
}

// ValidateReasoningCapabilities verifies that catalog reasoning declarations
// are internally coherent.
func (s ModelSpec) ValidateReasoningCapabilities() error {
	if len(s.reasoning.Levels) == 0 {
		if s.reasoning.SupportsAuto || s.reasoning.SupportsBudget || s.reasoning.AlwaysOn {
			return fmt.Errorf("reasoning modifiers require at least one supported level")
		}
		return nil
	}
	seen := make(map[string]struct{}, len(s.reasoning.Levels))
	for _, level := range s.reasoning.Levels {
		if level == "" {
			return fmt.Errorf("reasoning level cannot be empty")
		}
		if _, exists := seen[level]; exists {
			return fmt.Errorf("duplicate reasoning level %q", level)
		}
		seen[level] = struct{}{}
	}
	return nil
}

// dateSuffix matches Anthropic (-20251001) and OpenAI (-2024-08-06) trailing date stamps.
var dateSuffix = regexp.MustCompile(`-\d{4}-?\d{2}-?\d{2}$`)

// StripDateSuffix removes a trailing Anthropic-style (-20251001) or OpenAI-style
// (-2024-08-06) date stamp from a model ID. Returns the input unchanged when no suffix matches.
func StripDateSuffix(model string) string {
	return dateSuffix.ReplaceAllString(model, "")
}

// Lookup returns the spec for a known model ID. Dated variants (e.g.
// "-20251001") fall back to the base model; unknown models get zero-value.
func Lookup(model string) ModelSpec {
	if s, ok := registry[model]; ok {
		return s
	}
	if base := StripDateSuffix(model); base != model {
		if s, ok := registry[base]; ok {
			return s
		}
	}
	return ModelSpec{}
}

var (
	// Opus/Sonnet 4.6+ only accept thinking.type=adaptive (legacy "enabled" 400s
	// since output_config.effort rollout) and support 1M context via the
	// context-1m-2025-08-07 beta; Haiku 4.5 and Sonnet 4.5 top out at 200K.
	anthropicAdaptive = NewSpecWithReasoning(ReasoningCapabilities{Levels: []string{"low", "medium", "high", "max"}, AlwaysOn: true}, CapAdaptiveThinking, CapExtendedContext)
	// Opus 4.7+ (and fable) additionally accept effort level "xhigh"; the
	// older adaptive models (opus-4-6, sonnet-4-6) top out at "max".
	anthropicAdaptiveXhigh = NewSpecWithReasoning(ReasoningCapabilities{Levels: []string{"low", "medium", "high", "max", "xhigh"}, AlwaysOn: true}, CapAdaptiveThinking, CapExtendedContext, CapXhighEffort)
	anthropicExtended      = NewSpecWithReasoning(ReasoningCapabilities{Levels: []string{"low", "medium", "high"}, SupportsBudget: true}, CapExtendedThinking)
)

var (
	openaiReasoning = NewSpecWithReasoning(ReasoningCapabilities{Levels: []string{"low", "medium", "high"}, SupportsBudget: true}, CapReasoning)
	openaiBase      = NewSpec()
)

// Gemini's OpenAI-compatible endpoint does not honor reasoning_effort or
// Anthropic thinking fields, so the base spec strips both.
var googleBase = NewSpecWithReasoning(ReasoningCapabilities{Levels: []string{"low", "medium", "high"}, SupportsBudget: true})
var google3Base = NewSpecWithReasoning(ReasoningCapabilities{Levels: []string{"low", "medium", "high"}, AlwaysOn: true})

var openAICompatBase = NewSpec()

var registry = map[string]ModelSpec{
	// claude-fable-5 has adaptive thinking always on (disabled is rejected);
	// 1M context is native, so CapExtendedContext's beta header is a no-op.
	"claude-fable-5":  anthropicAdaptiveXhigh,
	"claude-opus-5":   anthropicAdaptiveXhigh,
	"claude-opus-4-8": anthropicAdaptiveXhigh,
	"claude-opus-4-7": anthropicAdaptiveXhigh,
	// claude-sonnet-5 mirrors sonnet-4-6: no xhigh, since Sonnet tops out at
	// effort "max" and marking xhigh unsupported clamps rather than 400s.
	"claude-sonnet-5":   anthropicAdaptive,
	"claude-sonnet-4-6": anthropicAdaptive,
	"claude-opus-4-6":   anthropicAdaptive,

	// claude-haiku-4-5 400s on thinking.type=adaptive, which Claude Code
	// bodies carry after a downgrade from opus.
	"claude-haiku-4-5": anthropicExtended,
	"claude-opus-4-5":  anthropicExtended,
	"claude-opus-4-1":  anthropicExtended,
	"claude-opus-4-0":  anthropicExtended,

	"claude-sonnet-4-5": NewSpec(),
	"claude-sonnet-4-0": NewSpec(),

	"gpt-5.6-sol":   openaiReasoning,
	"gpt-5.6-terra": openaiReasoning,
	"gpt-5.6-luna":  openaiReasoning,

	"gpt-5.5":      openaiReasoning,
	"gpt-5.5-pro":  openaiReasoning,
	"gpt-5.5-mini": openaiReasoning,
	"gpt-5.5-nano": openaiReasoning,

	// grok-4.5: reasoning_effort low/medium/high (default high); cannot disable;
	// rejects stop / presence / frequency penalties (CapReasoning strips stop).
	"grok-4.5": openaiReasoning,

	"gpt-5.4":      openaiReasoning,
	"gpt-5.4-pro":  openaiReasoning,
	"gpt-5.4-mini": openaiReasoning,
	"gpt-5.4-nano": openaiReasoning,

	"gpt-5.3":     openaiReasoning,
	"gpt-5.2":     openaiReasoning,
	"gpt-5.2-pro": openaiReasoning,
	"gpt-5.1":     openaiReasoning,
	"gpt-5":       openaiReasoning,
	"gpt-5-chat":  openaiReasoning,
	"gpt-5-pro":   openaiReasoning,
	"gpt-5-mini":  openaiReasoning,
	"gpt-5-nano":  openaiReasoning,

	"o3":      openaiReasoning,
	"o3-pro":  openaiReasoning,
	"o3-mini": openaiReasoning,
	"o4-mini": openaiReasoning,
	"o1":      openaiReasoning,
	"o1-pro":  openaiReasoning,
	"o1-mini": openaiReasoning,

	"gpt-4.1":      openaiBase,
	"gpt-4.1-mini": openaiBase,
	"gpt-4.1-nano": openaiBase,
	"gpt-4o":       openaiBase,
	"gpt-4o-mini":  openaiBase,
	"gpt-4-turbo":  openaiBase,
	"gpt-4":        openaiBase,

	// Gemini 3.x preview models use `-preview` suffix as canonical ID.
	// gemini-3-pro-preview is deprecated 2026-03-09 in favor of 3.1.
	"gemini-3-pro-preview":          google3Base,
	"gemini-3.1-pro-preview":        google3Base,
	"gemini-3-flash-preview":        google3Base,
	"gemini-3.1-flash-lite-preview": google3Base,
	"gemini-3.1-flash-live-preview": google3Base,
	"gemini-3.5-flash-lite":         google3Base,
	"gemini-3.6-flash":              google3Base,

	"gemini-2.5-pro":        googleBase,
	"gemini-2.5-flash":      googleBase,
	"gemini-2.5-flash-lite": googleBase,
	"gemini-2.0-flash":      googleBase,
	"gemini-2.0-flash-lite": googleBase,

	"qwen/qwen3-235b-a22b-2507":        openAICompatBase,
	"qwen/qwen3-30b-a3b-instruct-2507": openAICompatBase,
	"qwen/qwen3-coder-next":            openAICompatBase,
	"qwen/qwen3-next-80b-a3b-instruct": openAICompatBase,
}

// ValidateCatalogReasoningCapabilities validates every declared model at
// startup composition time or in catalog tests.
func ValidateCatalogReasoningCapabilities() error {
	for model, spec := range registry {
		if err := spec.ValidateReasoningCapabilities(); err != nil {
			return fmt.Errorf("model %q: %w", model, err)
		}
	}
	return nil
}
