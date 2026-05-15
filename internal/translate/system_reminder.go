package translate

import (
	"encoding/json"
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

const deepseekToolUseReminder = "When using file-edit tools, copy `old_string` byte-for-byte from the most recent file read — preserve tabs, leading and trailing whitespace, and unicode characters (em-dash —, smart quotes, non-breaking spaces) exactly. If an Edit call fails, re-read the file before retrying. Never fall back to shell commands (sed, awk, python) to modify files."

// injectSystemReminder appends `reminder` to an existing system message in
// `messages`, or prepends a new system message when none exists. Returns the
// (possibly new) slice.
func injectSystemReminder(messages []any, reminder string) []any {
	if reminder == "" {
		return messages
	}
	for i, raw := range messages {
		msg, _ := raw.(map[string]any)
		if msg == nil {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "system" {
			continue
		}
		switch content := msg["content"].(type) {
		case string:
			msg["content"] = content + "\n\n" + reminder
		case []any:
			msg["content"] = append(content, map[string]any{
				"type": "text",
				"text": reminder,
			})
		default:
			msg["content"] = reminder
		}
		messages[i] = msg
		return messages
	}
	return append([]any{map[string]any{"role": "system", "content": reminder}}, messages...)
}

// applySystemReminderToBody injects the reminder into a serialized OpenAI body's
// `messages` array. Best-effort: returns the input unchanged on parse failure
// rather than failing the request.
func applySystemReminderToBody(body []byte, reminder string) ([]byte, error) {
	if reminder == "" {
		return body, nil
	}
	msgsResult := gjson.GetBytes(body, "messages")
	if !msgsResult.IsArray() {
		return sjson.SetBytes(body, "messages", []any{
			map[string]any{"role": "system", "content": reminder},
		})
	}
	var msgs []any
	if err := json.Unmarshal([]byte(msgsResult.Raw), &msgs); err != nil {
		return body, nil
	}
	msgs = injectSystemReminder(msgs, reminder)
	return sjson.SetBytes(body, "messages", msgs)
}
