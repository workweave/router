package translate

import (
	"strings"

	"github.com/tidwall/gjson"
)

// AssistantTextMessages returns, in order, the visible narration text of each
// assistant message — the concatenation of its `text` blocks, with `thinking`
// and `tool_use` blocks ignored. Messages with no text contribute an empty
// string so callers can reason about position if they need to; empties are
// skipped here to keep the slice to real narration. Anthropic format only
// (Claude Code's wire format); other formats return nil.
//
// Feeds the proxy's enforcing text-repetition detector
// (internal/proxy/text_repetition.go): a model can defeat the tool-call and
// no-progress detectors by issuing a fresh tool call every turn while
// repeating the same narration verbatim, and that repeated text is the only
// durable tell. Pure function of the request body — the full history arrives
// on every turn, so no cross-turn state is needed.
func (e *RequestEnvelope) AssistantTextMessages() []string {
	if e.format != FormatAnthropic {
		return nil
	}
	msgs := gjson.GetBytes(e.body, "messages")
	if !msgs.IsArray() {
		return nil
	}
	var out []string
	msgs.ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() != "assistant" {
			return true
		}
		content := msg.Get("content")
		// A plain-string assistant message is itself the text.
		if content.Type == gjson.String {
			if s := content.String(); s != "" {
				out = append(out, s)
			}
			return true
		}
		if !content.IsArray() {
			return true
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
		if b.Len() > 0 {
			out = append(out, b.String())
		}
		return true
	})
	return out
}
