package translate

import (
	"strings"

	"github.com/tidwall/gjson"
)

// Gemini wire format reference:
//   - systemInstruction: { parts: [ {text: ...} ] }
//   - contents: [ { role: "user"|"model", parts: [...] } ]
//   - tools / generationConfig: top-level
// Streaming choice is encoded in the URL path, not the body.

func (e *RequestEnvelope) geminiRoutingFeatures(extractOnlyUser bool) RoutingFeatures {
	contents := gjson.GetBytes(e.body, "contents")

	var b strings.Builder
	appendText(&b, geminiSystemText(e.body))

	var (
		msgCount  int
		lastMsg   gjson.Result
		onlyUserB strings.Builder
	)
	contents.ForEach(func(_, msg gjson.Result) bool {
		msgCount++
		appendText(&b, geminiPartsText(msg.Get("parts")))
		lastMsg = msg
		if extractOnlyUser && msg.Get("role").String() == "user" {
			if text := geminiPartsText(msg.Get("parts")); text != "" {
				appendText(&onlyUserB, text)
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
		MaxTokens:    intGJSON(gjson.GetBytes(e.body, "generationConfig.maxOutputTokens")),
	}

	if msgCount > 0 {
		feats.LastKind = classifyLastMessageGemini(lastMsg)
		feats.LastPreview = previewText(geminiPartsText(lastMsg.Get("parts")))
	}

	if onlyUserB.Len() > 0 {
		feats.OnlyUserMessageText = onlyUserB.String()
	}

	return feats
}

// classifyLastMessageGemini maps a Gemini contents[] entry to the shared LastKind enum.
func classifyLastMessageGemini(msg gjson.Result) string {
	if msg.Get("role").String() == "model" {
		return "assistant"
	}
	parts := msg.Get("parts")
	if !parts.IsArray() {
		return "user_prompt"
	}
	hasToolResult := false
	hasText := false
	parts.ForEach(func(_, part gjson.Result) bool {
		if part.Get("functionResponse").Exists() {
			hasToolResult = true
		}
		if part.Get("text").Exists() {
			hasText = true
		}
		return true
	})
	if hasToolResult && !hasText {
		return "tool_result"
	}
	return "user_prompt"
}

func geminiSystemText(body []byte) string {
	parts := gjson.GetBytes(body, "systemInstruction.parts")
	if !parts.IsArray() {
		// Some clients send systemInstruction as a bare string or {text}.
		text := gjson.GetBytes(body, "systemInstruction.text")
		if text.Exists() {
			return text.String()
		}
		return ""
	}
	var b strings.Builder
	parts.ForEach(func(_, part gjson.Result) bool {
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

// geminiPartsText concatenates every text-bearing part.
func geminiPartsText(parts gjson.Result) string {
	if !parts.IsArray() {
		return ""
	}
	var b strings.Builder
	parts.ForEach(func(_, part gjson.Result) bool {
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

// geminiLastUserMessage walks contents[] backwards for the last role=="user" entry.
func geminiLastUserMessage(body []byte) LastUserMessageInfo {
	contents := gjson.GetBytes(body, "contents")
	if !contents.IsArray() {
		return LastUserMessageInfo{}
	}
	var lastUser gjson.Result
	contents.ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() == "user" {
			lastUser = msg
		}
		return true
	})
	if !lastUser.Exists() {
		return LastUserMessageInfo{}
	}
	parts := lastUser.Get("parts")
	if !parts.IsArray() {
		return LastUserMessageInfo{}
	}
	info := LastUserMessageInfo{}
	var b strings.Builder
	parts.ForEach(func(_, part gjson.Result) bool {
		if part.Get("functionResponse").Exists() {
			info.HasToolResult = true
			info.ToolResultCount++
			return true
		}
		text := part.Get("text").String()
		if text == "" {
			return true
		}
		info.HasText = true
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(text)
		return true
	})
	if info.HasText {
		info.Text = b.String()
	}
	return info
}

// geminiFirstUserMessageText returns the text of the first role=="user" contents entry.
func geminiFirstUserMessageText(body []byte) string {
	contents := gjson.GetBytes(body, "contents")
	if !contents.IsArray() {
		return ""
	}
	var firstUser gjson.Result
	contents.ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() == "user" {
			firstUser = msg
			return false
		}
		return true
	})
	if !firstUser.Exists() {
		return ""
	}
	return geminiPartsText(firstUser.Get("parts"))
}
