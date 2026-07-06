package translate

import (
	"bytes"
	"strings"

	"workweave/router/internal/observability"

	"github.com/tidwall/gjson"
)

const previewMaxChars = 120

// contentBytesPerToken is the measured bytes/token ratio (~4.2) for dense
// Claude Code request bodies, used by ContextOverflowTokenEstimate. Lower
// than FullTokenEstimate's ÷6 because that divisor is tuned to avoid
// over-counting base64 thought-signatures; here we subtract signature bytes
// explicitly instead, so the rest can divide at its true ratio.
const contentBytesPerToken = 4

// signatureFieldMarker precedes a base64 thought-signature payload in an
// Anthropic request body.
var signatureFieldMarker = []byte(`"signature":"`)

// RoutingFeatures bundles router inputs and per-request metadata for logging.
type RoutingFeatures struct {
	Tokens       int
	Model        string
	HasTools     bool
	HasImages    bool
	PromptText   string
	MessageCount int
	// MaxTokens is the caller's requested output cap. 0 means unset.
	// Probe detection keys off this — Anthropic SDK quota probes set max_tokens=1.
	MaxTokens int
	// LastKind: "user_prompt", "tool_result", or "assistant".
	LastKind string
	// LastPreview: first previewMaxChars of the last message's text.
	LastPreview string
	// OnlyUserMessageText: user-message text with system/assistant/tool_result excluded.
	OnlyUserMessageText string
}

// FullTokenEstimate estimates tokens for the whole request body — including
// tool defs/calls/results that RoutingFeatures.Tokens (text-only) misses.
// Used only for context-window pre-filtering, not routing.
func (e *RequestEnvelope) FullTokenEstimate() int {
	// ÷6, not ÷4: base64 thought signatures otherwise inflate byte length and
	// falsely evict Opus for exceeding its context window.
	return len(e.body) / 6
}

// ContextOverflowTokenEstimate estimates tokens for context-window overflow
// pre-filtering, for a signature-KEEPING (Anthropic-family) target. Uses
// contentBytesPerToken rather than FullTokenEstimate's ÷6: ÷6 undercounted a
// genuinely large, signature-light ~263K-token body to ~175K and 400'd on a
// 256K OSS model. FullTokenEstimate stays ÷6 for its own (extended-context
// beta) calibration. Signature-STRIPPING targets subtract
// SignatureTokenSavings from this — see excludeContextOverflowModels.
func (e *RequestEnvelope) ContextOverflowTokenEstimate() int {
	return len(e.body) / contentBytesPerToken
}

// SignatureTokenSavings returns the tokens a signature-STRIPPING target saves
// vs ContextOverflowTokenEstimate, since we drop thought-signature blocks
// before dispatch to non-Anthropic models. Zero for non-Anthropic inbound
// formats: a stray "signature" field there is caller data, not ours to strip.
func (e *RequestEnvelope) SignatureTokenSavings() int {
	if e.format != FormatAnthropic {
		return 0
	}
	return base64SignatureBytes(e.body) / contentBytesPerToken
}

// base64SignatureBytes sums the byte length of every base64 thought-signature
// payload in body. Signatures contain no quotes/backslashes, so each payload
// runs from its field marker to the next double quote.
func base64SignatureBytes(body []byte) int {
	total := 0
	for i := 0; ; {
		rel := bytes.Index(body[i:], signatureFieldMarker)
		if rel < 0 {
			break
		}
		start := i + rel + len(signatureFieldMarker)
		end := bytes.IndexByte(body[start:], '"')
		if end < 0 {
			break
		}
		total += end
		i = start + end + 1
	}
	return total
}

// RoutingFeatures extracts routing inputs from the envelope. When
// extractOnlyUser is true, OnlyUserMessageText is populated.
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
		HasImages:    e.HasImages(),
		PromptText:   text,
		MessageCount: msgCount,
		MaxTokens:    intGJSON(gjson.GetBytes(e.body, "max_tokens")),
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
		HasImages:    e.HasImages(),
		PromptText:   text,
		MessageCount: msgCount,
		MaxTokens:    openAIMaxTokens(e.body),
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

// intGJSON returns the integer value of a JSON number, or 0 when absent/non-numeric.
// Fractional values (invalid for token caps) are treated as unset.
func intGJSON(v gjson.Result) int {
	if !v.Exists() || v.Type != gjson.Number {
		return 0
	}
	n := v.Int()
	if n < 0 {
		return 0
	}
	return int(n)
}

// openAIMaxTokens reads the output token cap from an OpenAI body.
// Reads max_completion_tokens first, falling back to max_tokens.
func openAIMaxTokens(body []byte) int {
	if n := intGJSON(gjson.GetBytes(body, "max_completion_tokens")); n > 0 {
		return n
	}
	return intGJSON(gjson.GetBytes(body, "max_tokens"))
}

// classifyLastMessageOpenAI maps an OpenAI message role to the shared LastKind enum.
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

// previewText collapses newlines to spaces, then truncates to previewMaxChars.
func previewText(text string) string {
	if text == "" {
		return ""
	}
	return observability.Preview(strings.Join(strings.Fields(text), " "), previewMaxChars)
}

// anthropicLastUserMessage walks messages backwards for the last user entry.
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
			info.ToolResultBytes += len(block.Get("content").Raw)
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

// openAILastUserMessage scans the trailing run of role=="tool" messages and the
// most recent role=="user" message.
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
			info.ToolResultBytes += len(all[i].Get("content").Raw)
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

// classifyLastMessageGJSON maps an Anthropic message to the shared LastKind
// enum. Any user message with a tool_result block classifies as "tool_result"
// even alongside other text (e.g. Claude Code's <system-reminder> blocks) —
// switching models mid tool-flow would hand the new model a tool_result meant
// for the old model's tool_use, so these must stay pinned to the session.
func classifyLastMessageGJSON(role string, content gjson.Result) string {
	if role == "assistant" {
		return "assistant"
	}
	if !content.IsArray() {
		return "user_prompt"
	}
	hasToolResult := false
	content.ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").String() == "tool_result" {
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
// user messages; skipped since they dwarf the user's typed text and add no signal.
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

// anthropicHasImages reports whether any content block is
// {"type":"image",...}. String content never has an image.
func anthropicHasImages(body []byte) bool {
	found := false
	gjson.GetBytes(body, "messages").ForEach(func(_, msg gjson.Result) bool {
		content := msg.Get("content")
		if !content.IsArray() {
			return true
		}
		content.ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() == "image" {
				found = true
				return false
			}
			return true
		})
		return !found
	})
	return found
}

// openAIHasImages reports whether any content part is
// {"type":"image_url",...}. String content never has an image.
func openAIHasImages(body []byte) bool {
	found := false
	gjson.GetBytes(body, "messages").ForEach(func(_, msg gjson.Result) bool {
		content := msg.Get("content")
		if !content.IsArray() {
			return true
		}
		content.ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").String() == "image_url" {
				found = true
				return false
			}
			return true
		})
		return !found
	})
	return found
}
