package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"workweave/router/internal/router"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// INVARIANTS:
//   - e.body is immutable — set once at parse, never modified. Callers must
//     not hold a reference to e.body and mutate it; the slice is shared
//     across all same-format emit calls for the envelope's lifetime.
//   - Same-format emit (OpenAI→OpenAI, Anthropic→Anthropic) uses byte-level
//     overrides (gjson/sjson) on e.body — no cloning or json.Marshal.
//   - Cross-format emit pulls fields from e.ensureSrc() into a new map[string]any.
//     The Unmarshal is deferred until the first cross-format call.
//   - Field accessors (Model, Stream, HasTools) and RoutingFeatures use gjson
//     directly on e.body — zero allocations, no Unmarshal required.

// ErrNotJSONObject is returned when the request body is not a valid JSON object.
var ErrNotJSONObject = errors.New("request body must be a JSON object")

type Format int

const (
	FormatOpenAI Format = iota
	FormatAnthropic
)

// EmitOptions parameterizes how the envelope constructs an output body.
type EmitOptions struct {
	TargetModel        string
	Capabilities       router.ModelSpec
	IncludeStreamUsage bool
}

// RequestEnvelope wraps a parsed request body regardless of wire format.
// Use Prepare* methods to emit target-format bytes, and accessor methods
// (Stream, Model, HasTools) or RoutingFeatures for field reads.
type RequestEnvelope struct {
	body   []byte         // immutable raw request bytes, set once at parse time
	src    map[string]any // lazily populated; only needed for cross-format emit
	format Format
}

// ParseOpenAI validates body as a JSON object and wraps it in a RequestEnvelope.
// The full json.Unmarshal is deferred until a cross-format emit is requested.
func ParseOpenAI(body []byte) (*RequestEnvelope, error) {
	if err := validateJSONObject(body); err != nil {
		return nil, err
	}
	return &RequestEnvelope{body: body, format: FormatOpenAI}, nil
}

// ParseAnthropic validates body as a JSON object and wraps it in a RequestEnvelope.
// The full json.Unmarshal is deferred until a cross-format emit is requested.
func ParseAnthropic(body []byte) (*RequestEnvelope, error) {
	if err := validateJSONObject(body); err != nil {
		return nil, err
	}
	return &RequestEnvelope{body: body, format: FormatAnthropic}, nil
}

// validateJSONObject checks that body is valid JSON and specifically an object
// (rejects arrays, scalars, and null).
func validateJSONObject(body []byte) error {
	if !gjson.ValidBytes(body) {
		return fmt.Errorf("%w: invalid JSON", ErrNotJSONObject)
	}
	if !gjson.ParseBytes(body).IsObject() {
		return fmt.Errorf("%w: body is not a JSON object", ErrNotJSONObject)
	}
	return nil
}

// ensureSrc lazily unmarshals e.body into e.src on first call. Only needed
// by cross-format emit paths that must walk the parsed structure.
// Returns an error when encoding/json rejects input that gjson accepted
// (e.g. nesting beyond encoding/json's 500-level limit).
func (e *RequestEnvelope) ensureSrc() (map[string]any, error) {
	if e.src == nil {
		if err := json.Unmarshal(e.body, &e.src); err != nil {
			return nil, fmt.Errorf("translate: unmarshal request body: %w", err)
		}
	}
	return e.src, nil
}

func (e *RequestEnvelope) SourceFormat() Format { return e.format }

// Stream reports whether the request has "stream": true.
// Accepts JSON booleans and string values ("true"/"false") but rejects
// numeric coercion (e.g. 1 is not treated as true).
func (e *RequestEnvelope) Stream() bool {
	r := gjson.GetBytes(e.body, "stream")
	if r.Type == gjson.Number {
		return false
	}
	return r.Bool()
}

// Model returns the requested model name from the parsed body.
func (e *RequestEnvelope) Model() string {
	return gjson.GetBytes(e.body, "model").String()
}

// MetadataUserID returns the raw metadata.user_id string from the request body.
// Claude Code packs a JSON-encoded object here containing device_id,
// account_uuid, and session_id. Returns "" when the field is absent.
func (e *RequestEnvelope) MetadataUserID() string {
	return gjson.GetBytes(e.body, "metadata.user_id").String()
}

// HasTools reports whether the request contains a non-empty tools array.
func (e *RequestEnvelope) HasTools() bool {
	r := gjson.GetBytes(e.body, "tools.#")
	return r.Int() > 0
}

// EmitOverrides describes byte-level mutations for same-format serialization.
// Zero-valued fields are no-ops.
type EmitOverrides struct {
	Model string // always set: target model name

	DeleteKeys []string // top-level keys to unconditionally remove

	SetMaxCompletionTokens *int64 // if non-nil, set max_completion_tokens to this value

	ClampMaxTokensKey       string // field to clamp (e.g. "max_tokens"); empty = skip
	ClampMaxTokensValue     int64
	ClampMaxCompTokensValue int64 // applied after SetMaxCompletionTokens

	InjectStreamUsage   bool // set stream_options.include_usage = true
	StripThinkingBlocks bool // filter thinking/redacted_thinking from messages[*].content[*]
}

// emitSameFormat applies overrides to e.body via sjson, returning new bytes.
func (e *RequestEnvelope) emitSameFormat(ov EmitOverrides) ([]byte, error) {
	return applyOverrides(e.body, ov)
}

// applyOverrides applies mutations in order: structural (thinking blocks),
// field overrides, then deletions.
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

// clampFieldBytes caps a numeric JSON field to maxVal. No-op if absent or
// not a number.
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
// messages[*].content[*].
//
// Uses two-phase reconstruction to guarantee O(B) total work. A per-block
// sjson.DeleteBytes approach would be O(N*B) — each call rewrites the full
// buffer — which is a DoS vector when N is large (~300K blocks in 10 MB).
//
// Phase 1: keep unaffected msg.Raw verbatim; for affected messages, rebuild
// the content array from non-thinking block raws via sjson.SetRaw on the
// per-message buffer.
// Phase 2: join all message raws and replace "messages" with a single
// sjson.SetRawBytes call.
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

		// sjson.SetRaw on the per-message buffer, not the full body.
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

	// Single body-level replacement: join all message raws and write once.
	newMessagesArray := "[" + strings.Join(msgRaws, ",") + "]"
	return sjson.SetRawBytes(body, "messages", []byte(newMessagesArray))
}

// resolveOpenAIOverrides builds EmitOverrides for OpenAI → OpenAI emit.
func resolveOpenAIOverrides(body []byte, opts EmitOptions) EmitOverrides {
	ov := EmitOverrides{
		Model: opts.TargetModel,
	}

	ov.DeleteKeys = append(ov.DeleteKeys, "thinking")

	if gjson.GetBytes(body, "reasoning_effort").Exists() && !opts.Capabilities.Supports(router.CapReasoning) {
		ov.DeleteKeys = append(ov.DeleteKeys, "reasoning_effort")
	}

	if gjson.GetBytes(body, "max_tokens").Exists() && opts.Capabilities.Supports(router.CapReasoning) {
		if !gjson.GetBytes(body, "max_completion_tokens").Exists() {
			val := gjson.GetBytes(body, "max_tokens").Int()
			ov.SetMaxCompletionTokens = &val
		}
		ov.DeleteKeys = append(ov.DeleteKeys, "max_tokens")
	}

	cap := modelMaxOutputTokens[opts.TargetModel]
	if cap == 0 {
		cap = defaultMaxOutputTokenCap
	}
	maxTokensDeleted := gjson.GetBytes(body, "max_tokens").Exists() && opts.Capabilities.Supports(router.CapReasoning)
	if gjson.GetBytes(body, "max_tokens").Exists() && !maxTokensDeleted {
		ov.ClampMaxTokensKey = "max_tokens"
		ov.ClampMaxTokensValue = int64(cap)
	}
	if gjson.GetBytes(body, "max_completion_tokens").Exists() || ov.SetMaxCompletionTokens != nil {
		ov.ClampMaxCompTokensValue = int64(cap)
	}

	if opts.IncludeStreamUsage {
		ov.InjectStreamUsage = true
	}

	return ov
}

// resolveAnthropicOverrides builds EmitOverrides for Anthropic → Anthropic emit.
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

	return ov
}

// resolvePassthroughOverrides builds EmitOverrides for Anthropic passthrough
// endpoints, stripping inference-time fields that non-routing endpoints reject.
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

// shallowClone copies top-level keys of a map. Used by cross-format paths only.
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

// clampOutputTokens caps max_tokens/max_completion_tokens in a map. Cross-format only.
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
