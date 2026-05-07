package translate

import (
	"strings"

	"github.com/tidwall/gjson"
)

const previewMaxChars = 120

// RoutingFeatures bundles router inputs and per-request metadata for logging.
type RoutingFeatures struct {
	Tokens       int
	Model        string
	HasTools     bool
	PromptText   string
	MessageCount int
	// LastKind: "user_prompt", "tool_result", or "assistant". Empty when messages is empty.
	LastKind string
	// LastPreview: first previewMaxChars of the last message's text, newlines collapsed.
	LastPreview string
	// LastUserMessageText: most recent user-authored prompt text (skipping tool_result blocks).
	LastUserMessageText string
}

// RoutingFeatures extracts routing inputs from the envelope. Format-aware:
// dispatches to Anthropic or OpenAI extraction based on source format.
func (e *RequestEnvelope) RoutingFeatures(extractLastUser bool) RoutingFeatures {
	switch e.format {
	case FormatAnthropic:
		return e.anthropicRoutingFeatures(extractLastUser)
	case FormatOpenAI:
		return e.openAIRoutingFeatures()
	case FormatGemini:
		return e.geminiRoutingFeatures()
	default:
		return RoutingFeatures{}
	}
}

func (e *RequestEnvelope) anthropicRoutingFeatures(extractLastUser bool) RoutingFeatures {
	var b strings.Builder
	appendText(&b, systemTextGJSON(gjson.GetBytes(e.body, "system")))

	msgs := gjson.GetBytes(e.body, "messages")
	var (
		msgCount     int
		lastMsg      gjson.Result
		lastUserText string
	)
	msgs.ForEach(func(_, msg gjson.Result) bool {
		msgCount++
		appendText(&b, contentTextGJSON(msg.Get("content")))
		lastMsg = msg
		if extractLastUser && msg.Get("role").String() == "user" {
			if text := userPromptTextGJSON(msg.Get("content")); text != "" {
				lastUserText = text
			}
		}
		return true
	})
	text := b.String()

	feats := RoutingFeatures{
		Tokens:       len(text) / 4,
		Model:        e.Model(),
		HasTools:     e.HasTools(),
		PromptText:   text,
		MessageCount: msgCount,
	}

	if msgCount > 0 {
		feats.LastKind = classifyLastMessageGJSON(lastMsg.Get("role").String(), lastMsg.Get("content"))
		feats.LastPreview = previewGJSON(lastMsg.Get("content"))
	}

	if lastUserText != "" {
		feats.LastUserMessageText = lastUserText
	}

	return feats
}

func (e *RequestEnvelope) openAIRoutingFeatures() RoutingFeatures {
	msgs := gjson.GetBytes(e.body, "messages")

	var b strings.Builder
	var (
		msgCount int
		lastMsg  gjson.Result
	)
	msgs.ForEach(func(_, msg gjson.Result) bool {
		msgCount++
		appendText(&b, openAIContentTextGJSON(msg.Get("content")))
		lastMsg = msg
		return true
	})
	text := b.String()

	feats := RoutingFeatures{
		Tokens:       len(text) / 4,
		Model:        e.Model(),
		HasTools:     e.HasTools(),
		PromptText:   text,
		MessageCount: msgCount,
	}

	if msgCount > 0 {
		feats.LastKind = classifyLastMessageOpenAI(lastMsg.Get("role").String())
		feats.LastPreview = previewText(openAIContentTextGJSON(lastMsg.Get("content")))
	}

	return feats
}

// classifyLastMessageOpenAI maps an OpenAI message role onto the
// shared three-value LastKind enum. OpenAI uses dedicated `tool` and
// `assistant` roles, so unlike Anthropic there is no need to inspect
// content blocks.
func classifyLastMessageOpenAI(role string) string {
	switch role {
	case "assistant":
		return "assistant"
	case "tool":
		return "tool_result"
	default:
		return "user_prompt"
	}
}

// previewText returns the first previewMaxChars of text with newlines
// collapsed to single spaces. Shared by Anthropic and OpenAI feature
// extraction so LastPreview has identical semantics across formats.
func previewText(text string) string {
	if text == "" {
		return ""
	}
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) <= previewMaxChars {
		return text
	}
	return string(runes[:previewMaxChars]) + "…"
}

// anthropicLastUserMessage walks messages backwards for the last role
// "user" entry and reports whether it contains text and/or tool_result
// blocks. The bare-string content shape counts as text.
func anthropicLastUserMessage(body []byte) LastUserMessageInfo {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return LastUserMessageInfo{}
	}
	var lastUser gjson.Result
	msgs.ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() == "user" {
			lastUser = msg
		}
		return true
	})
	if !lastUser.Exists() {
		return LastUserMessageInfo{}
	}
	content := lastUser.Get("content")
	if content.Type == gjson.String {
		s := content.String()
		return LastUserMessageInfo{HasText: s != "", Text: s}
	}
	if !content.IsArray() {
		return LastUserMessageInfo{}
	}
	info := LastUserMessageInfo{}
	var b strings.Builder
	content.ForEach(func(_, block gjson.Result) bool {
		switch block.Get("type").String() {
		case "tool_result":
			info.HasToolResult = true
			info.ToolResultCount++
		case "text":
			text := block.Get("text").String()
			if text == "" {
				return true
			}
			info.HasText = true
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(text)
		}
		return true
	})
	if info.HasText {
		info.Text = b.String()
	}
	return info
}

// openAILastUserMessage scans the trailing run of role=="tool"
// messages and the most recent role=="user" message. OpenAI splits
// tool returns into separate messages, so a tool-result-only turn is
// one or more trailing role=="tool" messages with no role=="user"
// after them.
func openAILastUserMessage(body []byte) LastUserMessageInfo {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return LastUserMessageInfo{}
	}
	all := msgs.Array()
	if len(all) == 0 {
		return LastUserMessageInfo{}
	}
	info := LastUserMessageInfo{}
	for i := len(all) - 1; i >= 0; i-- {
		role := all[i].Get("role").String()
		switch role {
		case "tool":
			info.HasToolResult = true
			info.ToolResultCount++
			continue
		case "user":
			text := openAIContentTextGJSON(all[i].Get("content"))
			if text != "" {
				info.HasText = true
				info.Text = text
			}
		}
		// Stop at the first non-tool message regardless of role.
		break
	}
	return info
}

// openAISystemText concatenates every role=="system" message's text
// content. OpenAI carries the system prompt inline rather than in a
// separate field, and may have multiple system messages (e.g. one
// from the framework, one from the developer).
func openAISystemText(body []byte) string {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return ""
	}
	var b strings.Builder
	msgs.ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() != "system" {
			return true
		}
		text := openAIContentTextGJSON(msg.Get("content"))
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

// --- gjson-based content text helpers (zero allocation from e.body) ---

func systemTextGJSON(v gjson.Result) string {
	if !v.Exists() {
		return ""
	}
	if v.Type == gjson.String {
		return v.String()
	}
	return contentTextGJSON(v)
}

func contentTextGJSON(v gjson.Result) string {
	if !v.Exists() {
		return ""
	}
	if v.Type == gjson.String {
		return v.String()
	}
	if !v.IsArray() {
		return ""
	}
	var b strings.Builder
	v.ForEach(func(_, block gjson.Result) bool {
		text := block.Get("text").String()
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

func classifyLastMessageGJSON(role string, content gjson.Result) string {
	if role == "assistant" {
		return "assistant"
	}
	if !content.IsArray() {
		return "user_prompt"
	}
	hasToolResult := false
	hasText := false
	content.ForEach(func(_, block gjson.Result) bool {
		switch block.Get("type").String() {
		case "tool_result":
			hasToolResult = true
		case "text":
			hasText = true
		}
		return true
	})
	if hasToolResult && !hasText {
		return "tool_result"
	}
	return "user_prompt"
}

func previewGJSON(content gjson.Result) string {
	return previewText(contentTextGJSON(content))
}

func userPromptTextGJSON(content gjson.Result) string {
	if !content.Exists() {
		return ""
	}
	if content.Type == gjson.String {
		return content.String()
	}
	if !content.IsArray() {
		return ""
	}
	var b strings.Builder
	content.ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").String() == "tool_result" {
			return true
		}
		text := block.Get("text").String()
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

// --- OpenAI content text helpers ---

func openAIContentTextGJSON(v gjson.Result) string {
	if !v.Exists() {
		return ""
	}
	if v.Type == gjson.String {
		return v.String()
	}
	if !v.IsArray() {
		return ""
	}
	var b strings.Builder
	v.ForEach(func(_, part gjson.Result) bool {
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

func appendText(b *strings.Builder, s string) {
	if s == "" {
		return
	}
	if b.Len() > 0 {
		b.WriteByte('\n')
	}
	b.WriteString(s)
}
