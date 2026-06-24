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

// INVARIANTS:
//   - e.body is immutable after parse. Same-format emit uses gjson/sjson overrides
//     on e.body with no cloning. Cross-format emit reads via gjson, writes via jsonWriter.
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
	TargetModel string
	// TargetProvider is the resolved upstream provider name (the
	// providers.Provider* constant, e.g. "openrouter", "fireworks",
	// "deepinfra", "bedrock"). OpenAI-compat emission keys provider-
	// specific hints (the OpenRouter `provider`/`reasoning` body fields,
	// tool-turn temperature override, system reminders) on this so the
	// same model slug routed to a non-OpenRouter binding (post SOC 2
	// isolation) doesn't carry OpenRouter-only fields the direct upstream
	// rejects with 400. Empty falls back to model-slug behavior for
	// callers that don't plumb a provider through yet (handover summary).
	TargetProvider     string
	Capabilities       router.ModelSpec
	IncludeStreamUsage bool
	// SessionAffinity is a stable per-conversation identifier forwarded to
	// the upstream as a prompt-cache stickiness hint so a session's turns
	// land on the same serverless replica (where the prefix KV-cache lives)
	// rather than being load-balanced to a cold one. The knob differs per
	// upstream — see applySessionAffinity. Empty disables the hint.
	SessionAffinity string
	// ModelSwitched reports that the model serving this turn differs from the
	// one that served the previous turn in the same session. Anthropic
	// thinking-block `signature`s are only valid for the model that produced
	// them, so historical thinking blocks carried over from a different model
	// (after auto-routing or a /force-model switch) make Anthropic reject the
	// request with `Invalid signature in thinking block` (400). When set, the
	// Anthropic emit path strips thinking blocks from the conversation so the
	// stale signatures never reach the upstream.
	ModelSwitched bool
	// ForceReasoningEffort, when non-empty ("low"/"medium"/"high"), overrides the
	// request-derived reasoning effort for reasoning-capable targets (gpt-5.x via
	// the Responses API, gemini-3.x via thinkingConfig). Set by the proxy's
	// escalate-on-failure policy: gpt-5.x serves "low" by default and "high" after
	// an observed failed/no-progress turn; gemini is pinned "low" (effort-immune on
	// hard tasks). Empty leaves the request-derived effort untouched, so the
	// feature is a no-op unless the policy is enabled.
	ForceReasoningEffort string
	// EnableExtendedContext injects the context-1m-2025-08-07 Anthropic beta on
	// the upstream request so CapExtendedContext targets (Opus 4.6+, Sonnet 4.6)
	// serve at their 1M window instead of the 200K default. The proxy sets this
	// for every extended-context-capable Anthropic dispatch so a large request is
	// never sent to a model whose default window it would immediately overflow
	// (Anthropic 400 "prompt is too long"). Below 200K input it is a no-op:
	// standard pricing, no behavior change. deriveAnthropicHeaders gates the
	// actual injection on the target's CapExtendedContext support.
	EnableExtendedContext bool
	// DowngradeGeminiValidatedToAuto, when true, makes the Gemini emit path emit
	// functionCallingConfig.mode=AUTO in place of the mode=VALIDATED it would
	// otherwise set for a tools-with-no-forced-choice Gemini 3.x request. Under
	// VALIDATED, Gemini compiles every tool's parameter schema into a decode-time
	// grammar; a schema it can't compile makes it reject the whole request with a
	// generic 400 INVALID_ARGUMENT. The proxy sets this on a one-shot retry after
	// such a 400 — AUTO skips grammar compilation, so the same tools survive. A
	// no-op when the request would not have used VALIDATED.
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

// clientSessionEmbeddedUUID matches a UUID following an explicit "session_" or
// "session-" or "sessionId=" / "sessionId:" marker. Lets us pull the bare
// session UUID out of bundled identifiers like Claude Code's
// "user_<account>_account__session_<session>".
var clientSessionEmbeddedUUID = regexp.MustCompile(
	`(?i)session[_\-]?(?:id[=:])?([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`,
)

// clientSessionTrailingUUID matches a UUID at the end of a string. Fallback for
// formats that just dump the session UUID with no marker prefix.
var clientSessionTrailingUUID = regexp.MustCompile(
	`([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`,
)

const clientSessionIDMaxLen = 64

// ClientSessionID returns the calling client's own session identifier, suitable
// for log correlation. Unlike the internal session_key (a sha256 over apiKeyID
// + user_id used for sticky-pin lookup), this is the value the client knows
// itself by — so running `/status` in Claude Code (or the equivalent in
// OpenCode / Codex) yields a string the operator can grep for in router logs.
//
// Per-ingress source:
//   - Anthropic (`metadata.user_id`): Claude Code packs the bundle
//     "user_<account>_account__session_<session>"; we pull the trailing UUID.
//   - OpenAI (`user`): Codex / OpenCode put the session UUID here directly;
//     same extraction handles wrapped forms.
//   - Gemini: no canonical field today; we still check `metadata.user_id` so a
//     wrapper that sets it gets picked up.
//
// If the raw value contains a UUID-shaped session marker we return the bare
// UUID; otherwise we return the raw value truncated to clientSessionIDMaxLen
// so a free-form string still grep-correlates without bloating log lines.
// Returns "" when nothing usable is set.
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
	// Claude Code (post 0.x) packs the identifier as a stringified JSON object
	// like {"device_id":"…","session_id":"<uuid>","account_id":"…"}. Probe the
	// well-known session-id keys first; only fall through to regex on the raw
	// string when no key matches.
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

// jsonSessionIDField returns the value of the first matching session-id field
// when raw is a JSON object, else "". Order matches observed client shapes:
// Claude Code uses "session_id"; OpenAI surfaces have variously used
// "sessionId", and chat-style clients sometimes use "conversation_id".
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
	// ToolResultBytes is the summed raw-JSON byte size of the trailing turn's
	// tool_result payload(s) — the incoming tool-output size. A cheap structural
	// triviality proxy for the right-sizing tier-cap shadow (tiny tool_result →
	// likely-trivial continuation). 0 when no tool_result is present.
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

// ToolValidator compiles the inbound Anthropic tool definitions
// (tools[].input_schema) into a toolcheck.Validator, used by the response
// translators to validate and repair model-emitted tool calls. Returns nil
// for non-Anthropic source formats or when the request carries no tools —
// translators treat a nil validator as syntax-check-only. Compilation is
// amortized across turns via toolcheck's LRU (agent sessions resend a
// byte-identical tools block every turn).
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

// HasImages reports whether any message carries image (or other inline media)
// content. Used to keep image-bearing turns off text-only models, which reject
// image parts with a 4xx (e.g. DeepInfra's "does not accept image input" on
// GLM-5.1). A single image anywhere in the history is enough — clients like
// Cursor re-send earlier pasted screenshots on every subsequent turn.
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
	// SanitizeToolUseIDs rewrites tool_use.id / tool_use_id values that contain
	// characters outside ^[a-zA-Z0-9_-]+$. Always set when targeting Anthropic:
	// non-Anthropic upstreams (e.g. Kimi-k2.6) emit IDs like "functions.Read:0"
	// that Anthropic rejects when the history is forwarded back to it.
	SanitizeToolUseIDs bool
	// StripThoughtSignature removes the `thought_signature` field from every
	// messages[*].content[*] block. Set when targeting Anthropic: Gemini 3.x
	// signatures are foreign to Anthropic, and Anthropic rejects unknown block
	// fields with a non-retryable 400.
	StripThoughtSignature bool
	// RewriteThinkingAdaptive replaces the inbound thinking block with
	// {"type":"adaptive"} and sets output_config.effort. Used when the target
	// model only accepts adaptive thinking (claude-opus-4-6+ / sonnet-4-6+).
	RewriteThinkingAdaptive bool
	OutputConfigEffort      string
	// ClampEffortXhighTo downgrades a caller-supplied effort level of "xhigh"
	// (top-level `effort` and `output_config.effort`) to this value. Set when
	// the target model's effort menu lacks xhigh (router.CapXhighEffort): a
	// mid-session re-route from opus to sonnet otherwise forwards the client's
	// xhigh verbatim and Anthropic rejects the request with a non-retryable
	// 400, killing the session.
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

// feedbackFooterPattern matches the rating footer appended at the end of a
// streamed response (see proxy.Service.feedbackFooter). It tolerates both
// rendered forms — the clickable thumb links and the link-free hint — by
// absorbing everything from the sentinel through the trailing `/rf-` companion,
// which both forms end with. Leading newlines are absorbed so the blank-line
// separator is removed with the footer. Stripping on ingress keeps the footer
// (and its signed rate URLs) out of upstream context on later turns, the same
// failure mode the routing marker had.
var feedbackFooterPattern = regexp.MustCompile("\\n*_Was this routing right\\?_ [^\\n]*?`/rf-`")

// StripRoutingMarkerFromMessages removes the routing-marker snippet from every
// text block in messages[*].content[*]. Stripping on ingress keeps it out of
// upstream context and stabilizes assistant prefixes for prompt-cache reuse.
func StripRoutingMarkerFromMessages(body []byte) ([]byte, error) {
	return stripPatternFromMessages(body, routingMarkerPattern)
}

// StripFeedbackFooterFromMessages removes the one-click thumbs footer from every
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

// effortForBudget maps the legacy thinking.budget_tokens value onto the
// adaptive output_config.effort tier. The thresholds match Anthropic's
// published guidance: ≤4k tokens is "low" headroom, ≤16k is "medium", and
// anything larger is "high". Zero or missing budget defaults to "medium" so
// pre-rollout clients keep working.
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

	// Adaptive targets keep caller-supplied effort, but the menu differs per
	// model: "xhigh" is opus-4-7+ only. Clamp to the highest level every
	// adaptive model accepts so a re-route never manufactures an invalid
	// request out of a valid one.
	if opts.Capabilities.Supports(router.CapAdaptiveThinking) && !opts.Capabilities.Supports(router.CapXhighEffort) {
		ov.ClampEffortXhighTo = effortMax
	}

	if !opts.Capabilities.Supports(router.CapAdaptiveThinking) && !opts.Capabilities.Supports(router.CapExtendedThinking) {
		ov.StripThinkingBlocks = true
	}

	// On a mid-session model switch, any thinking blocks carried over in the
	// conversation history were signed by the previous model; Anthropic rejects
	// them with `Invalid signature in thinking block` (400). Strip them so the
	// stale signatures never reach the upstream. The new model produces its own
	// thinking fresh, so nothing is lost beyond cross-model reasoning continuity.
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
