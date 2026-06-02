package translate

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func hasNonEmptyTools(body []byte) bool {
	tools := gjson.GetBytes(body, "tools")
	return tools.Exists() && tools.IsArray() && tools.Get("#").Int() > 0
}

// PrepareOpenAI builds an OpenAI Chat Completions request body.
func (e *RequestEnvelope) PrepareOpenAI(in http.Header, opts EmitOptions) (providers.PreparedRequest, error) {
	var body []byte
	var stats providers.RequestMutationStats
	var err error
	switch e.format {
	case FormatOpenAI:
		body, err = e.buildOpenAIFromOpenAI(opts)
	case FormatAnthropic:
		body, stats, err = e.buildOpenAIFromAnthropic(opts)
	default:
		return providers.PreparedRequest{}, fmt.Errorf("unsupported source format for OpenAI emit: %d", e.format)
	}
	if err != nil {
		return providers.PreparedRequest{}, err
	}
	headers := make(http.Header)
	body, err = applySessionAffinity(body, headers, opts)
	if err != nil {
		return providers.PreparedRequest{}, err
	}
	return providers.PreparedRequest{Body: body, Headers: headers, Stats: stats}, nil
}

// applySessionAffinity attaches an upstream-specific prompt-cache routing hint
// derived from opts.SessionAffinity (a stable per-conversation identifier).
// Serverless upstreams fan a session's turns across replicas and hold the
// prefix KV-cache per replica; without a stickiness hint, a turn can land on a
// cold replica and pay a full prefill (the deepseek-v4-pro/Fireworks incident:
// 60k-token turn, zero cache read, 26s TTFT). Each upstream exposes a different
// knob:
//   - Fireworks / DeepInfra: x-session-affinity request header
//   - OpenRouter: x-session-id request header (its sticky-routing key, ≤256 chars)
//   - OpenAI: prompt_cache_key body field (combined with the prefix hash)
//
// Bedrock (explicit cachePoint caching, centrally routed — no replica roulette)
// and any unrecognized target get nothing. No-op when SessionAffinity is empty,
// which also covers the handover summarizer's empty-TargetProvider calls.
func applySessionAffinity(body []byte, headers http.Header, opts EmitOptions) ([]byte, error) {
	if opts.SessionAffinity == "" {
		return body, nil
	}
	switch opts.TargetProvider {
	case providers.ProviderFireworks, providers.ProviderDeepInfra:
		headers.Set("x-session-affinity", opts.SessionAffinity)
	case providers.ProviderOpenRouter:
		headers.Set("x-session-id", opts.SessionAffinity)
	case providers.ProviderOpenAI:
		out, err := sjson.SetBytes(body, "prompt_cache_key", opts.SessionAffinity)
		if err != nil {
			return nil, fmt.Errorf("set prompt_cache_key: %w", err)
		}
		return out, nil
	}
	return body, nil
}

func (e *RequestEnvelope) buildOpenAIFromOpenAI(opts EmitOptions) ([]byte, error) {
	ov := resolveOpenAIOverrides(e.body, opts)
	body, err := e.emitSameFormat(ov)
	if err != nil {
		return nil, err
	}
	if targetIsOpenRouter(opts) {
		if hint := openRouterProviderHint(opts.TargetModel); hint != nil {
			body, err = sjson.SetBytes(body, "provider", hint)
			if err != nil {
				return nil, fmt.Errorf("set openrouter provider hint: %w", err)
			}
		}
		if reasoning := openRouterReasoningHint(opts.TargetModel); reasoning != nil {
			body, err = sjson.SetBytes(body, "reasoning", reasoning)
			if err != nil {
				return nil, fmt.Errorf("set openrouter reasoning hint: %w", err)
			}
		}
		if reminder := openRouterSystemReminder(opts.TargetModel); reminder != "" && hasNonEmptyTools(body) {
			body, err = applySystemReminderToBody(body, reminder)
			if err != nil {
				return nil, fmt.Errorf("set system reminder: %w", err)
			}
		}
		if openRouterForcesToolTemperatureZero(opts.TargetModel) &&
			hasNonEmptyTools(body) &&
			!gjson.GetBytes(body, "temperature").Exists() {
			body, err = sjson.SetBytes(body, "temperature", 0)
			if err != nil {
				return nil, fmt.Errorf("set tool temperature override: %w", err)
			}
		}
	}
	body, err = applyQwen3SamplersIfNeeded(body, opts.TargetModel)
	if err != nil {
		return nil, err
	}
	body, err = applyGLM51FlagsIfNeeded(body, opts)
	if err != nil {
		return nil, err
	}
	return body, nil
}

// targetIsOpenRouter reports whether the emit target is the OpenRouter
// upstream. OpenRouter-specific body fields (`provider`, `reasoning`),
// system reminders, and tool-turn temperature overrides only belong on
// the OpenRouter wire; direct upstreams (Fireworks/DeepInfra/Bedrock)
// reject them. Empty TargetProvider falls back to the model-slug match
// so the historical single-binding behavior keeps working for callers
// that haven't been plumbed through yet (the handover summarizer).
func targetIsOpenRouter(opts EmitOptions) bool {
	if opts.TargetProvider != "" {
		return opts.TargetProvider == providers.ProviderOpenRouter
	}
	return true
}

func (e *RequestEnvelope) buildOpenAIFromAnthropic(opts EmitOptions) ([]byte, providers.RequestMutationStats, error) {
	var stats providers.RequestMutationStats
	body, removed, err := filterClaudeCodeOnlyToolsFromAnthropicBody(e.body)
	if err != nil {
		return nil, stats, fmt.Errorf("strip claude-code-only tools: %w", err)
	}
	stats.CCOnlyToolsStripped = removed
	body, _, err = applyExploreLoopReminderToAnthropicBody(body)
	if err != nil {
		return nil, stats, fmt.Errorf("inject explore-loop reminder: %w", err)
	}
	jw := newJSONWriter()
	jw.Obj()
	jw.Key("model")
	jw.Str(opts.TargetModel)

	// Stream
	if r := gjson.GetBytes(body, "stream"); r.Exists() {
		jw.Key("stream")
		jw.Raw(r.Raw)
	}

	// System + Messages
	writeOpenAISystemAndMessagesFromAnthropic(jw, body, opts)

	// Stop sequences
	if r := gjson.GetBytes(body, "stop_sequences"); r.Exists() {
		jw.Key("stop")
		jw.Raw(r.Raw)
	}

	// Tools
	writeOpenAIToolsFromAnthropic(jw, body)

	// Tool choice
	writeOpenAIToolChoiceFromAnthropic(jw, body)

	// Temperature, top_p
	clientSetTemp := false
	if r := gjson.GetBytes(body, "temperature"); r.Exists() {
		jw.Key("temperature")
		jw.Raw(r.Raw)
		clientSetTemp = true
	}
	if r := gjson.GetBytes(body, "top_p"); r.Exists() {
		jw.Key("top_p")
		jw.Raw(r.Raw)
	}

	// Tool temperature override for OpenRouter
	if !clientSetTemp && targetIsOpenRouter(opts) && openRouterForcesToolTemperatureZero(opts.TargetModel) {
		if hasNonEmptyTools(body) {
			jw.Key("temperature")
			jw.Int(0)
		}
	}

	// Max tokens
	writeOpenAIMaxTokensFromAnthropic(jw, body, opts)

	// Stream usage option
	if opts.IncludeStreamUsage && gjson.GetBytes(body, "stream").Bool() {
		jw.Key("stream_options")
		jw.Obj()
		jw.Key("include_usage")
		jw.Bool(true)
		jw.EndObj()
	}

	// OpenRouter hints
	if targetIsOpenRouter(opts) {
		if hint := openRouterProviderHint(opts.TargetModel); hint != nil {
			if hintBytes, err := json.Marshal(hint); err == nil {
				jw.Key("provider")
				jw.RawBytes(hintBytes)
			}
		}
		if reasoning := openRouterReasoningHint(opts.TargetModel); reasoning != nil {
			if reasoningBytes, err := json.Marshal(reasoning); err == nil {
				jw.Key("reasoning")
				jw.RawBytes(reasoningBytes)
			}
		}
	}

	jw.EndObj()
	body, err = applyQwen3SamplersIfNeeded(jw.Bytes(), opts.TargetModel)
	if err != nil {
		return nil, stats, err
	}
	body, err = applyGLM51FlagsIfNeeded(body, opts)
	if err != nil {
		return nil, stats, err
	}
	return body, stats, nil
}

// applyGLM51FlagsIfNeeded sets the request-body knobs GLM-5.1 needs to behave
// correctly on tool-heavy turns. Two knobs, both gated on isGLM51:
//
//  1. tool_stream=true — opt-in flag per Z.AI docs that switches GLM-5.1 from
//     "emit tool envelope, send args as one late chunk (or never)" to proper
//     incremental argument streaming. Without it, GLM-5.1 reproduces the
//     GLM-5 empty-input loop documented in
//     docs/investigations/2026-05-26-glm5-empty-tool-loop.md.
//
//  2. chat_template_kwargs.enable_thinking=false — DeepInfra serves GLM-5.1
//     on vLLM, which honors the Jinja template kwarg to disable thinking
//     mode. Default is on; we don't want reasoning blocks leaking into the
//     response stream. OpenRouter routes the same disable through its native
//     reasoning={enabled:false} hint (see openRouterReasoningHint), so the
//     chat_template_kwargs path only fires for non-OpenRouter targets.
//
// Both knobs respect client-set values so a caller forcing thinking-on or
// tool_stream-off still wins.
func applyGLM51FlagsIfNeeded(body []byte, opts EmitOptions) ([]byte, error) {
	if !isGLM51(opts.TargetModel) {
		return body, nil
	}
	if !gjson.GetBytes(body, "tool_stream").Exists() {
		out, err := sjson.SetBytes(body, "tool_stream", true)
		if err != nil {
			return nil, fmt.Errorf("set glm-5.1 tool_stream: %w", err)
		}
		body = out
	}
	if !targetIsOpenRouter(opts) && !gjson.GetBytes(body, "chat_template_kwargs.enable_thinking").Exists() {
		out, err := sjson.SetBytes(body, "chat_template_kwargs.enable_thinking", false)
		if err != nil {
			return nil, fmt.Errorf("set glm-5.1 chat_template_kwargs.enable_thinking: %w", err)
		}
		body = out
	}
	return body, nil
}

// applyQwen3SamplersIfNeeded layers the Qwen3 model-card sampling defaults onto
// the outbound body when the target is a qwen3-family model and the client did
// not set them. Applied across all OpenAI-compat providers (OpenRouter, Bedrock,
// DeepInfra, Fireworks) — the recommendation is model-keyed, not provider-keyed,
// and Qwen3 only routes to OpenAI-compat backends so unknown-field rejection
// (e.g. OpenAI native strict mode) is not in scope.
func applyQwen3SamplersIfNeeded(body []byte, model string) ([]byte, error) {
	if !isQwen3Family(model) {
		return body, nil
	}
	type sampler struct {
		key string
		val float64
	}
	defaults := []sampler{
		{"temperature", qwen3Temperature},
		{"top_p", qwen3TopP},
		{"presence_penalty", qwen3PresencePenalty},
		{"repetition_penalty", qwen3RepetitionPenalty},
	}
	for _, s := range defaults {
		if gjson.GetBytes(body, s.key).Exists() {
			continue
		}
		out, err := sjson.SetBytes(body, s.key, s.val)
		if err != nil {
			return nil, fmt.Errorf("set qwen3 %s: %w", s.key, err)
		}
		body = out
	}
	return body, nil
}

// writeOpenAISystemAndMessagesFromAnthropic emits the "messages" key into jw by
// converting the Anthropic system field and messages array to OpenAI format.
func writeOpenAISystemAndMessagesFromAnthropic(jw *jsonWriter, body []byte, opts EmitOptions) {
	systemText := flattenAnthropicSystemGJSON(gjson.GetBytes(body, "system"))
	if targetIsOpenRouter(opts) && hasNonEmptyTools(body) {
		if reminder := openRouterSystemReminder(opts.TargetModel); reminder != "" {
			if systemText == "" {
				systemText = reminder
			} else {
				systemText = systemText + "\n\n" + reminder
			}
		}
	}

	jw.Key("messages")
	jw.Arr()

	if systemText != "" {
		jw.Obj()
		jw.Key("role")
		jw.Str("system")
		jw.Key("content")
		jw.Str(systemText)
		jw.EndObj()
	}

	gjson.GetBytes(body, "messages").ForEach(func(_, msg gjson.Result) bool {
		role := msg.Get("role").String()
		switch role {
		case "assistant":
			writeOpenAIAssistantFromAnthropic(jw, msg)
		default:
			writeOpenAIUserFromAnthropic(jw, msg)
		}
		return true
	})

	jw.EndArr()
}

// flattenAnthropicSystemGJSON converts the Anthropic system field (string or
// array of text blocks) to a single plain string, stripping the billing header.
func flattenAnthropicSystemGJSON(system gjson.Result) string {
	switch system.Type {
	case gjson.String:
		return stripAnthropicBillingHeader(system.String())
	case gjson.JSON:
		if !system.IsArray() {
			return ""
		}
		var parts []string
		system.ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() == "text" {
				if text := block.Get("text").String(); text != "" {
					if stripped := stripAnthropicBillingHeader(text); stripped != "" {
						parts = append(parts, stripped)
					}
				}
			}
			return true
		})
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// writeOpenAIAssistantFromAnthropic emits an OpenAI assistant message from an
// Anthropic assistant message.
func writeOpenAIAssistantFromAnthropic(jw *jsonWriter, msg gjson.Result) {
	jw.Obj()
	jw.Key("role")
	jw.Str("assistant")

	content := msg.Get("content")
	switch content.Type {
	case gjson.String:
		jw.Key("content")
		jw.Raw(content.Raw)
	case gjson.JSON:
		if !content.IsArray() {
			jw.Key("content")
			jw.Null()
			jw.EndObj()
			return
		}
		var textParts []string
		var toolCallRaws []string

		content.ForEach(func(_, block gjson.Result) bool {
			switch block.Get("type").String() {
			case "text":
				if text := block.Get("text").String(); text != "" {
					textParts = append(textParts, text)
				}
			case "tool_use":
				toolCallRaws = append(toolCallRaws, buildOpenAIToolCall(block))
			}
			return true
		})

		if len(textParts) > 0 {
			jw.Key("content")
			jw.Str(strings.Join(textParts, "\n"))
		} else {
			jw.Key("content")
			jw.Null()
		}
		if len(toolCallRaws) > 0 {
			jw.Key("tool_calls")
			jw.Arr()
			for _, raw := range toolCallRaws {
				jw.Raw(raw)
			}
			jw.EndArr()
		}
	default:
		jw.Key("content")
		jw.Null()
	}

	jw.EndObj()
}

// buildOpenAIToolCall serializes an Anthropic tool_use block as an OpenAI tool
// call JSON string (returned as raw JSON for direct embedding).
func buildOpenAIToolCall(block gjson.Result) string {
	inner := newJSONWriter()
	inner.Obj()
	inner.Key("id")
	inner.Str(block.Get("id").String())
	inner.Key("type")
	inner.Str("function")
	inner.Key("function")
	inner.Obj()
	inner.Key("name")
	inner.Str(block.Get("name").String())
	// input is a JSON object; encode it as a JSON string (arguments field).
	inputRaw := block.Get("input").Raw
	if inputRaw == "" {
		inputRaw = "{}"
	}
	inner.Key("arguments")
	inner.Str(inputRaw)
	inner.EndObj()
	inner.EndObj()
	return string(inner.Bytes())
}

// writeOpenAIUserFromAnthropic emits zero or more OpenAI messages for an
// Anthropic user message. Mixed content (tool_result + text + image) is split
// into separate tool-role messages followed by a single user message.
func writeOpenAIUserFromAnthropic(jw *jsonWriter, msg gjson.Result) {
	content := msg.Get("content")
	switch content.Type {
	case gjson.String:
		jw.Obj()
		jw.Key("role")
		jw.Str("user")
		jw.Key("content")
		jw.Raw(content.Raw)
		jw.EndObj()
		return
	case gjson.JSON:
		if !content.IsArray() {
			return
		}
	default:
		return
	}

	// Separate tool_result blocks from user content blocks.
	var toolResultRaws []string
	var userPartRaws []string

	content.ForEach(func(_, block gjson.Result) bool {
		switch block.Get("type").String() {
		case "tool_result":
			toolResultRaws = append(toolResultRaws, buildOpenAIToolResultMessage(block))
		case "text":
			inner := newJSONWriter()
			inner.Obj()
			inner.Key("type")
			inner.Str("text")
			inner.Key("text")
			inner.Str(block.Get("text").String())
			inner.EndObj()
			userPartRaws = append(userPartRaws, string(inner.Bytes()))
		case "image":
			if part := buildOpenAIImagePart(block); part != "" {
				userPartRaws = append(userPartRaws, part)
			}
		}
		return true
	})

	for _, raw := range toolResultRaws {
		jw.Raw(raw)
	}

	if len(userPartRaws) == 0 {
		return
	}

	// Single plain text part: emit as string content.
	if len(userPartRaws) == 1 {
		p := gjson.Parse(userPartRaws[0])
		if p.Get("type").String() == "text" {
			jw.Obj()
			jw.Key("role")
			jw.Str("user")
			jw.Key("content")
			jw.Str(p.Get("text").String())
			jw.EndObj()
			return
		}
	}

	jw.Obj()
	jw.Key("role")
	jw.Str("user")
	jw.Key("content")
	jw.Arr()
	for _, raw := range userPartRaws {
		jw.Raw(raw)
	}
	jw.EndArr()
	jw.EndObj()
}

// buildOpenAIToolResultMessage converts an Anthropic tool_result block to an
// OpenAI role:tool message JSON string.
func buildOpenAIToolResultMessage(block gjson.Result) string {
	inner := newJSONWriter()
	inner.Obj()
	inner.Key("role")
	inner.Str("tool")
	inner.Key("tool_call_id")
	inner.Str(block.Get("tool_use_id").String())
	inner.Key("content")
	inner.Str(toolResultContentGJSON(block.Get("content")))
	inner.EndObj()
	return string(inner.Bytes())
}

// buildOpenAIImagePart converts an Anthropic image block to an OpenAI
// image_url content-part JSON string. Returns "" if the block is malformed.
func buildOpenAIImagePart(block gjson.Result) string {
	src := block.Get("source")
	if !src.Exists() {
		return ""
	}
	inner := newJSONWriter()
	inner.Obj()
	inner.Key("type")
	inner.Str("image_url")
	inner.Key("image_url")
	inner.Obj()
	inner.Key("url")
	switch src.Get("type").String() {
	case "base64":
		mediaType := src.Get("media_type").String()
		if mediaType == "" {
			mediaType = "image/jpeg"
		}
		data := src.Get("data").String()
		if data == "" {
			return ""
		}
		inner.Str("data:" + mediaType + ";base64," + data)
	case "url":
		urlStr := src.Get("url").String()
		if urlStr == "" {
			return ""
		}
		inner.Str(urlStr)
	default:
		return ""
	}
	inner.EndObj()
	inner.EndObj()
	return string(inner.Bytes())
}

const openAIMaxTools = 128

// writeOpenAIToolsFromAnthropic emits the "tools" key into jw by converting
// Anthropic tool definitions to OpenAI function-calling format.
func writeOpenAIToolsFromAnthropic(jw *jsonWriter, body []byte) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() || tools.Get("#").Int() == 0 {
		return
	}

	jw.Key("tools")
	jw.Arr()
	count := 0
	tools.ForEach(func(_, tool gjson.Result) bool {
		if count >= openAIMaxTools {
			return false
		}
		count++
		var params any
		if schema := tool.Get("input_schema"); schema.Exists() {
			_ = json.Unmarshal([]byte(schema.Raw), &params)
			params = inlineSchemaDefs(params)
			sanitizeOpenAIToolSchema(params)
		}
		jw.Obj()
		jw.Key("type")
		jw.Str("function")
		jw.Key("function")
		jw.Obj()
		jw.Key("name")
		jw.Str(tool.Get("name").String())
		if desc := tool.Get("description"); desc.Exists() {
			jw.Key("description")
			jw.Raw(desc.Raw)
		}
		if params != nil {
			if paramBytes, err := json.Marshal(params); err == nil {
				jw.Key("parameters")
				jw.RawBytes(paramBytes)
			}
		}
		jw.EndObj()
		jw.EndObj()
		return true
	})
	jw.EndArr()
}

// writeOpenAIToolChoiceFromAnthropic emits the "tool_choice" key into jw by
// converting the Anthropic tool_choice field to OpenAI format.
func writeOpenAIToolChoiceFromAnthropic(jw *jsonWriter, body []byte) {
	r := gjson.GetBytes(body, "tool_choice")
	if !r.Exists() || !r.IsObject() {
		return
	}
	switch r.Get("type").String() {
	case "auto":
		jw.Key("tool_choice")
		jw.Str("auto")
	case "any":
		jw.Key("tool_choice")
		jw.Str("required")
	case "tool":
		nameRes := r.Get("name")
		if nameRes.Type != gjson.String {
			return
		}
		name := nameRes.String()
		if name == "" {
			return
		}
		inner := newJSONWriter()
		inner.Obj()
		inner.Key("type")
		inner.Str("function")
		inner.Key("function")
		inner.Obj()
		inner.Key("name")
		inner.Str(name)
		inner.EndObj()
		inner.EndObj()
		jw.Key("tool_choice")
		jw.RawBytes(inner.Bytes())
	}
}

// writeOpenAIMaxTokensFromAnthropic emits either "max_tokens" or
// "max_completion_tokens" (for reasoning-capable models), clamped to the
// model's output-token cap.
func writeOpenAIMaxTokensFromAnthropic(jw *jsonWriter, body []byte, opts EmitOptions) {
	r := gjson.GetBytes(body, "max_tokens")
	val := defaultOutputTokens(opts.TargetModel)
	if r.Exists() {
		val = r.Int()
	}
	cap := modelMaxOutputTokens[opts.TargetModel]
	if cap == 0 {
		cap = defaultMaxOutputTokenCap
	}
	if val > int64(cap) {
		val = int64(cap)
	}
	if opts.Capabilities.Supports(router.CapReasoning) {
		jw.Key("max_completion_tokens")
	} else {
		jw.Key("max_tokens")
	}
	jw.Int(val)
}

// inlineSchemaDefs replaces "$ref" pointers to "#/$defs/<name>" or
// "#/definitions/<name>" with a deep copy of the referenced definition, then
// strips the defs maps from the schema. Some OpenAI-compatible upstreams
// (notably Fireworks) do not dereference $ref themselves and return a 400
// ("Error resolving schema reference '#/$defs/X': AttributeError('NoneType'
// object has no attribute 'lookup')") on tool schemas that use $defs. Inlining
// before forwarding keeps the schema self-contained so it works on every
// OpenAI-compat backend regardless of how its validator handles $ref.
//
// A $ref with sibling keys (e.g. {"$ref": "#/$defs/X", "description": "..."},
// emitted by Pydantic v2 / OpenAPI 3.1 / many MCP servers including Intuit
// QuickBooks) is resolved the same way: per JSON Schema Draft 7, siblings to
// $ref are ignored, so dropping them on substitution is spec-compliant and
// keeps the upstream from seeing an unresolvable ref after $defs is stripped.
//
// Cyclic refs are left intact (no infinite recursion); unresolvable refs are
// left intact so the upstream can surface its own clearer error.
func inlineSchemaDefs(node any) any {
	root, ok := node.(map[string]any)
	if !ok {
		return node
	}
	defs := map[string]any{}
	for _, key := range []string{"$defs", "definitions"} {
		d, ok := root[key].(map[string]any)
		if !ok {
			continue
		}
		for name, v := range d {
			defs[key+"/"+name] = v
		}
	}
	if len(defs) == 0 {
		return node
	}
	resolved, _ := resolveSchemaRefs(node, defs, map[string]struct{}{}).(map[string]any)
	delete(resolved, "$defs")
	delete(resolved, "definitions")
	return resolved
}

func resolveSchemaRefs(node any, defs map[string]any, visited map[string]struct{}) any {
	switch v := node.(type) {
	case map[string]any:
		if ref, ok := v["$ref"].(string); ok {
			name := strings.TrimPrefix(ref, "#/")
			if _, cycle := visited[name]; cycle {
				return v
			}
			target, ok := defs[name]
			if !ok {
				return v
			}
			visited[name] = struct{}{}
			out := resolveSchemaRefs(deepCopyJSON(target), defs, visited)
			delete(visited, name)
			return out
		}
		out := make(map[string]any, len(v))
		for k, child := range v {
			out[k] = resolveSchemaRefs(child, defs, visited)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			out[i] = resolveSchemaRefs(child, defs, visited)
		}
		return out
	default:
		return v
	}
}

func deepCopyJSON(node any) any {
	switch v := node.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, c := range v {
			out[k] = deepCopyJSON(c)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, c := range v {
			out[i] = deepCopyJSON(c)
		}
		return out
	default:
		return v
	}
}

func sanitizeOpenAIToolSchema(node any) {
	m, ok := node.(map[string]any)
	if !ok {
		return
	}
	if t, _ := m["type"].(string); t == "array" {
		if _, hasItems := m["items"]; !hasItems {
			m["items"] = map[string]any{}
		}
	}
	if props, ok := m["properties"].(map[string]any); ok {
		for _, v := range props {
			sanitizeOpenAIToolSchema(v)
		}
	}
	sanitizeOpenAIToolSchema(m["items"])
	sanitizeOpenAIToolSchema(m["additionalProperties"])
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if arr, ok := m[key].([]any); ok {
			for _, v := range arr {
				sanitizeOpenAIToolSchema(v)
			}
		}
	}
	sanitizeOpenAIToolSchema(m["not"])
}
