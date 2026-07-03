package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"workweave/router/internal/router"
	"workweave/router/internal/translate/toolcheck"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// INVARIANTS: e.body is immutable after parse. Same-format emit overrides it via
// gjson/sjson with no cloning; cross-format emit reads via gjson, writes via jsonWriter.

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
	TargetModel string
	// TargetProvider is the resolved upstream provider (providers.Provider*,
	// e.g. "openrouter", "fireworks"). Gates OpenRouter-only body fields
	// (provider/reasoning hints, tool-turn temp override) so a model slug
	// rebound to a non-OpenRouter upstream doesn't send fields it 400s on.
	// Empty falls back to model-slug behavior for callers not yet plumbing it.
	TargetProvider     string
	Capabilities       router.ModelSpec
	IncludeStreamUsage bool
	// SessionAffinity is a per-conversation ID forwarded as a prompt-cache
	// stickiness hint so a session lands on the same warm replica instead of
	// a cold one. Knob differs per upstream — see applySessionAffinity.
	SessionAffinity string
	// ModelSwitched reports the serving model changed since the last turn.
	// Thinking-block signatures are only valid for the model that produced
	// them, so carried-over blocks make Anthropic 400 with "Invalid signature
	// in thinking block". When set, the emit path strips thinking blocks.
	ModelSwitched bool
	// ForceReasoningEffort, when non-empty, overrides the request-derived
	// reasoning effort for gpt-5.x (Responses API) / gemini-3.x (thinkingConfig).
	// Set by the proxy's escalate-on-failure policy: gpt-5.x starts "low", goes
	// "high" after a failed/no-progress turn; gemini stays "low" (effort-immune).
	ForceReasoningEffort string
	// EnableExtendedContext injects the context-1m-2025-08-07 beta so
	// CapExtendedContext targets (Opus 4.6+, Sonnet 4.6) get a 1M window
	// instead of 200K, avoiding a 400 "prompt is too long" on large requests.
	// No-op below 200K input. deriveAnthropicHeaders gates on CapExtendedContext.
	EnableExtendedContext bool
	// DowngradeGeminiValidatedToAuto emits functionCallingConfig.mode=AUTO
	// instead of VALIDATED for Gemini 3.x. VALIDATED compiles tool schemas into
	// a decode-time grammar and 400s INVALID_ARGUMENT if one won't compile; the
	// proxy sets this on a one-shot retry after such a 400 since AUTO skips
	// compilation. No-op when VALIDATED wouldn't have been used.
	DowngradeGeminiValidatedToAuto bool
}

// RequestEnvelope wraps a parsed request body regardless of wire format.
// Use Prepare* to emit target-format bytes; accessors read fields.
type RequestEnvelope struct {
	body   []byte
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

// clientSessionEmbeddedUUID pulls the bare session UUID out of bundled
// identifiers like Claude Code's "user_<account>_account__session_<session>".
var clientSessionEmbeddedUUID = regexp.MustCompile(
	`(?i)session[_\-]?(?:id[=:])?([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`,
)

// clientSessionTrailingUUID matches a UUID at the end of a string. Fallback for
// formats that just dump the session UUID with no marker prefix.
var clientSessionTrailingUUID = regexp.MustCompile(
	`([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`,
)

const clientSessionIDMaxLen = 64

// ClientSessionID returns the calling client's own session identifier for log
// correlation — unlike the internal session_key (sha256 of apiKeyID+user_id),
// this is the value visible to the client itself (e.g. via `/status`).
// Extracted from metadata.user_id (Anthropic/Gemini) or user (OpenAI); a
// UUID-shaped marker is pulled out bare, otherwise the raw value is truncated
// to clientSessionIDMaxLen. Returns "" when nothing usable is set.
func (e *RequestEnvelope) ClientSessionID() string {
	var raw string
	switch e.format {
	case FormatAnthropic, FormatGemini:
		raw = gjson.GetBytes(e.body, "metadata.user_id").String()
	case FormatOpenAI:
		raw = gjson.GetBytes(e.body, "user").String()
		if raw == "" {
			raw = gjson.GetBytes(e.body, "metadata.user_id").String()
		}
	}
	if raw == "" {
		return ""
	}
	// Claude Code packs the identifier as a stringified JSON object like
	// {"device_id":"…","session_id":"<uuid>","account_id":"…"}; probe known
	// keys before falling back to regex.
	if id := jsonSessionIDField(raw); id != "" {
		return id
	}
	if m := clientSessionEmbeddedUUID.FindStringSubmatch(raw); m != nil {
		return m[1]
	}
	if m := clientSessionTrailingUUID.FindStringSubmatch(raw); m != nil {
		return m[1]
	}
	if len(raw) > clientSessionIDMaxLen {
		return raw[:clientSessionIDMaxLen]
	}
	return raw
}

// jsonSessionIDField returns the first matching session-id field when raw is
// a JSON object, else "". Key order matches observed client shapes.
func jsonSessionIDField(raw string) string {
	if len(raw) == 0 || raw[0] != '{' {
		return ""
	}
	if !gjson.Valid(raw) {
		return ""
	}
	for _, key := range [...]string{"session_id", "sessionId", "conversation_id", "conversationId"} {
		if v := gjson.Get(raw, key).String(); v != "" {
			return v
		}
	}
	return ""
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
	// ToolResultBytes sums the trailing turn's tool_result payload size — a
	// cheap triviality proxy for the tier-cap shadow (tiny result → likely
	// trivial continuation). 0 when no tool_result is present.
	ToolResultBytes int
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

// ToolValidator compiles inbound Anthropic tool definitions into a
// toolcheck.Validator for validating/repairing model-emitted tool calls.
// Returns nil for non-Anthropic formats or no tools (translators treat nil as
// syntax-check-only). Compilation is cached via toolcheck's LRU since agent
// sessions resend a byte-identical tools block every turn.
func (e *RequestEnvelope) ToolValidator() *toolcheck.Validator {
	if e.format != FormatAnthropic {
		return nil
	}
	tools := gjson.GetBytes(e.body, "tools")
	if !tools.IsArray() {
		return nil
	}
	return toolcheck.CompileCached([]byte(tools.Raw))
}

// HasImages reports whether any message carries image content. Used to keep
// such turns off text-only models, which 4xx on image parts (e.g. DeepInfra's
// GLM-5.1). Checks the whole history since clients like Cursor re-send earlier
// screenshots on every turn.
func (e *RequestEnvelope) HasImages() bool {
	switch e.format {
	case FormatAnthropic:
		return anthropicHasImages(e.body)
	case FormatOpenAI:
		return openAIHasImages(e.body)
	case FormatGemini:
		return geminiHasImages(e.body)
	default:
		return false
	}
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
	Model                   string
	DeleteKeys              []string
	SetMaxCompletionTokens  *int64
	ClampMaxTokensKey       string
	ClampMaxTokensValue     int64
	ClampMaxCompTokensValue int64
	DefaultMaxTokensKey     string
	DefaultMaxTokensValue   int64
	InjectStreamUsage       bool
	StripThinkingBlocks     bool
	// SanitizeToolUseIDs rewrites tool_use.id / tool_use_id values outside
	// ^[a-zA-Z0-9_-]+$. Always set for Anthropic targets: upstreams like
	// Kimi-k2.6 emit IDs (e.g. "functions.Read:0") Anthropic rejects on replay.
	SanitizeToolUseIDs bool
	// StripThoughtSignature removes `thought_signature` from content blocks.
	// Set for Anthropic targets: the field is Gemini-only and Anthropic 400s
	// on unknown block fields.
	StripThoughtSignature bool
	// RewriteThinkingAdaptive replaces the inbound thinking block with
	// {"type":"adaptive"} and sets output_config.effort. Used when the target
	// model only accepts adaptive thinking (claude-opus-4-6+ / sonnet-4-6+).
	RewriteThinkingAdaptive bool
	OutputConfigEffort      string
	// ClampEffortXhighTo downgrades a caller-supplied "xhigh" effort (`effort`
	// and `output_config.effort`) to this value. Set when the target lacks
	// xhigh (router.CapXhighEffort) so a mid-session re-route doesn't forward
	// an effort level Anthropic rejects with a session-killing 400.
	ClampEffortXhighTo string
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

	if ov.StripThoughtSignature {
		out, err = stripThoughtSignatureBytes(out)
		if err != nil {
			return nil, fmt.Errorf("strip thought_signature: %w", err)
		}
	}

	if ov.SanitizeToolUseIDs {
		out, err = sanitizeToolUseIDsBytes(out)
		if err != nil {
			return nil, fmt.Errorf("sanitize tool_use ids: %w", err)
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

	if ov.RewriteThinkingAdaptive {
		out, err = sjson.SetBytes(out, "thinking", map[string]string{"type": "adaptive"})
		if err != nil {
			return nil, fmt.Errorf("rewrite thinking to adaptive: %w", err)
		}
		if ov.OutputConfigEffort != "" {
			if !gjson.GetBytes(out, "output_config.effort").Exists() {
				out, err = sjson.SetBytes(out, "output_config.effort", ov.OutputConfigEffort)
				if err != nil {
					return nil, fmt.Errorf("set output_config.effort: %w", err)
				}
			}
		}
	}

	if ov.ClampEffortXhighTo != "" {
		for _, key := range []string{"effort", "output_config.effort"} {
			if gjson.GetBytes(out, key).String() != effortXhigh {
				continue
			}
			out, err = sjson.SetBytes(out, key, ov.ClampEffortXhighTo)
			if err != nil {
				return nil, fmt.Errorf("clamp %s: %w", key, err)
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

// sanitizeToolUseIDsBytes rewrites tool_use.id and tool_use_id in
// messages[*].content[*] that contain characters outside ^[a-zA-Z0-9_-]+$.
// Non-Anthropic upstreams (e.g. Kimi-k2.6) emit IDs like "functions.Read:0";
// Anthropic rejects those with a 400 when the history is forwarded back to it.
func sanitizeToolUseIDsBytes(body []byte) ([]byte, error) {
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

		needsRewrite := false
		content.ForEach(func(_, block gjson.Result) bool {
			switch block.Get("type").String() {
			case "tool_use":
				if id := block.Get("id").String(); sanitizeToolUseID(id) != id {
					needsRewrite = true
					return false
				}
			case "tool_result":
				if id := block.Get("tool_use_id").String(); sanitizeToolUseID(id) != id {
					needsRewrite = true
					return false
				}
			}
			return true
		})

		if !needsRewrite {
			msgRaws = append(msgRaws, msg.Raw)
			return true
		}

		anyChanged = true
		var rewritten []string
		content.ForEach(func(_, block gjson.Result) bool {
			raw := block.Raw
			var err error
			switch block.Get("type").String() {
			case "tool_use":
				if id := block.Get("id").String(); sanitizeToolUseID(id) != id {
					raw, err = sjson.Set(raw, "id", sanitizeToolUseID(id))
					if err != nil {
						walkErr = fmt.Errorf("rewrite tool_use id: %w", err)
						return false
					}
				}
			case "tool_result":
				if id := block.Get("tool_use_id").String(); sanitizeToolUseID(id) != id {
					raw, err = sjson.Set(raw, "tool_use_id", sanitizeToolUseID(id))
					if err != nil {
						walkErr = fmt.Errorf("rewrite tool_use_id: %w", err)
						return false
					}
				}
			}
			rewritten = append(rewritten, raw)
			return true
		})
		if walkErr != nil {
			return false
		}

		newMsg, err := sjson.SetRaw(msg.Raw, "content", "["+strings.Join(rewritten, ",")+"]")
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
	return sjson.SetRawBytes(body, "messages", []byte("["+strings.Join(msgRaws, ",")+"]"))
}

// stripThoughtSignatureBytes removes the `thought_signature` field from every
// messages[*].content[*] block. Anthropic rejects this Gemini-only field; tool
// signatures still survive through the id carrier.
func stripThoughtSignatureBytes(body []byte) ([]byte, error) {
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

		needsStrip := false
		content.ForEach(func(_, block gjson.Result) bool {
			if block.Get("thought_signature").Exists() {
				needsStrip = true
				return false
			}
			return true
		})

		if !needsStrip {
			msgRaws = append(msgRaws, msg.Raw)
			return true
		}

		anyChanged = true
		var rewritten []string
		content.ForEach(func(_, block gjson.Result) bool {
			raw := block.Raw
			if block.Get("thought_signature").Exists() {
				var err error
				raw, err = sjson.Delete(raw, "thought_signature")
				if err != nil {
					walkErr = fmt.Errorf("delete thought_signature: %w", err)
					return false
				}
			}
			rewritten = append(rewritten, raw)
			return true
		})
		if walkErr != nil {
			return false
		}

		newMsg, err := sjson.SetRaw(msg.Raw, "content", "["+strings.Join(rewritten, ",")+"]")
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
	return sjson.SetRawBytes(body, "messages", []byte("["+strings.Join(msgRaws, ",")+"]"))
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

// feedbackFooterPattern matches the rating footer appended to streamed
// responses (see proxy.Service.feedbackFooter), absorbing leading newlines
// and everything to end of line since there's no fixed end-anchor. Keep the
// sentinel in sync with proxy.feedbackFooterText.
var feedbackFooterPattern = regexp.MustCompile("\\n*_Weave Router feedback:_ [^\\n]*")

// StripRoutingMarkerFromMessages removes the routing-marker snippet from every
// text block in messages[*].content[*]. Stripping on ingress keeps it out of
// upstream context and stabilizes assistant prefixes for prompt-cache reuse.
func StripRoutingMarkerFromMessages(body []byte) ([]byte, error) {
	return stripPatternFromMessages(body, routingMarkerPattern)
}

// StripFeedbackFooterFromMessages removes the rating footer from every
// text block in messages[*].content[*]. Like the routing marker, the footer is
// injected as assistant text on egress, so clients echo it back verbatim on the
// next turn; stripping it on ingress keeps it out of upstream context.
func StripFeedbackFooterFromMessages(body []byte) ([]byte, error) {
	return stripPatternFromMessages(body, feedbackFooterPattern)
}

// stripPatternFromMessages removes every match of pattern from each text block
// in messages[*].content[*], handling both the OpenAI plain-string content shape
// and the Anthropic typed-block-array shape. Blocks whose text becomes empty are
// dropped. Returns the original body unchanged when nothing matched.
func stripPatternFromMessages(body []byte, pattern *regexp.Regexp) ([]byte, error) {
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return body, nil
	}

	anyChanged := false
	var msgRaws []string
	var walkErr error

	messages.ForEach(func(_, msg gjson.Result) bool {
		content := msg.Get("content")
		if !content.Exists() {
			msgRaws = append(msgRaws, msg.Raw)
			return true
		}

		// OpenAI format: content is a plain string.
		if content.Type == gjson.String {
			text := content.String()
			if !pattern.MatchString(text) {
				msgRaws = append(msgRaws, msg.Raw)
				return true
			}
			stripped := pattern.ReplaceAllString(text, "")
			anyChanged = true
			encoded, err := encodeJSONStringNoHTMLEscape(stripped)
			if err != nil {
				walkErr = fmt.Errorf("marshal stripped string content: %w", err)
				return false
			}
			newMsg, err := sjson.SetRawBytes([]byte(msg.Raw), "content", encoded)
			if err != nil {
				walkErr = fmt.Errorf("replace string content in message: %w", err)
				return false
			}
			msgRaws = append(msgRaws, string(newMsg))
			return true
		}

		// Anthropic format: content is an array of typed blocks.
		if !content.IsArray() {
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
			if !pattern.MatchString(text) {
				newBlocks = append(newBlocks, block.Raw)
				return true
			}
			stripped := pattern.ReplaceAllString(text, "")
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

// StripFeedbackFooterFromGeminiContents removes the one-click thumbs footer from
// every text part in contents[*].parts[*]. The Gemini footer is emitted as its
// own model text part on egress (see GeminiRoutingFooterWriter), so clients echo
// it back as a standalone part on the next turn; stripping it on ingress keeps
// it out of upstream context. Parts whose text becomes empty are dropped.
func StripFeedbackFooterFromGeminiContents(body []byte) ([]byte, error) {
	contents := gjson.GetBytes(body, "contents")
	if !contents.Exists() || !contents.IsArray() {
		return body, nil
	}

	anyChanged := false
	var contentRaws []string
	var walkErr error

	contents.ForEach(func(_, content gjson.Result) bool {
		parts := content.Get("parts")
		if !parts.Exists() || !parts.IsArray() {
			contentRaws = append(contentRaws, content.Raw)
			return true
		}

		var newParts []string
		contentChanged := false
		parts.ForEach(func(_, part gjson.Result) bool {
			textNode := part.Get("text")
			if !textNode.Exists() {
				newParts = append(newParts, part.Raw)
				return true
			}
			text := textNode.String()
			if !feedbackFooterPattern.MatchString(text) {
				newParts = append(newParts, part.Raw)
				return true
			}
			stripped := feedbackFooterPattern.ReplaceAllString(text, "")
			contentChanged = true
			if strings.TrimSpace(stripped) == "" {
				return true
			}
			encoded, err := encodeJSONStringNoHTMLEscape(stripped)
			if err != nil {
				walkErr = fmt.Errorf("marshal stripped gemini text: %w", err)
				return false
			}
			newPart, err := sjson.SetRawBytes([]byte(part.Raw), "text", encoded)
			if err != nil {
				walkErr = fmt.Errorf("replace text in gemini part: %w", err)
				return false
			}
			newParts = append(newParts, string(newPart))
			return true
		})
		if walkErr != nil {
			return false
		}
		if !contentChanged {
			contentRaws = append(contentRaws, content.Raw)
			return true
		}

		anyChanged = true
		newPartsArray := "[" + strings.Join(newParts, ",") + "]"
		newContent, err := sjson.SetRawBytes([]byte(content.Raw), "parts", []byte(newPartsArray))
		if err != nil {
			walkErr = fmt.Errorf("replace parts in gemini content: %w", err)
			return false
		}
		contentRaws = append(contentRaws, string(newContent))
		return true
	})

	if walkErr != nil {
		return nil, walkErr
	}
	if !anyChanged {
		return body, nil
	}
	return sjson.SetRawBytes(body, "contents", []byte("["+strings.Join(contentRaws, ",")+"]"))
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

// Anthropic adaptive effort levels referenced by emit logic. Every adaptive
// model accepts low/medium/high/max; xhigh requires router.CapXhighEffort.
const (
	effortMax   = "max"
	effortXhigh = "xhigh"
)

// effortForBudget maps legacy thinking.budget_tokens onto an adaptive
// output_config.effort tier per Anthropic's guidance (≤4k low, ≤16k medium,
// else high). Missing/zero budget defaults to "medium".
func effortForBudget(budgetTokens int64) string {
	switch {
	case budgetTokens <= 0:
		return "medium"
	case budgetTokens <= 4096:
		return "low"
	case budgetTokens <= 16384:
		return "medium"
	default:
		return "high"
	}
}

func resolveAnthropicOverrides(body []byte, opts EmitOptions) EmitOverrides {
	ov := EmitOverrides{
		Model:                 opts.TargetModel,
		SanitizeToolUseIDs:    true,
		StripThoughtSignature: true,
	}

	thinkingResult := gjson.GetBytes(body, "thinking")
	if thinkingResult.Exists() {
		thinkingType := thinkingResult.Get("type").String()
		shouldDelete := false
		switch thinkingType {
		case "adaptive":
			shouldDelete = !opts.Capabilities.Supports(router.CapAdaptiveThinking)
		case "enabled":
			if opts.Capabilities.Supports(router.CapExtendedThinking) {
				// Target accepts the legacy shape; leave it untouched.
			} else if opts.Capabilities.Supports(router.CapAdaptiveThinking) {
				ov.RewriteThinkingAdaptive = true
				ov.OutputConfigEffort = effortForBudget(thinkingResult.Get("budget_tokens").Int())
			} else {
				shouldDelete = true
			}
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

	// "xhigh" is opus-4-7+ only; clamp to the max every adaptive model accepts
	// so a re-route can't turn a valid request into an invalid one.
	if opts.Capabilities.Supports(router.CapAdaptiveThinking) && !opts.Capabilities.Supports(router.CapXhighEffort) {
		ov.ClampEffortXhighTo = effortMax
	}

	if !opts.Capabilities.Supports(router.CapAdaptiveThinking) && !opts.Capabilities.Supports(router.CapExtendedThinking) {
		ov.StripThinkingBlocks = true
	}

	// Carried-over thinking blocks were signed by the previous model; strip
	// them so stale signatures don't 400 against the new one.
	if opts.ModelSwitched {
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
