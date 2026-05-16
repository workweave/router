package translate

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ReasonUserForceModel is the decision_reason stored in the session pin when a
// user has forced a specific model via /force-model. The turn loop treats this
// as an immutable sticky: scorer and planner are bypassed for the session's
// lifetime until /unforce-model clears it.
const ReasonUserForceModel = "user_forced"

// ForceModelResult holds the parsed outcome of a force-model command.
type ForceModelResult struct {
	// Model is the target model name; empty when Clear is true.
	Model string
	// Clear is true for /unforce-model.
	Clear bool
}

// ExtractForceModelCommand scans the last user-role message in env for a
// /force-model <model> or /unforce-model directive. When found, it strips the
// command line from env.body and returns the parsed result.
// Returns (zero, false) when no command is present.
func (env *RequestEnvelope) ExtractForceModelCommand() (ForceModelResult, bool) {
	switch env.format {
	case FormatAnthropic, FormatOpenAI:
		return env.extractForceModelFromMessages()
	default:
		return ForceModelResult{}, false
	}
}

func (env *RequestEnvelope) extractForceModelFromMessages() (ForceModelResult, bool) {
	msgs := gjson.GetBytes(env.body, "messages")
	if !msgs.IsArray() {
		return ForceModelResult{}, false
	}

	lastIdx := -1
	var lastContent gjson.Result
	msgs.ForEach(func(key, msg gjson.Result) bool {
		if msg.Get("role").String() == "user" {
			lastIdx = int(key.Int())
			lastContent = msg.Get("content")
		}
		return true
	})
	if lastIdx < 0 {
		return ForceModelResult{}, false
	}

	idxStr := strconv.Itoa(lastIdx)

	switch {
	case lastContent.Type == gjson.String:
		res, found, stripped := parseForceModelCommand(lastContent.String())
		if !found {
			return ForceModelResult{}, false
		}
		if newBody, err := sjson.SetBytes(env.body, "messages."+idxStr+".content", stripped); err == nil {
			env.body = newBody
		}
		return res, true

	case lastContent.Type == gjson.JSON && lastContent.IsArray():
		textIdx := -1
		var textVal string
		lastContent.ForEach(func(key, block gjson.Result) bool {
			if block.Get("type").String() == "text" && textIdx < 0 {
				textIdx = int(key.Int())
				textVal = block.Get("text").String()
			}
			return true
		})
		if textIdx < 0 {
			return ForceModelResult{}, false
		}
		res, found, stripped := parseForceModelCommand(textVal)
		if !found {
			return ForceModelResult{}, false
		}
		blockPath := "messages." + idxStr + ".content." + strconv.Itoa(textIdx) + ".text"
		if newBody, err := sjson.SetBytes(env.body, blockPath, stripped); err == nil {
			env.body = newBody
		}
		return res, true

	default:
		return ForceModelResult{}, false
	}
}

// parseForceModelCommand scans text line-by-line for a /force-model or
// /unforce-model directive. Returns the parsed result, whether found, and
// the text with the command line removed.
func parseForceModelCommand(text string) (res ForceModelResult, found bool, stripped string) {
	lines := strings.Split(text, "\n")
	cmdIdx := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(trimmed, "/force-model "); ok {
			model := strings.TrimSpace(after)
			if model != "" {
				res = ForceModelResult{Model: model}
				found = true
				cmdIdx = i
			}
			break
		}
		if trimmed == "/unforce-model" {
			res = ForceModelResult{Clear: true}
			found = true
			cmdIdx = i
			break
		}
	}
	if !found {
		return ForceModelResult{}, false, text
	}
	remaining := make([]string, 0, len(lines)-1)
	remaining = append(remaining, lines[:cmdIdx]...)
	remaining = append(remaining, lines[cmdIdx+1:]...)
	stripped = strings.TrimSpace(strings.Join(remaining, "\n"))
	return res, true, stripped
}
