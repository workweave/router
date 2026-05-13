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
	// OnlyUserMessageText: concatenated text from every role=="user" message in
	// order, with system prompt, assistant turns, and tool_result blocks
	// excluded. Populated when the caller passes extractOnlyUser=true.
	OnlyUserMessageText string
}

// RoutingFeatures extracts routing inputs from the envelope. When
// extractOnlyUser is true, OnlyUserMessageText is populated so the caller can
// route on user-typed text only.
func (e *RequestEnvelope) RoutingFeatures(extractOnlyUser bool) RoutingFeatures {
	switch e.format {
	case FormatAnthropic:
		return e.anthropicRoutingFeatures(extractOnlyUser)
	case FormatOpenAI:
		return e.openAIRoutingFeatures(extractOnlyUser)
	case FormatGemini:
		return e.geminiRoutingFeatures(extractOnlyUser)
	default:
		return RoutingFeatures{}
	}
}

func (e *RequestEnvelope) anthropicRoutingFeatures(extractOnlyUser bool) RoutingFeatures {
	var b strings.Builder
	appendText(&b, systemTextGJSON(gjson.GetBytes(e.body, "system")))

	msgs := gjson.GetBytes(e.body, "messages")
	var (
		msgCount  int
		lastMsg   gjson.Result
		onlyUserB strings.Builder
	)
	msgs.ForEach(func(_, msg gjson.Result) bool {
		msgCount++
		appendText(&b, contentTextGJSON(msg.Get("content")))
		lastMsg = msg
		if extractOnlyUser && msg.Get("role").String() == "user" {
			if text := userPromptTextGJSON(msg.Get("content")); text != "" {
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
	}

	if msgCount > 0 {
		feats.LastKind = classifyLastMessageGJSON(lastMsg.Get("role").String(), lastMsg.Get("content"))
		feats.LastPreview = previewGJSON(lastMsg.Get("content"))
	}

	if onlyUserB.Len() > 0 {
		feats.OnlyUserMessageText = onlyUserB.String()
	}

	return feats
}

func (e *RequestEnvelope) openAIRoutingFeatures(extractOnlyUser bool) RoutingFeatures {
	msgs := gjson.GetBytes(e.body, "messages")

	var b strings.Builder
	var (
		msgCount  int
		lastMsg   gjson.Result
		onlyUserB strings.Builder
	)
	msgs.ForEach(func(_, msg gjson.Result) bool {
		msgCount++
		appendText(&b, openAIContentTextGJSON(msg.Get("content")))
		lastMsg = msg
		if extractOnlyUser && msg.Get("role").String() == "user" {
			if text := openAIContentTextGJSON(msg.Get("content")); text != "" {
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
	}

	if msgCount > 0 {
		feats.LastKind = classifyLastMessageOpenAI(lastMsg.Get("role").String())
		feats.LastPreview = previewText(openAIContentTextGJSON(lastMsg.Get("content")))
	}

	if onlyUserB.Len() > 0 {
		feats.OnlyUserMessageText = onlyUserB.String()
	}

	return feats
}

// classifyLastMessageOpenAI maps an OpenAI message role onto the shared
// three-value LastKind enum.
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
// collapsed to spaces.
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

// anthropicLastUserMessage walks messages backwards for the last user entry.
// The bare-string content shape counts as text.
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

// openAILastUserMessage scans the trailing run of role=="tool" messages and
// the most recent role=="user" message. OpenAI splits tool returns into
// separate messages, so a tool-result-only turn is trailing role=="tool"
// messages with no role=="user" after them.
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
		break
	}
	return info
}

// openAISystemText concatenates every role=="system" message's text content.
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
		if isClaudeCodeInjectedBlock(text) {
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

// claudeCodeInjectedBlockPrefixes are wrapper tags Claude Code injects into
// user-message content arrays (system reminders, slash-command echoes, local
// command output). They carry no routing signal and dwarf the user's typed
// text, so we skip them when building the embed input. The full untouched
// request body is still proxied to the upstream model.
var claudeCodeInjectedBlockPrefixes = []string{
	"<system-reminder>",
	"<command-name>",
	"<command-message>",
	"<command-args>",
	"<local-command-stdout>",
	"<local-command-stderr>",
	"<local-command-caveat>",
}

func isClaudeCodeInjectedBlock(text string) bool {
	trimmed := strings.TrimLeft(text, " \t\r\n")
	for _, prefix := range claudeCodeInjectedBlockPrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

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
