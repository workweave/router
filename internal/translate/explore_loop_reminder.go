package translate

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// exploreLoopMinReads is the explore-tool count above which a request that
// hasn't yet seen an Edit/Write/MultiEdit tool_use triggers the explore-loop
// reminder. Set at 10 from the v0.59 SWE-bench Claude Code empty-patch
// audit: bucket-B failures averaged 45+ Read/Grep calls before the model
// gave up; reminding at 10 catches the loop before it cements.
const exploreLoopMinReads = 10

// exploreLoopReminderText is appended to the request's system prompt when the
// detector fires. The wording mirrors geminiSystemReminder's framing but is
// upstream-agnostic — bucket-B failures hit Gemini-3.1-Pro AND Opus-4.8.
const exploreLoopReminderText = "You have spent multiple turns exploring this codebase with Read/Grep/Glob without applying an Edit, Write, or MultiEdit. The task requires modifying code; exploration alone will not solve it. After your next Read or Grep call, your next step MUST be an Edit/Write/MultiEdit call. A turn that explores files without producing an edit counts as the model giving up."

// claudeCodeExploreToolNames is the set of tools the detector counts as
// "explore" actions on Claude Code Anthropic Messages requests. Bash is
// deliberately excluded — Bash can be either explore (ls, grep) or edit
// (echo >file, sed -i) and over-counting would fire the reminder on legit
// repair sessions.
var claudeCodeExploreToolNames = map[string]struct{}{
	"Read": {},
	"Grep": {},
	"Glob": {},
	"LS":   {},
}

// claudeCodeEditToolNames is the set of tools that, if present anywhere in
// the conversation history, suppress the reminder. The model has clearly
// already crossed from exploration into edit, so further Read calls are not
// a stuck-loop signal.
var claudeCodeEditToolNames = map[string]struct{}{
	"Edit":         {},
	"Write":        {},
	"MultiEdit":    {},
	"NotebookEdit": {},
}

// countAnthropicExploreVsEditTools walks an Anthropic Messages request body
// and counts tool_use blocks in assistant-role messages, bucketing into
// "explore" and "edit". Used by applyExploreLoopReminderToAnthropicBody.
func countAnthropicExploreVsEditTools(body []byte) (explores, edits int) {
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return 0, 0
	}
	messages.ForEach(func(_, msg gjson.Result) bool {
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
			if _, ok := claudeCodeExploreToolNames[name]; ok {
				explores++
			} else if _, ok := claudeCodeEditToolNames[name]; ok {
				edits++
			}
			return true
		})
		return true
	})
	return explores, edits
}

// applyExploreLoopReminderToAnthropicBody mutates the request body to append
// exploreLoopReminderText to the `system` field IFF the conversation history
// shows ≥exploreLoopMinReads Read/Grep/Glob/LS tool_use blocks AND zero
// Edit/Write/MultiEdit blocks. Returns (mutatedBody, fired, err). On parse
// failure or when the detector doesn't fire, returns (body, false, nil) so
// callers can apply it unconditionally without paying a re-serialize cost.
//
// Bucket-B fix from the v0.59 SWE-bench Claude Code empty-patch audit:
// 8 of 24 residual empty shards (33%) were Gemini-3.1-Pro and Opus-4.8
// running 45+ Read/Grep calls per session without ever calling Edit/Write,
// then end_turning. This reminder injects after the 10th explore tool,
// nudging the model to switch from research to action.
//
// Handles all three Anthropic `system` shapes: absent (omit, fine), string
// (append with double newline), array of content blocks (append text block).
func applyExploreLoopReminderToAnthropicBody(body []byte) ([]byte, bool, error) {
	explores, edits := countAnthropicExploreVsEditTools(body)
	if explores < exploreLoopMinReads || edits > 0 {
		return body, false, nil
	}
	systemResult := gjson.GetBytes(body, "system")
	if !systemResult.Exists() {
		out, err := sjson.SetBytes(body, "system", exploreLoopReminderText)
		return out, err == nil, err
	}
	if systemResult.Type == gjson.String {
		newContent := systemResult.String() + "\n\n" + exploreLoopReminderText
		out, err := sjson.SetBytes(body, "system", newContent)
		return out, err == nil, err
	}
	if systemResult.IsArray() {
		jw := newJSONWriter()
		jw.Obj()
		jw.Key("type")
		jw.Str("text")
		jw.Key("text")
		jw.Str(exploreLoopReminderText)
		jw.EndObj()
		out, err := sjson.SetRawBytes(body, "system.-1", jw.Bytes())
		return out, err == nil, err
	}
	// Unknown shape (object, number, etc.) — leave body alone rather than
	// corrupt it. The detector having fired with no injection happens is
	// recoverable: the reminder doesn't reach the model this turn but the
	// next turn re-evaluates.
	return body, false, nil
}
