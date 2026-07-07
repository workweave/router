package translate

import (
	"strings"

	"github.com/tidwall/gjson"
)

// TrailingAssistantTexts returns the visible narration of each assistant
// message since the last real user turn, in order — the concatenation of a
// message's `text` blocks, with `thinking` and `tool_use` ignored and empty
// messages dropped. A user message with any non-tool_result content bounds the
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
			if userHasNonToolResultContent(msg) {
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
