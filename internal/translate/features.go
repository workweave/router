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
	msgCount := 0
	msgs.ForEach(func(_, msg gjson.Result) bool {
		msgCount++
		appendText(&b, openAIContentTextGJSON(msg.Get("content")))
		return true
	})
	text := b.String()

	return RoutingFeatures{
		Tokens:       len(text) / 4,
		Model:        e.Model(),
		HasTools:     e.HasTools(),
		PromptText:   text,
		MessageCount: msgCount,
	}
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
	text := contentTextGJSON(content)
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
