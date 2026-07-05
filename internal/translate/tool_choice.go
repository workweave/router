package translate

import "github.com/tidwall/gjson"

// toolChoiceKind is the source-format-neutral tool_choice value shared by
// the four emit_*.go renderers.
type toolChoiceKind int

const (
	// toolChoiceAbsent covers a missing tool_choice as well as any shape this
	// package doesn't recognize (unknown string, malformed object, empty
	// named-tool name) — all of which every renderer treats as a no-op.
	toolChoiceAbsent toolChoiceKind = iota
	toolChoiceAuto
	// toolChoiceRequired is Anthropic's "any" / OpenAI's "required".
	toolChoiceRequired
	toolChoiceNone
	// toolChoiceNamed pins a single tool by name (Anthropic's {"type":"tool"},
	// OpenAI's {"type":"function"}); the name is returned alongside the kind.
	toolChoiceNamed
)

// anthropicToolChoice parses an Anthropic-shape tool_choice field
// ({"type":"auto"|"any"|"none"|"tool","name":"..."}) into a neutral kind.
func anthropicToolChoice(body []byte) (toolChoiceKind, string) {
	r := gjson.GetBytes(body, "tool_choice")
	if !r.Exists() || !r.IsObject() {
		return toolChoiceAbsent, ""
	}
	switch r.Get("type").String() {
	case "auto":
		return toolChoiceAuto, ""
	case "any":
		return toolChoiceRequired, ""
	case "none":
		return toolChoiceNone, ""
	case "tool":
		name := r.Get("name")
		if name.Type != gjson.String || name.String() == "" {
			return toolChoiceAbsent, ""
		}
		return toolChoiceNamed, name.String()
	default:
		return toolChoiceAbsent, ""
	}
}

// openAIToolChoice parses an OpenAI-shape tool_choice field into a neutral kind.
func openAIToolChoice(body []byte) (toolChoiceKind, string) {
	r := gjson.GetBytes(body, "tool_choice")
	if !r.Exists() {
		return toolChoiceAbsent, ""
	}
	if r.Type == gjson.String {
		switch r.String() {
		case "auto":
			return toolChoiceAuto, ""
		case "required":
			return toolChoiceRequired, ""
		case "none":
			return toolChoiceNone, ""
		default:
			return toolChoiceAbsent, ""
		}
	}
	if r.IsObject() && r.Get("type").String() == "function" {
		name := r.Get("function.name")
		if name.Type != gjson.String || name.String() == "" {
			return toolChoiceAbsent, ""
		}
		return toolChoiceNamed, name.String()
	}
	return toolChoiceAbsent, ""
}
