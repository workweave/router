package translate

import (
	"errors"
	"fmt"
	"strings"

	"workweave/router/internal/router"

	"github.com/tidwall/gjson"
)

var ErrReasoningIncompatible = errors.New("reasoning intent is not representable")

// ReasoningIntentKind describes the request-level reasoning semantics.
type ReasoningIntentKind string

const (
	ReasoningDisabled ReasoningIntentKind = "disabled"
	ReasoningAuto     ReasoningIntentKind = "auto"
	ReasoningLevel    ReasoningIntentKind = "level"
	ReasoningBudget   ReasoningIntentKind = "budget"
)

// ReasoningIntent is the canonical reasoning request preserved across wire
// formats before it is checked against a target model's capabilities.
type ReasoningIntent struct {
	Kind               ReasoningIntentKind
	Level              string
	BudgetTokens       int64
	Source             string
	Explicit           bool
	NormalizationNotes []string
}

// ReasoningIntent returns the reasoning request carried by this envelope.
func (e *RequestEnvelope) ReasoningIntent() ReasoningIntent {
	if e == nil {
		return ReasoningIntent{}
	}
	return ParseReasoningIntent(e.format, e.body)
}

// ParseReasoningIntent extracts a wire-format-specific reasoning request.
func ParseReasoningIntent(format Format, body []byte) ReasoningIntent {
	if intent := parseReasoningEffort(gjson.GetBytes(body, "reasoning_effort"), "openai.reasoning_effort"); intent.Kind != "" {
		return intent
	}
	if intent := parseReasoningEffort(gjson.GetBytes(body, "reasoning.effort"), "openai.responses.reasoning.effort"); intent.Kind != "" {
		return intent
	}

	thinking := gjson.GetBytes(body, "thinking")
	if thinking.Exists() {
		switch thinking.Get("type").String() {
		case "disabled":
			return ReasoningIntent{Kind: ReasoningDisabled, Source: "anthropic.thinking", Explicit: true}
		case "adaptive":
			if intent := parseReasoningEffort(gjson.GetBytes(body, "output_config.effort"), "anthropic.output_config.effort"); intent.Kind != "" {
				return intent
			}
			return ReasoningIntent{Kind: ReasoningAuto, Source: "anthropic.thinking", Explicit: true}
		case "enabled":
			return ReasoningIntent{Kind: ReasoningBudget, BudgetTokens: thinking.Get("budget_tokens").Int(), Source: "anthropic.thinking", Explicit: true}
		}
	}
	if intent := parseReasoningEffort(gjson.GetBytes(body, "output_config.effort"), "anthropic.output_config.effort"); intent.Kind != "" {
		return intent
	}

	if format == FormatGemini {
		config := gjson.GetBytes(body, "generationConfig.thinkingConfig")
		if level := config.Get("thinkingLevel"); level.Exists() {
			return parseReasoningEffort(level, "gemini.thinkingConfig.thinkingLevel")
		}
		if budget := config.Get("thinkingBudget"); budget.Exists() && budget.Type == gjson.Number {
			if budget.Int() == 0 {
				return ReasoningIntent{Kind: ReasoningDisabled, Source: "gemini.thinkingConfig.thinkingBudget", Explicit: true}
			}
			return ReasoningIntent{Kind: ReasoningBudget, BudgetTokens: budget.Int(), Source: "gemini.thinkingConfig.thinkingBudget", Explicit: true}
		}
	}
	return ReasoningIntent{}
}

func parseReasoningEffort(value gjson.Result, source string) ReasoningIntent {
	if !value.Exists() || value.Type != gjson.String {
		return ReasoningIntent{}
	}
	effort := strings.ToLower(value.String())
	switch effort {
	case "", "none", "disabled":
		return ReasoningIntent{Kind: ReasoningDisabled, Source: source, Explicit: true}
	case "auto", "adaptive":
		return ReasoningIntent{Kind: ReasoningAuto, Source: source, Explicit: true}
	default:
		return ReasoningIntent{Kind: ReasoningLevel, Level: effort, Source: source, Explicit: true}
	}
}

// ApplyReasoningIntent validates the request against a target model. A forced
// effort retains the caller's source while recording the router override.
func ApplyReasoningIntent(intent ReasoningIntent, spec router.ModelSpec, forcedLevel string) (ReasoningIntent, error) {
	if forcedLevel != "" {
		intent = ReasoningIntent{
			Kind: ReasoningLevel, Level: strings.ToLower(forcedLevel), Source: "router.force_reasoning_effort", Explicit: true,
			NormalizationNotes: append(intent.NormalizationNotes, "router override replaced client/default reasoning intent"),
		}
	}
	if intent.Kind == "" {
		return intent, nil
	}
	caps := spec.Reasoning()
	if len(caps.Levels) == 0 {
		return ReasoningIntent{}, fmt.Errorf("%w: target model has no reasoning support", ErrReasoningIncompatible)
	}
	switch intent.Kind {
	case ReasoningDisabled:
		if caps.AlwaysOn {
			intent.Kind = ReasoningLevel
			intent.Level = caps.Levels[0]
			intent.NormalizationNotes = append(intent.NormalizationNotes, "disabled maps to the model's lowest always-on level")
		}
		return intent, nil
	case ReasoningAuto:
		// Omitting a target-specific setting preserves the target's default.
		return intent, nil
	case ReasoningBudget:
		if !caps.SupportsBudget {
			return ReasoningIntent{}, fmt.Errorf("%w: target model does not support reasoning budgets", ErrReasoningIncompatible)
		}
		return intent, nil
	case ReasoningLevel:
		if intent.Level == "" {
			return ReasoningIntent{}, fmt.Errorf("%w: missing reasoning level", ErrReasoningIncompatible)
		}
		if containsReasoningLevel(caps.Levels, intent.Level) {
			return intent, nil
		}
		clamped := nearestReasoningLevel(caps.Levels, intent.Level)
		intent.NormalizationNotes = append(intent.NormalizationNotes, "unsupported reasoning level clamped to "+clamped)
		intent.Level = clamped
		return intent, nil
	default:
		return ReasoningIntent{}, fmt.Errorf("%w: unknown intent %q", ErrReasoningIncompatible, intent.Kind)
	}
}

func containsReasoningLevel(levels []string, level string) bool {
	for _, candidate := range levels {
		if candidate == level {
			return true
		}
	}
	return false
}

func nearestReasoningLevel(levels []string, wanted string) string {
	order := map[string]int{"low": 0, "medium": 1, "high": 2, "max": 3, "xhigh": 4}
	wantedRank, known := order[wanted]
	if !known {
		return levels[len(levels)-1]
	}
	best := levels[0]
	bestDistance := 1 << 30
	for _, candidate := range levels {
		rank, known := order[candidate]
		if !known {
			continue
		}
		distance := rank - wantedRank
		if distance < 0 {
			distance = -distance
		}
		if distance < bestDistance {
			best, bestDistance = candidate, distance
		}
	}
	return best
}
