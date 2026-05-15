package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"workweave/router/internal/router"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// INVARIANTS:
//   - e.body is immutable after parse. Same-format emit uses gjson/sjson overrides
//     on e.body with no cloning. Cross-format emit unmarshals into e.src once.
//   - Field accessors use gjson directly on e.body.

// ErrNotJSONObject is returned when the request body is not a valid JSON object.
var ErrNotJSONObject = errors.New("request body must be a JSON object")

type Format int

const (
	FormatOpenAI Format = iota
	FormatAnthropic
	FormatGemini
)

// EmitOptions parameterizes output-body construction.
type EmitOptions struct {
	TargetModel        string
	Capabilities       router.ModelSpec
	IncludeStreamUsage bool
}

// RequestEnvelope wraps a parsed request body regardless of wire format.
// Use Prepare* to emit target-format bytes; accessors read fields.
type RequestEnvelope struct {
	body   []byte
	src    map[string]any // lazily populated for cross-format emit
	format Format
}

// ParseOpenAI validates body as a JSON object and wraps it in a RequestEnvelope.
func ParseOpenAI(body []byte) (*RequestEnvelope, error) {
	if err := validateJSONObject(body); err != nil {
		return nil, err
	}
	return &RequestEnvelope{body: body, format: FormatOpenAI}, nil
}

// ParseAnthropic validates body as a JSON object and wraps it in a RequestEnvelope.
func ParseAnthropic(body []byte) (*RequestEnvelope, error) {
	if err := validateJSONObject(body); err != nil {
		return nil, err
	}
	return &RequestEnvelope{body: body, format: FormatAnthropic}, nil
}

// ParseGemini validates body as a JSON object and wraps it in a RequestEnvelope
// sourced from Gemini's native generateContent shape.
func ParseGemini(body []byte) (*RequestEnvelope, error) {
	if err := validateJSONObject(body); err != nil {
		return nil, err
	}
	return &RequestEnvelope{body: body, format: FormatGemini}, nil
}

// validateJSONObject rejects arrays, scalars, and null.
func validateJSONObject(body []byte) error {
	if !gjson.ValidBytes(body) {
		return fmt.Errorf("%w: invalid JSON", ErrNotJSONObject)
	}
	if !gjson.ParseBytes(body).IsObject() {
		return fmt.Errorf("%w: body is not a JSON object", ErrNotJSONObject)
	}
	return nil
}

// ensureSrc lazily unmarshals e.body into e.src on first call.
func (e *RequestEnvelope) ensureSrc() (map[string]any, error) {
	if e.src == nil {
		if err := json.Unmarshal(e.body, &e.src); err != nil {
			return nil, fmt.Errorf("translate: unmarshal request body: %w", err)
		}
	}
	return e.src, nil
}

func (e *RequestEnvelope) SourceFormat() Format { return e.format }

// Stream reports whether the request has "stream": true. Rejects numeric coercion.
// For Gemini ingress, the handler injects a synthetic "stream": true.
func (e *RequestEnvelope) Stream() bool {
	r := gjson.GetBytes(e.body, "stream")
	if r.Type == gjson.Number {
		return false
	}
	return r.Bool()
}

// Model returns the requested model name.
func (e *RequestEnvelope) Model() string {
	return gjson.GetBytes(e.body, "model").String()
}

// MetadataUserID returns the raw metadata.user_id string.
func (e *RequestEnvelope) MetadataUserID() string {
	return gjson.GetBytes(e.body, "metadata.user_id").String()
}

// SystemText returns the concatenated system-prompt text format-neutrally.
func (e *RequestEnvelope) SystemText() string {
	switch e.format {
	case FormatAnthropic:
		return systemTextGJSON(gjson.GetBytes(e.body, "system"))
	case FormatOpenAI:
		return openAISystemText(e.body)
	case FormatGemini:
		return geminiSystemText(e.body)
	default:
		return ""
	}
}

// LastUserMessageInfo summarizes the trailing user-side input.
type LastUserMessageInfo struct {
	HasText         bool
	HasToolResult   bool
	ToolResultCount int
	Text            string
}

// LastUserMessage returns format-neutral information about the last user input.
func (e *RequestEnvelope) LastUserMessage() LastUserMessageInfo {
	switch e.format {
	case FormatAnthropic:
		return anthropicLastUserMessage(e.body)
	case FormatOpenAI:
		return openAILastUserMessage(e.body)
	case FormatGemini:
		return geminiLastUserMessage(e.body)
	default:
		return LastUserMessageInfo{}
	}
}

// FirstUserMessageText returns the text of the first user-authored message.
// Returns "" if there is no first user message.
func (e *RequestEnvelope) FirstUserMessageText() string {
	if e.format == FormatGemini {
		return geminiFirstUserMessageText(e.body)
	}
	first := gjson.GetBytes(e.body, "messages.0")
	if !first.Exists() {
		return ""
	}
	if first.Get("role").String() != "user" {
		return ""
	}
	content := first.Get("content")
	switch e.format {
	case FormatAnthropic:
		return userPromptTextGJSON(content)
	case FormatOpenAI:
		return openAIContentTextGJSON(content)
	default:
		return ""
	}
}

// HasTools reports whether the request has a non-empty tools array.
func (e *RequestEnvelope) HasTools() bool {
	r := gjson.GetBytes(e.body, "tools.#")
	return r.Int() > 0
}

// RequestsTitleSchema reports whether the request asks for a JSON-schema response
// with a top-level string "title" property. Used to identify Claude Code's
// sidebar-title generation call without content-matching the system prompt.
func (e *RequestEnvelope) RequestsTitleSchema() bool {
	switch e.format {
	case FormatAnthropic:
		fmtNode := gjson.GetBytes(e.body, "output_config.format")
		if fmtNode.Get("type").String() != "json_schema" {
			return false
		}
		return fmtNode.Get("schema.properties.title.type").String() == "string"
	case FormatOpenAI:
		rf := gjson.GetBytes(e.body, "response_format")
		if rf.Get("type").String() != "json_schema" {
			return false
		}
		return rf.Get("json_schema.schema.properties.title.type").String() == "string"
	default:
		return false
	}
}

// EmitOverrides describes byte-level mutations for same-format serialization.
// Zero-valued fields are no-ops.
type EmitOverrides struct {
	Model         string
	DeleteKeys    []string
	SetMaxCompletionTokens *int64
	ClampMaxTokensKey       string
	ClampMaxTokensValue     int64
	ClampMaxCompTokensValue int64
	DefaultMaxTokensKey   string
	DefaultMaxTokensValue int64
	InjectStreamUsage   bool
	StripThinkingBlocks bool
}

func (e *RequestEnvelope) emitSameFormat(ov EmitOverrides) ([]byte, error) {
	return applyOverrides(e.body, ov)
}

// applyOverrides applies mutations in order: structural, field overrides, deletions.
func applyOverrides(body []byte, ov EmitOverrides) ([]byte, error) {
	var err error
	out := body

	if ov.StripThinkingBlocks {
		out, err = stripThinkingBlocksBytes(out)
		if err != nil {
			return nil, fmt.Errorf("strip thinking blocks: %w", err)
		}
	}

	out, err = sjson.SetBytes(out, "model", ov.Model)
	if err != nil {
		return nil, fmt.Errorf("set model: %w", err)
	}

	if ov.SetMaxCompletionTokens != nil {
		out, err = sjson.SetBytes(out, "max_completion_tokens", *ov.SetMaxCompletionTokens)
		if err != nil {
			return nil, fmt.Errorf("set max_completion_tokens: %w", err)
		}
	}

	if ov.ClampMaxTokensKey != "" && ov.ClampMaxTokensValue > 0 {
		out = clampFieldBytes(out, ov.ClampMaxTokensKey, ov.ClampMaxTokensValue)
	}
	if ov.ClampMaxCompTokensValue > 0 {
		out = clampFieldBytes(out, "max_completion_tokens", ov.ClampMaxCompTokensValue)
	}

	if ov.DefaultMaxTokensKey != "" && ov.DefaultMaxTokensValue > 0 {
		if !gjson.GetBytes(out, ov.DefaultMaxTokensKey).Exists() {
			out, err = sjson.SetBytes(out, ov.DefaultMaxTokensKey, ov.DefaultMaxTokensValue)
			if err != nil {
				return nil, fmt.Errorf("set default %s: %w", ov.DefaultMaxTokensKey, err)
			}
		}
	}

	if ov.InjectStreamUsage {
		if gjson.GetBytes(out, "stream").Bool() {
			out, err = sjson.SetBytes(out, "stream_options.include_usage", true)
			if err != nil {
				return nil, fmt.Errorf("set stream_options.include_usage: %w", err)
			}
		}
	}

	for _, key := range ov.DeleteKeys {
		out, err = sjson.DeleteBytes(out, key)
		if err != nil {
			return nil, fmt.Errorf("delete %s: %w", key, err)
		}
	}

	return out, nil
}

// clampFieldBytes caps a numeric JSON field to maxVal. No-op if absent or non-numeric.
func clampFieldBytes(body []byte, key string, maxVal int64) []byte {
	r := gjson.GetBytes(body, key)
	if !r.Exists() || r.Type != gjson.Number {
		return body
	}
	if r.Num <= float64(maxVal) {
		return body
	}
	out, err := sjson.SetBytes(body, key, maxVal)
	if err != nil {
		return body
	}
	return out
}

// stripThinkingBlocksBytes removes thinking/redacted_thinking blocks from
// messages[*].content[*]. Uses two-phase reconstruction for O(B) work.
func stripThinkingBlocksBytes(body []byte) ([]byte, error) {
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return body, nil
	}

	anyChanged := false
	var msgRaws []string
	var walkErr error

	messages.ForEach(func(_, msg gjson.Result) bool {
		content := msg.Get("content")
		if !content.Exists() || !content.IsArray() {
			msgRaws = append(msgRaws, msg.Raw)
			return true
		}

		hasThinking := false
		content.ForEach(func(_, block gjson.Result) bool {
			blockType := block.Get("type").String()
			if blockType == "thinking" || blockType == "redacted_thinking" {
				hasThinking = true
				return false
			}
			return true
		})

		if !hasThinking {
			msgRaws = append(msgRaws, msg.Raw)
			return true
		}

		anyChanged = true
		var kept []string
		content.ForEach(func(_, block gjson.Result) bool {
			blockType := block.Get("type").String()
			if blockType != "thinking" && blockType != "redacted_thinking" {
				kept = append(kept, block.Raw)
			}
			return true
		})

		filteredContent := "[" + strings.Join(kept, ",") + "]"

		newMsg, err := sjson.SetRaw(msg.Raw, "content", filteredContent)
		if err != nil {
			walkErr = fmt.Errorf("replace content in message: %w", err)
			return false
		}
		msgRaws = append(msgRaws, newMsg)
		return true
	})

	if walkErr != nil {
		return nil, walkErr
	}
	if !anyChanged {
		return body, nil
	}

	newMessagesArray := "[" + strings.Join(msgRaws, ",") + "]"
	return sjson.SetRawBytes(body, "messages", []byte(newMessagesArray))
}

// encodeJSONStringNoHTMLEscape marshals s without HTML-escaping <, >, or &.
// Preserves the client's original escaping for upstream prompt-cache keys.
func encodeJSONStringNoHTMLEscape(s string) ([]byte, error) {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	out := buf.String()
	// json.Encoder appends a trailing newline; strip it for sjson.
	return []byte(strings.TrimSuffix(out, "\n")), nil
}

// routingMarkerPattern matches the "✦ **Weave Router** → ..." snippet
// injected in cross-format responses.
var routingMarkerPattern = regexp.MustCompile(`✦ \*\*Weave Router\*\* → [^\n]*\n\n`)

// StripRoutingMarkerFromMessages removes the routing-marker snippet from every
// text block in messages[*].content[*]. Stripping on ingress keeps it out of
// upstream context and stabilizes assistant prefixes for prompt-cache reuse.
func StripRoutingMarkerFromMessages(body []byte) ([]byte, error) {
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return body, nil
	}

	anyChanged := false
	var msgRaws []string
	var walkErr error

	messages.ForEach(func(_, msg gjson.Result) bool {
		content := msg.Get("content")
		if !content.Exists() || !content.IsArray() {
			msgRaws = append(msgRaws, msg.Raw)
			return true
		}

		var newBlocks []string
		msgChanged := false
		content.ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() != "text" {
				newBlocks = append(newBlocks, block.Raw)
				return true
			}
			text := block.Get("text").String()
			if !routingMarkerPattern.MatchString(text) {
				newBlocks = append(newBlocks, block.Raw)
				return true
			}
			stripped := routingMarkerPattern.ReplaceAllString(text, "")
			msgChanged = true
			if strings.TrimSpace(stripped) == "" {
				return true
			}
			encoded, err := encodeJSONStringNoHTMLEscape(stripped)
			if err != nil {
				walkErr = fmt.Errorf("marshal stripped text: %w", err)
				return false
			}
			newBlock, err := sjson.SetRawBytes([]byte(block.Raw), "text", encoded)
			if err != nil {
				walkErr = fmt.Errorf("replace text in block: %w", err)
				return false
			}
			newBlocks = append(newBlocks, string(newBlock))
			return true
		})
		if walkErr != nil {
			return false
		}
		if !msgChanged {
			msgRaws = append(msgRaws, msg.Raw)
			return true
		}

		anyChanged = true
		newContent := "[" + strings.Join(newBlocks, ",") + "]"
		newMsg, err := sjson.SetRawBytes([]byte(msg.Raw), "content", []byte(newContent))
		if err != nil {
			walkErr = fmt.Errorf("replace content in message: %w", err)
			return false
		}
		msgRaws = append(msgRaws, string(newMsg))
		return true
	})

	if walkErr != nil {
		return nil, walkErr
	}
	if !anyChanged {
		return body, nil
	}

	newMessagesArray := "[" + strings.Join(msgRaws, ",") + "]"
	return sjson.SetRawBytes(body, "messages", []byte(newMessagesArray))
}

func resolveOpenAIOverrides(body []byte, opts EmitOptions) EmitOverrides {
	ov := EmitOverrides{
		Model: opts.TargetModel,
	}

	ov.DeleteKeys = append(ov.DeleteKeys, "thinking")

	if gjson.GetBytes(body, "reasoning_effort").Exists() && !opts.Capabilities.Supports(router.CapReasoning) {
		ov.DeleteKeys = append(ov.DeleteKeys, "reasoning_effort")
	}

	hasMaxTokens := gjson.GetBytes(body, "max_tokens").Exists()
	hasMaxComp := gjson.GetBytes(body, "max_completion_tokens").Exists()
	supportsReasoning := opts.Capabilities.Supports(router.CapReasoning)

	if hasMaxTokens && supportsReasoning {
		if !hasMaxComp {
			val := gjson.GetBytes(body, "max_tokens").Int()
			ov.SetMaxCompletionTokens = &val
		}
		ov.DeleteKeys = append(ov.DeleteKeys, "max_tokens")
	}

	cap := modelMaxOutputTokens[opts.TargetModel]
	if cap == 0 {
		cap = defaultMaxOutputTokenCap
	}
	maxTokensDeleted := hasMaxTokens && supportsReasoning
	if hasMaxTokens && !maxTokensDeleted {
		ov.ClampMaxTokensKey = "max_tokens"
		ov.ClampMaxTokensValue = int64(cap)
	}
	if hasMaxComp || ov.SetMaxCompletionTokens != nil {
		ov.ClampMaxCompTokensValue = int64(cap)
	}

	if !hasMaxTokens && !hasMaxComp {
		if supportsReasoning {
			ov.DefaultMaxTokensKey = "max_completion_tokens"
		} else {
			ov.DefaultMaxTokensKey = "max_tokens"
		}
		ov.DefaultMaxTokensValue = defaultOutputTokens(opts.TargetModel)
	}

	if opts.IncludeStreamUsage {
		ov.InjectStreamUsage = true
	}

	return ov
}

func resolveAnthropicOverrides(body []byte, opts EmitOptions) EmitOverrides {
	ov := EmitOverrides{
		Model: opts.TargetModel,
	}

	thinkingResult := gjson.GetBytes(body, "thinking")
	if thinkingResult.Exists() {
		thinkingType := thinkingResult.Get("type").String()
		shouldDelete := false
		switch thinkingType {
		case "adaptive":
			shouldDelete = !opts.Capabilities.Supports(router.CapAdaptiveThinking)
		case "enabled":
			shouldDelete = !opts.Capabilities.Supports(router.CapExtendedThinking)
		default:
			shouldDelete = !opts.Capabilities.Supports(router.CapAdaptiveThinking) && !opts.Capabilities.Supports(router.CapExtendedThinking)
		}
		if shouldDelete {
			ov.DeleteKeys = append(ov.DeleteKeys, "thinking")
		}
	}

	if !opts.Capabilities.Supports(router.CapAdaptiveThinking) {
		for _, key := range []string{"context_management", "effort", "output_config"} {
			if gjson.GetBytes(body, key).Exists() {
				ov.DeleteKeys = append(ov.DeleteKeys, key)
			}
		}
	}

	if !opts.Capabilities.Supports(router.CapAdaptiveThinking) && !opts.Capabilities.Supports(router.CapExtendedThinking) {
		ov.StripThinkingBlocks = true
	}

	if !gjson.GetBytes(body, "max_tokens").Exists() {
		ov.DefaultMaxTokensKey = "max_tokens"
		ov.DefaultMaxTokensValue = defaultOutputTokens(opts.TargetModel)
	}

	return ov
}

// resolvePassthroughOverrides strips inference-time fields that non-routing
// Anthropic endpoints reject.
func resolvePassthroughOverrides(body []byte) (EmitOverrides, bool) {
	var deleteKeys []string
	for _, key := range []string{"effort", "thinking", "context_management", "output_config"} {
		if gjson.GetBytes(body, key).Exists() {
			deleteKeys = append(deleteKeys, key)
		}
	}
	if len(deleteKeys) == 0 {
		return EmitOverrides{}, false
	}
	return EmitOverrides{
		Model:      gjson.GetBytes(body, "model").String(),
		DeleteKeys: deleteKeys,
	}, true
}

func shallowClone(src map[string]any) map[string]any {
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

var modelMaxOutputTokens = map[string]int{
	"gpt-4.1": 32768, "gpt-4.1-mini": 32768, "gpt-4.1-nano": 32768,
	"gpt-4o": 16384, "gpt-4o-mini": 16384,
	"gpt-4-turbo": 4096, "gpt-4": 8192,
	"gpt-5": 128000, "gpt-5-chat": 128000, "gpt-5-pro": 128000,
	"gpt-5-mini": 128000, "gpt-5-nano": 128000,
	"gpt-5.1": 128000, "gpt-5.2": 128000, "gpt-5.2-pro": 128000,
	"gpt-5.3": 128000, "gpt-5.4": 128000, "gpt-5.4-pro": 128000,
	"gpt-5.4-mini": 128000, "gpt-5.4-nano": 128000,
	"gpt-5.5": 128000, "gpt-5.5-pro": 128000, "gpt-5.5-mini": 128000,
	"gpt-5.5-nano": 128000,
	"o1":           100000, "o1-pro": 100000, "o1-mini": 65536,
	"o3": 100000, "o3-pro": 100000, "o3-mini": 100000,
	"o4-mini":              100000,
	"gemini-3-pro-preview": 65536, "gemini-3.1-pro-preview": 65536,
	"gemini-3-flash-preview": 65536, "gemini-3.1-flash-lite-preview": 65536,
	"gemini-3.1-flash-live-preview": 65536,
	"gemini-2.5-pro":                65536, "gemini-2.5-flash": 65536,
	"gemini-2.5-flash-lite": 65536,
	"gemini-2.0-flash":      8192, "gemini-2.0-flash-lite": 8192,
}

const defaultMaxOutputTokenCap = 8192

// defaultOutputTokens returns the default max output tokens for a model,
// floored by the model's own cap and globally at defaultMaxOutputTokenCap.
func defaultOutputTokens(model string) int64 {
	if cap, ok := modelMaxOutputTokens[model]; ok && cap < defaultMaxOutputTokenCap {
		return int64(cap)
	}
	return defaultMaxOutputTokenCap
}

// clampOutputTokens caps max_tokens/max_completion_tokens in a map.
func clampOutputTokens(doc map[string]any, model string) {
	cap := modelMaxOutputTokens[model]
	if cap == 0 {
		cap = defaultMaxOutputTokenCap
	}
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		v, ok := doc[key]
		if !ok {
			continue
		}
		f, ok := v.(float64)
		if !ok {
			continue
		}
		if f > float64(cap) {
			doc[key] = cap
		}
	}
}
