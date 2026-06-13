package translate

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// openRouterSystemReminder returns a system-message addendum for upstreams whose
// models mishandle agentic tool use, or "" when no reminder is needed.
//
// DeepSeek (V3/V4 family) routinely emits `old_string` values that differ from
// the file by tabs vs spaces, em-dash vs hyphen, or trailing whitespace, then
// falls back to sed/python and corrupts files. Aider pins these models to its
// diff edit format with a sys-level "EXACTLY MATCH" reminder
// (aider/coders/editblock_prompts.py); this is the router equivalent for
// Anthropic-style Edit calls reaching DeepSeek via OpenRouter.
func openRouterSystemReminder(model string) string {
	if strings.HasPrefix(model, "deepseek/") {
		return deepseekToolUseReminder
	}
	return ""
}

// geminiSystemReminder returns a system-message addendum for Gemini 3.x. Empty
// for non-Gemini models or older Gemini families.
//
// Gemini 3.x in agentic-coding traces (SWE-bench Verified) reads source files
// extensively, then ends turns describing the proposed edit in markdown
// instead of calling Edit/Write. The behavior persists even when the request
// carries Edit/Write tool schemas. This addendum nudges it to apply edits via
// the tools when the user asked for a change.
//
// The nudge is deliberately CONDITIONAL. An earlier unconditional phrasing
// ("always call Edit/Write; a turn that only explores counts as giving up")
// framed every explore-only turn as failure, which tipped advisory/read-only
// requests ("what should I fix?") into unsolicited edits, commits, and PRs.
// The conditional form preserves the under-calling fix on genuine edit turns
// while leaving analysis/review requests in prose — mirroring the industry
// norm of mode-scoped guidance (Cline plan/act, Aider ask/code) over a blunt
// global imperative, which research shows over-pushes and is an unreliable
// governor of model action.
func geminiSystemReminder(model string) string {
	if isGemini3xModel(model) {
		return geminiToolUseReminder
	}
	return ""
}

const deepseekToolUseReminder = "When using file-edit tools, copy `old_string` byte-for-byte from the most recent file read — preserve tabs, leading and trailing whitespace, and unicode characters (em-dash —, smart quotes, non-breaking spaces) exactly. If an Edit call fails, re-read the file before retrying. Never fall back to shell commands (sed, awk, python) to modify files."

const geminiToolUseReminder = "When the user has asked you to apply a change, prefer calling the Edit or Write tool over describing the edit in prose or markdown; once you have read enough to know what to change, the next step is the Edit/Write call, not a summary. If the user only asked for analysis, a review, or what could be fixed, answer in prose — do not edit, commit, or push unless they asked you to."

// applySystemReminderToBody injects the reminder into a serialized OpenAI body's
// `messages` array. Best-effort: returns the input unchanged on parse failure
// rather than failing the request.
func applySystemReminderToBody(body []byte, reminder string) ([]byte, error) {
	if reminder == "" {
		return body, nil
	}
	msgsResult := gjson.GetBytes(body, "messages")
	if !msgsResult.IsArray() {
		// No messages array; create one with just the reminder.
		jw := newJSONWriter()
		jw.Arr()
		jw.Obj()
		jw.Key("role")
		jw.Str("system")
		jw.Key("content")
		jw.Str(reminder)
		jw.EndObj()
		jw.EndArr()
		return sjson.SetRawBytes(body, "messages", jw.Bytes())
	}

	// Find the first system message index.
	sysIdx := -1
	msgsResult.ForEach(func(key, value gjson.Result) bool {
		if value.Get("role").String() == "system" {
			sysIdx = int(key.Int())
			return false
		}
		return true
	})

	if sysIdx >= 0 {
		// System message exists — append reminder to its content.
		contentPath := fmt.Sprintf("messages.%d.content", sysIdx)
		contentResult := gjson.GetBytes(body, contentPath)

		if contentResult.IsArray() {
			// Array content: append a text block using sjson append syntax.
			jw := newJSONWriter()
			jw.Obj()
			jw.Key("type")
			jw.Str("text")
			jw.Key("text")
			jw.Str(reminder)
			jw.EndObj()
			return sjson.SetRawBytes(body, contentPath+".-1", jw.Bytes())
		}
		// String content (or other): append with newlines.
		newContent := contentResult.String() + "\n\n" + reminder
		return sjson.SetBytes(body, contentPath, newContent)
	}

	// No system message found — prepend one. Rebuild messages array with
	// the new system message first, then existing messages.
	jw := newJSONWriter()
	jw.Arr()
	jw.Obj()
	jw.Key("role")
	jw.Str("system")
	jw.Key("content")
	jw.Str(reminder)
	jw.EndObj()
	msgsResult.ForEach(func(_, value gjson.Result) bool {
		jw.Raw(value.Raw)
		return true
	})
	jw.EndArr()
	return sjson.SetRawBytes(body, "messages", jw.Bytes())
}
