package translate

import (
	"strings"

	"github.com/tidwall/gjson"
)

// TrailingAssistantTexts returns, in order, the narration text of each
// assistant message since the last real human turn — the concatenation of a
// message's `text` blocks, with `thinking` and `tool_use` ignored and empty
// messages dropped. Tool-result turns with CC `<system-reminder>` injections
// are not treated as human boundaries (see userIsHumanTurn). Anthropic only;
// others return nil.
func (e *RequestEnvelope) TrailingAssistantTexts() []string {
	if e.format != FormatAnthropic {
		return nil
	}
	msgs := gjson.GetBytes(e.body, "messages")
	if !msgs.IsArray() {
		return nil
	}
	all := msgs.Array()
	var rev []string
loop:
	for i := len(all) - 1; i >= 0; i-- {
		msg := all[i]
		switch msg.Get("role").String() {
		case "user":
			if userIsHumanTurn(msg) {
				break loop
			}
		case "assistant":
			if s := assistantMessageText(msg); s != "" {
				rev = append(rev, s)
			}
		}
	}
	// Collected tail-first; reverse to chronological order so callers can take
	// the most-recent window off the end.
	for l, r := 0, len(rev)-1; l < r; l, r = l+1, r-1 {
		rev[l], rev[r] = rev[r], rev[l]
	}
	return rev
}

// userIsHumanTurn reports whether a user message carries genuine human input:
// text that is neither a tool_result nor a CC-injected wrapper block.
// Injected turns must not reset the window or the backward scan collects nothing.
func userIsHumanTurn(msg gjson.Result) bool {
	content := msg.Get("content")
	if content.Type == gjson.String {
		return isHumanText(content.String())
	}
	if !content.IsArray() {
		return false
	}
	found := false
	content.ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").String() != "text" {
			return true // tool_result / image / etc. are not human input
		}
		if isHumanText(block.Get("text").String()) {
			found = true
			return false
		}
		return true
	})
	return found
}

// isHumanText reports whether s is real human input rather than empty text or
// one of Claude Code's injected wrapper blocks (<system-reminder>, <command-name>, <local-command-*>, …).
func isHumanText(s string) bool {
	return strings.TrimSpace(s) != "" && !isClaudeCodeInjectedBlock(s)
}

// assistantMessageText joins the text blocks of one assistant message,
// skipping thinking/tool_use. A plain-string content is the text itself.
func assistantMessageText(msg gjson.Result) string {
	content := msg.Get("content")
	if content.Type == gjson.String {
		return content.String()
	}
	if !content.IsArray() {
		return ""
	}
	var b strings.Builder
	content.ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").String() != "text" {
			return true
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(block.Get("text").String())
		return true
	})
	return b.String()
}
