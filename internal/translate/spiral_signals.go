package translate

import (
	"strings"

	"github.com/tidwall/gjson"
)

// Spiral-signal extraction: cheap, per-request scans of the message history
// that feed the proxy's shadow-mode spiral detector (see
// internal/proxy/spiral_detection.go). Like the loop-detection extractors in
// this package, everything here is a pure function of the request body — the
// full history arrives on every turn, so no cross-turn state is needed.

// toolResultErrorScanLimit caps how many bytes of a tool_result's string
// content are scanned for error markers. Tool results can be hundreds of KB
// (full test logs); errors announce themselves early.
const toolResultErrorScanLimit = 2048

// toolResultErrorMarkers are substrings in a tool_result's content that mark
// the result as errored even when the client did not set is_error. Claude
// Code sets is_error for tool-level failures (bad Edit old_string, command
// not found) but a Bash tool call that ran a test suite to a red result is a
// successful tool call carrying a failure payload — the marker scan catches
// those. Kept deliberately short and high-precision; the offline trajectory
// audit graded the combined flag+marker signal at AUC 0.73-0.86.
var toolResultErrorMarkers = []string{
	"Traceback (most recent call last)",
	"FAILED",
	"Error:",
	"error:",
}

// ToolResultErrorStats summarizes tool_result error evidence in the message
// history. TrailingErrStreak counts consecutive errored results from the end
// of the history backwards — the "agent keeps trying things that fail" shape;
// one healthy result resets it.
type ToolResultErrorStats struct {
	Total             int
	Errored           int
	TrailingErrStreak int
}

// ToolResultErrors scans user-role tool_result blocks (Anthropic format) for
// error evidence: the is_error flag, or a high-precision error marker within
// the first toolResultErrorScanLimit bytes of string content. Returns zero
// stats for non-Anthropic formats — OpenAI tool-role messages carry no error
// flag and the Claude Code traffic this detector targets is Anthropic-format.
func (e *RequestEnvelope) ToolResultErrors() ToolResultErrorStats {
	if e.format != FormatAnthropic {
		return ToolResultErrorStats{}
	}
	var stats ToolResultErrorStats
	msgs := gjson.GetBytes(e.body, "messages")
	if !msgs.IsArray() {
		return stats
	}
	msgs.ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() != "user" {
			return true
		}
		content := msg.Get("content")
		if !content.IsArray() {
			return true
		}
		content.ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() != "tool_result" {
				return true
			}
			stats.Total++
			if toolResultIsErrored(block) {
				stats.Errored++
				stats.TrailingErrStreak++
			} else {
				stats.TrailingErrStreak = 0
			}
			return true
		})
		return true
	})
	return stats
}

// toolResultIsErrored reports whether a single tool_result block carries
// error evidence: the is_error flag, or an error marker early in its string
// content. Content can be a plain string or an array of text blocks; only
// the first toolResultErrorScanLimit bytes are scanned either way.
func toolResultIsErrored(block gjson.Result) bool {
	if block.Get("is_error").Bool() {
		return true
	}
	content := block.Get("content")
	var text string
	switch {
	case content.Type == gjson.String:
		text = content.String()
	case content.IsArray():
		var b strings.Builder
		content.ForEach(func(_, inner gjson.Result) bool {
			if inner.Get("type").String() == "text" {
				b.WriteString(inner.Get("text").String())
			}
			return b.Len() < toolResultErrorScanLimit
		})
		text = b.String()
	default:
		return false
	}
	if len(text) > toolResultErrorScanLimit {
		text = text[:toolResultErrorScanLimit]
	}
	for _, marker := range toolResultErrorMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// ToolCallFilePath is one assistant tool invocation that targets a file:
// the tool name plus the file path argument it carried.
type ToolCallFilePath struct {
	Name string
	Path string
}

// AssistantToolCallFilePaths returns, in message order, every assistant
// tool_use invocation that carries a file_path or notebook_path argument.
// The proxy's spiral detector counts repeat edits to the same path (the
// same-file-thrash death-march shape). Anthropic format only — mirrors
// AssistantToolCallSignatures' nudge skip.
func (e *RequestEnvelope) AssistantToolCallFilePaths() []ToolCallFilePath {
	if e.format != FormatAnthropic {
		return nil
	}
	msgs := gjson.GetBytes(e.body, "messages")
	if !msgs.IsArray() {
		return nil
	}
	var out []ToolCallFilePath
	msgs.ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() != "assistant" {
			return true
		}
		content := msg.Get("content")
		if !content.IsArray() {
			return true
		}
		content.ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() != "tool_use" {
				return true
			}
			if strings.HasPrefix(block.Get("id").String(), "toolu_router_nudge_") {
				return true
			}
			name := block.Get("name").String()
			if name == "" {
				return true
			}
			path := block.Get("input.file_path").String()
			if path == "" {
				path = block.Get("input.notebook_path").String()
			}
			if path == "" {
				return true
			}
			out = append(out, ToolCallFilePath{Name: name, Path: path})
			return true
		})
		return true
	})
	return out
}

// TrailingAssistantMonologue counts consecutive assistant messages at the
// tail of the history that carry no real tool_use block (router-synthesized
// nudges don't count as tool activity). The walk stops at the first
// assistant message WITH tool activity or the first user message carrying
// non-tool_result content — i.e. it measures "assistant turns since the last
// real progress or real user input", the OpenHands monologue shape.
func (e *RequestEnvelope) TrailingAssistantMonologue() int {
	if e.format != FormatAnthropic {
		return 0
	}
	msgs := gjson.GetBytes(e.body, "messages")
	if !msgs.IsArray() {
		return 0
	}
	all := msgs.Array()
	streak := 0
	for i := len(all) - 1; i >= 0; i-- {
		msg := all[i]
		switch msg.Get("role").String() {
		case "assistant":
			if assistantHasRealToolUse(msg) {
				return streak
			}
			streak++
		case "user":
			if userHasNonToolResultContent(msg) {
				return streak
			}
		}
	}
	return streak
}

// assistantHasRealToolUse reports whether an assistant message carries at
// least one tool_use block that is not a router-synthesized nudge.
func assistantHasRealToolUse(msg gjson.Result) bool {
	content := msg.Get("content")
	if !content.IsArray() {
		return false
	}
	has := false
	content.ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").String() != "tool_use" {
			return true
		}
		if strings.HasPrefix(block.Get("id").String(), "toolu_router_nudge_") {
			return true
		}
		has = true
		return false
	})
	return has
}

// userHasNonToolResultContent reports whether a user message carries real
// user input (anything other than tool_result blocks). A plain-string user
// message is always real input.
func userHasNonToolResultContent(msg gjson.Result) bool {
	content := msg.Get("content")
	if content.Type == gjson.String {
		return content.String() != ""
	}
	if !content.IsArray() {
		return false
	}
	has := false
	content.ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").String() == "tool_result" {
			return true
		}
		has = true
		return false
	})
	return has
}
