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
		HasImages:    e.HasImages(),
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

// classifyLastMessageGemini maps a Gemini contents[] entry to the shared
// LastKind enum. Mirrors the Anthropic classifier: any functionResponse part
// makes the turn a tool-flow continuation regardless of accompanying text.
func classifyLastMessageGemini(msg gjson.Result) string {
	if msg.Get("role").String() == "model" {
		return "assistant"
	}
	parts := msg.Get("parts")
	if !parts.IsArray() {
		return "user_prompt"
	}
	hasToolResult := false
	parts.ForEach(func(_, part gjson.Result) bool {
		if part.Get("functionResponse").Exists() {
			hasToolResult = true
			return false
		}
		return true
	})
	if hasToolResult {
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

// geminiHasImages reports whether any contents[] part carries inline or
// referenced media. Gemini sends non-text media as inlineData (base64 blob) or
// fileData (Files API URI); both camelCase (REST) and snake_case (some SDKs)
// spellings appear in the wild. Any such part means the turn needs a multimodal
// model.
func geminiHasImages(body []byte) bool {
	found := false
	gjson.GetBytes(body, "contents").ForEach(func(_, content gjson.Result) bool {
		content.Get("parts").ForEach(func(_, part gjson.Result) bool {
			if part.Get("inlineData").Exists() || part.Get("inline_data").Exists() ||
				part.Get("fileData").Exists() || part.Get("file_data").Exists() {
				found = true
				return false
			}
			return true
		})
		return !found
	})
	return found
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
