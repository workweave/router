package translate

import (
	"strings"

	"github.com/tidwall/gjson"
)

// TrailingAssistantTexts returns the visible narration of each assistant
// message since the last real human turn, in order — the concatenation of a
// message's `text` blocks, with `thinking` and `tool_use` ignored and empty
// messages dropped. A genuine human user turn (see userIsHumanTurn) bounds the
// run: repetition before it belongs to a prior task, so a re-route after a
// break starts from a clean window. Anthropic format only; others return nil.
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
// text that is neither a tool_result nor one of Claude Code's injected
// <system-reminder> blocks. CC appends those reminders on the same user turn as
// tool_result payloads every iteration, so treating any non-tool_result content
// as a boundary (as userHasNonToolResultContent does) would stop the backward
// scan on exactly the tool-result turns this detector guards, collecting no
// narration. Only real human text resets the repetition window.
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

// isHumanText reports whether s is real human input rather than empty text or a
// Claude Code injected <system-reminder> block.
func isHumanText(s string) bool {
	s = strings.TrimSpace(s)
	return s != "" && !strings.HasPrefix(s, "<system-reminder")
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
