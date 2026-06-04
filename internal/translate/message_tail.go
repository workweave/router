package translate

import (
	"strings"

	"github.com/tidwall/gjson"
)

// MessageBlockPreview is a one-block snapshot of a request-side message,
// suitable for log diagnostics. Type is the block's wire type ("text",
// "tool_use", "tool_result", "thinking", etc.). Name carries the tool name
// for tool_use and the tool_use_id for tool_result; empty otherwise.
// Preview is a short excerpt of the block's salient payload.
type MessageBlockPreview struct {
	Type    string
	Name    string
	Preview string
}

// MessagePreview is a role-tagged window into one request message.
type MessagePreview struct {
	Role   string
	Blocks []MessageBlockPreview
}

// MessageTailPreview returns the last n messages with each block summarized
// for log output.
func (e *RequestEnvelope) MessageTailPreview(n, maxLen int) []MessagePreview {
	if n <= 0 || maxLen <= 0 {
		return nil
	}
	switch e.format {
	case FormatAnthropic:
		return anthropicMessageTailPreview(e.body, n, maxLen)
	case FormatOpenAI:
		return openAIMessageTailPreview(e.body, n, maxLen)
	case FormatGemini:
		return geminiMessageTailPreview(e.body, n, maxLen)
	default:
		return nil
	}
}

func anthropicMessageTailPreview(body []byte, n, maxLen int) []MessagePreview {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return nil
	}
	all := msgs.Array()
	if len(all) == 0 {
		return nil
	}
	start := len(all) - n
	if start < 0 {
		start = 0
	}
	out := make([]MessagePreview, 0, len(all)-start)
	for _, msg := range all[start:] {
		mp := MessagePreview{Role: msg.Get("role").String()}
		content := msg.Get("content")
		switch {
		case content.Type == gjson.String:
			mp.Blocks = append(mp.Blocks, MessageBlockPreview{
				Type:    "text",
				Preview: truncatePreview(content.String(), maxLen),
			})
		case content.IsArray():
			content.ForEach(func(_, block gjson.Result) bool {
				mp.Blocks = append(mp.Blocks, anthropicBlockPreview(block, maxLen))
				return true
			})
		}
		out = append(out, mp)
	}
	return out
}

func anthropicBlockPreview(block gjson.Result, maxLen int) MessageBlockPreview {
	t := block.Get("type").String()
	switch t {
	case "text":
		return MessageBlockPreview{Type: t, Preview: truncatePreview(block.Get("text").String(), maxLen)}
	case "tool_use":
		return MessageBlockPreview{
			Type:    t,
			Name:    truncatePreview(block.Get("name").String(), blockNameMaxLen),
			Preview: truncatePreview(block.Get("input").Raw, maxLen),
		}
	case "tool_result":
		return MessageBlockPreview{
			Type:    t,
			Name:    truncatePreview(block.Get("tool_use_id").String(), blockNameMaxLen),
			Preview: truncatePreview(toolResultContentText(block.Get("content")), maxLen),
		}
	default:
		// thinking / redacted_thinking / image / etc — record presence only.
		return MessageBlockPreview{Type: t}
	}
}

func toolResultContentText(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.String()
	}
	if !content.IsArray() {
		return ""
	}
	var b strings.Builder
	content.ForEach(func(_, part gjson.Result) bool {
		if part.Get("type").String() != "text" {
			return true
		}
		text := part.Get("text").String()
		if text == "" {
			return true
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(text)
		return true
	})
	return b.String()
}

func openAIMessageTailPreview(body []byte, n, maxLen int) []MessagePreview {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return nil
	}
	all := msgs.Array()
	if len(all) == 0 {
		return nil
	}
	start := len(all) - n
	if start < 0 {
		start = 0
	}
	out := make([]MessagePreview, 0, len(all)-start)
	for _, msg := range all[start:] {
		role := msg.Get("role").String()
		mp := MessagePreview{Role: role}
		if text := openAIContentTextGJSON(msg.Get("content")); text != "" {
			mp.Blocks = append(mp.Blocks, MessageBlockPreview{
				Type:    "text",
				Preview: truncatePreview(text, maxLen),
			})
		}
		switch role {
		case "assistant":
			toolCalls := msg.Get("tool_calls")
			if toolCalls.IsArray() {
				toolCalls.ForEach(func(_, tc gjson.Result) bool {
					mp.Blocks = append(mp.Blocks, MessageBlockPreview{
						Type:    "tool_use",
						Name:    truncatePreview(tc.Get("function.name").String(), blockNameMaxLen),
						Preview: truncatePreview(tc.Get("function.arguments").String(), maxLen),
					})
					return true
				})
			}
		case "tool":
			// Surface as tool_result for symmetry with the Anthropic shape.
			id := truncatePreview(msg.Get("tool_call_id").String(), blockNameMaxLen)
			if len(mp.Blocks) > 0 {
				mp.Blocks[0].Type = "tool_result"
				mp.Blocks[0].Name = id
			} else if id != "" {
				mp.Blocks = append(mp.Blocks, MessageBlockPreview{
					Type: "tool_result",
					Name: id,
				})
			}
		}
		out = append(out, mp)
	}
	return out
}

func geminiMessageTailPreview(body []byte, n, maxLen int) []MessagePreview {
	contents := gjson.GetBytes(body, "contents")
	if !contents.IsArray() {
		return nil
	}
	all := contents.Array()
	if len(all) == 0 {
		return nil
	}
	start := len(all) - n
	if start < 0 {
		start = 0
	}
	out := make([]MessagePreview, 0, len(all)-start)
	for _, msg := range all[start:] {
		mp := MessagePreview{Role: msg.Get("role").String()}
		parts := msg.Get("parts")
		if parts.IsArray() {
			parts.ForEach(func(_, part gjson.Result) bool {
				mp.Blocks = append(mp.Blocks, geminiPartPreview(part, maxLen))
				return true
			})
		}
		out = append(out, mp)
	}
	return out
}

func geminiPartPreview(part gjson.Result, maxLen int) MessageBlockPreview {
	if text := part.Get("text").String(); text != "" {
		if part.Get("thought").Bool() {
			return MessageBlockPreview{Type: "thinking", Preview: truncatePreview(text, maxLen)}
		}
		return MessageBlockPreview{Type: "text", Preview: truncatePreview(text, maxLen)}
	}
	if fc := part.Get("functionCall"); fc.Exists() {
		return MessageBlockPreview{
			Type:    "tool_use",
			Name:    truncatePreview(fc.Get("name").String(), blockNameMaxLen),
			Preview: truncatePreview(fc.Get("args").Raw, maxLen),
		}
	}
	if fr := part.Get("functionResponse"); fr.Exists() {
		return MessageBlockPreview{
			Type:    "tool_result",
			Name:    truncatePreview(fr.Get("name").String(), blockNameMaxLen),
			Preview: truncatePreview(fr.Get("response").Raw, maxLen),
		}
	}
	if inlineData := part.Get("inlineData"); inlineData.Exists() {
		return MessageBlockPreview{Type: "image", Name: truncatePreview(inlineData.Get("mimeType").String(), blockNameMaxLen)}
	}
	if fileData := part.Get("fileData"); fileData.Exists() {
		return MessageBlockPreview{Type: "file", Name: truncatePreview(fileData.Get("mimeType").String(), blockNameMaxLen)}
	}
	return MessageBlockPreview{Type: "part"}
}

// SystemTextTail returns the system-prompt length plus head and tail
// excerpts of up to maxLen bytes each. Tail is empty when the system text
// fits inside maxLen. Lets log readers spot transient <system-reminder>
// injections without dumping the static prompt body.
func (e *RequestEnvelope) SystemTextTail(maxLen int) (length int, head, tail string) {
	s := e.SystemText()
	length = len(s)
	if length == 0 || maxLen <= 0 {
		return length, "", ""
	}
	if length <= maxLen {
		return length, s, ""
	}
	return length, s[:maxLen], s[length-maxLen:]
}

// blockNameMaxLen caps tool names and tool_use_ids in the conversation tail
// log. The Anthropic→OpenAI translator packs thoughtSignatures into
// tool_use_ids and the resulting base64 string can exceed 4 KB, dwarfing
// every other block in the log line.
const blockNameMaxLen = 48

func truncatePreview(s string, maxLen int) string {
	if maxLen <= 0 || len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}
