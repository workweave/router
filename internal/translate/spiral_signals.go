package translate

import (
	"strings"

	"github.com/tidwall/gjson"
)

// Spiral-signal extraction: cheap per-request scans of message history feeding
// the proxy's shadow-mode spiral detector (internal/proxy/spiral_detection.go).
// Pure functions of the request body — no cross-turn state needed.

// toolResultErrorScanLimit caps bytes of tool_result content scanned for error
// markers; results can be hundreds of KB but errors show up early.
const toolResultErrorScanLimit = 2048

// toolResultErrorMarkers catch errored tool_results where is_error wasn't set
// (e.g. a test suite that ran fine but returned red). Kept short/high-precision;
// offline audit graded the combined flag+marker signal at AUC 0.73-0.86.
var toolResultErrorMarkers = []string{
	"Traceback (most recent call last)",
	"FAILED",
	"Error:",
	"error:",
}

// ToolResultErrorStats summarizes tool_result error evidence. TrailingErrStreak
// counts consecutive errored results from the end of history; one healthy
// result resets it.
type ToolResultErrorStats struct {
	Total             int
	Errored           int
	TrailingErrStreak int
}

// ToolResultErrors scans user-role tool_result blocks (Anthropic format) for
// error evidence: the is_error flag, or an error marker in the first
// toolResultErrorScanLimit bytes. Returns zero stats for non-Anthropic formats.
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

// toolResultIsErrored checks the is_error flag or an early error marker.
// Content may be a plain string or an array of text blocks.
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

// AssistantToolCallFilePaths returns, in order, every assistant tool_use call
// carrying a file_path or notebook_path arg. Used to detect same-file-thrash.
// Anthropic format only.
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

// TrailingAssistantMonologue counts consecutive tool-less assistant messages
// at the tail of history — "turns since the last real progress or user input"
// (the OpenHands monologue shape). Stops at an assistant tool call (including
// router-synthesized nudges) or a user message with non-tool_result content.
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

// assistantHasRealToolUse reports whether msg carries a tool_use block.
// Router-synthesized nudges count as tool activity too.
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
		has = true
		return false
	})
	return has
}

// userHasNonToolResultContent reports whether msg carries real user input
// (anything besides tool_result blocks).
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
