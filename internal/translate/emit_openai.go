package translate

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// PrepareOpenAI builds an OpenAI Chat Completions request body.
func (e *RequestEnvelope) PrepareOpenAI(in http.Header, opts EmitOptions) (providers.PreparedRequest, error) {
	var body []byte
	var err error
	switch e.format {
	case FormatOpenAI:
		body, err = e.buildOpenAIFromOpenAI(opts)
	case FormatAnthropic:
		body, err = e.buildOpenAIFromAnthropic(opts)
	default:
		return providers.PreparedRequest{}, fmt.Errorf("unsupported source format for OpenAI emit: %d", e.format)
	}
	if err != nil {
		return providers.PreparedRequest{}, err
	}
	return providers.PreparedRequest{Body: body, Headers: make(http.Header)}, nil
}

func (e *RequestEnvelope) buildOpenAIFromOpenAI(opts EmitOptions) ([]byte, error) {
	ov := resolveOpenAIOverrides(e.body, opts)
	body, err := e.emitSameFormat(ov)
	if err != nil {
		return nil, err
	}
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
	if reminder := openRouterSystemReminder(opts.TargetModel); reminder != "" && gjson.GetBytes(body, "tools").Exists() {
		body, err = applySystemReminderToBody(body, reminder)
		if err != nil {
			return nil, fmt.Errorf("set system reminder: %w", err)
		}
	}
	if openRouterForcesToolTemperatureZero(opts.TargetModel) &&
		gjson.GetBytes(body, "tools").Exists() &&
		!gjson.GetBytes(body, "temperature").Exists() {
		body, err = sjson.SetBytes(body, "temperature", 0)
		if err != nil {
			return nil, fmt.Errorf("set tool temperature override: %w", err)
		}
	}
	if openRouterStrictTools(opts.TargetModel) && gjson.GetBytes(body, "tools").Exists() {
		body, err = applyStrictToolsToBody(body)
		if err != nil {
			return nil, fmt.Errorf("apply strict tools: %w", err)
		}
	}
	return body, nil
}

func (e *RequestEnvelope) buildOpenAIFromAnthropic(opts EmitOptions) ([]byte, error) {
	out := make(map[string]any)
	out["model"] = opts.TargetModel

	if r := gjson.GetBytes(e.body, "stream"); r.Exists() {
		out["stream"] = r.Value()
	}

	if err := e.pullAnthropicSystemAndMessages(out); err != nil {
		return nil, err
	}
	pullAnthropicStopSequences(e.body, out)
	if err := e.pullAnthropicTools(out, openRouterStrictTools(opts.TargetModel)); err != nil {
		return nil, err
	}
	pullAnthropicToolChoice(e.body, out)

	clientSetTemp := false
	for _, key := range []string{"temperature", "top_p"} {
		if r := gjson.GetBytes(e.body, key); r.Exists() {
			out[key] = r.Value()
			if key == "temperature" {
				clientSetTemp = true
			}
		}
	}
	if !clientSetTemp && openRouterForcesToolTemperatureZero(opts.TargetModel) {
		if _, hasTools := out["tools"]; hasTools {
			out["temperature"] = 0
		}
	}
	if r := gjson.GetBytes(e.body, "max_tokens"); r.Exists() {
		out["max_tokens"] = r.Value()
	} else {
		out["max_tokens"] = defaultOutputTokens(opts.TargetModel)
	}

	if mt, ok := out["max_tokens"]; ok && opts.Capabilities.Supports(router.CapReasoning) {
		if _, alreadySet := out["max_completion_tokens"]; !alreadySet {
			out["max_completion_tokens"] = mt
		}
		delete(out, "max_tokens")
	}

	clampOutputTokens(out, opts.TargetModel)
	if opts.IncludeStreamUsage {
		injectStreamUsageOption(out)
	}

	if hint := openRouterProviderHint(opts.TargetModel); hint != nil {
		out["provider"] = hint
	}
	if reasoning := openRouterReasoningHint(opts.TargetModel); reasoning != nil {
		out["reasoning"] = reasoning
	}
	if reminder := openRouterSystemReminder(opts.TargetModel); reminder != "" {
		if _, hasTools := out["tools"]; hasTools {
			msgs, _ := out["messages"].([]any)
			out["messages"] = injectSystemReminder(msgs, reminder)
		}
	}

	return json.Marshal(out)
}

func injectStreamUsageOption(doc map[string]any) {
	if stream, _ := doc["stream"].(bool); !stream {
		return
	}
	src, _ := doc["stream_options"].(map[string]any)
	so := shallowClone(src)
	so["include_usage"] = true
	doc["stream_options"] = so
}

func flattenAnthropicSystem(system any) map[string]any {
	switch s := system.(type) {
	case string:
		s = stripAnthropicBillingHeader(s)
		if s == "" {
			return nil
		}
		return map[string]any{"role": "system", "content": s}
	case []any:
		var parts []string
		for _, b := range s {
			block, _ := b.(map[string]any)
			if block == nil {
				continue
			}
			if t, _ := block["type"].(string); t == "text" {
				if text, _ := block["text"].(string); text != "" {
					if stripped := stripAnthropicBillingHeader(text); stripped != "" {
						parts = append(parts, stripped)
					}
				}
			}
		}
		if len(parts) == 0 {
			return nil
		}
		return map[string]any{"role": "system", "content": strings.Join(parts, "\n")}
	default:
		return nil
	}
}

func anthropicMessagesToOpenAI(systemMsg map[string]any, msgs []any) []any {
	var out []any
	if systemMsg != nil {
		out = append(out, systemMsg)
	}
	for _, raw := range msgs {
		msg, _ := raw.(map[string]any)
		if msg == nil {
			continue
		}
		role, _ := msg["role"].(string)
		switch role {
		case "assistant":
			out = append(out, anthropicAssistantToOpenAI(msg))
		default:
			out = append(out, anthropicUserToOpenAI(msg)...)
		}
	}
	return out
}

func anthropicAssistantToOpenAI(msg map[string]any) map[string]any {
	out := map[string]any{"role": "assistant"}
	switch c := msg["content"].(type) {
	case string:
		out["content"] = c
		return out
	case []any:
		var textParts []string
		var toolCalls []any
		for _, b := range c {
			block, _ := b.(map[string]any)
			if block == nil {
				continue
			}
			switch t, _ := block["type"].(string); t {
			case "text":
				if text, _ := block["text"].(string); text != "" {
					textParts = append(textParts, text)
				}
			case "tool_use":
				id, _ := block["id"].(string)
				name, _ := block["name"].(string)
				args, _ := json.Marshal(block["input"])
				toolCalls = append(toolCalls, map[string]any{
					"id":   id,
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": string(args),
					},
				})
			}
		}
		if len(textParts) > 0 {
			out["content"] = strings.Join(textParts, "\n")
		} else {
			out["content"] = nil
		}
		if len(toolCalls) > 0 {
			out["tool_calls"] = toolCalls
		}
	default:
		out["content"] = nil
	}
	return out
}

func anthropicUserToOpenAI(msg map[string]any) []any {
	switch c := msg["content"].(type) {
	case string:
		return []any{map[string]any{"role": "user", "content": c}}
	case []any:
		var toolMsgs []any
		var userParts []any
		for _, b := range c {
			block, _ := b.(map[string]any)
			if block == nil {
				continue
			}
			switch t, _ := block["type"].(string); t {
			case "tool_result":
				toolMsgs = append(toolMsgs, anthropicToolResultToOpenAI(block))
			case "text":
				userParts = append(userParts, map[string]any{
					"type": "text",
					"text": block["text"],
				})
			case "image":
				if part := anthropicImageToOpenAI(block); part != nil {
					userParts = append(userParts, part)
				}
			}
		}
		out := append([]any{}, toolMsgs...)
		if len(userParts) == 1 {
			if first, _ := userParts[0].(map[string]any); first != nil {
				if t, _ := first["type"].(string); t == "text" {
					out = append(out, map[string]any{"role": "user", "content": first["text"]})
					return out
				}
			}
		}
		if len(userParts) > 0 {
			out = append(out, map[string]any{"role": "user", "content": userParts})
		}
		return out
	default:
		return nil
	}
}

func anthropicToolResultToOpenAI(block map[string]any) map[string]any {
	id, _ := block["tool_use_id"].(string)
	return map[string]any{
		"role":         "tool",
		"tool_call_id": id,
		"content":      toolResultContent(block["content"]),
	}
}

func anthropicImageToOpenAI(block map[string]any) map[string]any {
	src, _ := block["source"].(map[string]any)
	if src == nil {
		return nil
	}
	switch t, _ := src["type"].(string); t {
	case "base64":
		mediaType, _ := src["media_type"].(string)
		data, _ := src["data"].(string)
		if data == "" {
			return nil
		}
		if mediaType == "" {
			mediaType = "image/jpeg"
		}
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": fmt.Sprintf("data:%s;base64,%s", mediaType, data)},
		}
	case "url":
		urlStr, _ := src["url"].(string)
		if urlStr == "" {
			return nil
		}
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": urlStr},
		}
	}
	return nil
}

func (e *RequestEnvelope) pullAnthropicSystemAndMessages(out map[string]any) error {
	src, err := e.ensureSrc()
	if err != nil {
		return err
	}
	systemMsg := flattenAnthropicSystem(src["system"])
	if msgs, ok := src["messages"].([]any); ok {
		out["messages"] = anthropicMessagesToOpenAI(systemMsg, msgs)
	} else if systemMsg != nil {
		out["messages"] = []any{systemMsg}
	}
	return nil
}

const openAIMaxTools = 128

func (e *RequestEnvelope) pullAnthropicTools(out map[string]any, strict bool) error {
	src, err := e.ensureSrc()
	if err != nil {
		return err
	}
	tools, ok := src["tools"].([]any)
	if !ok || len(tools) == 0 {
		return nil
	}
	var result []any
	for _, t := range tools {
		tool, _ := t.(map[string]any)
		if tool == nil {
			continue
		}
		params := tool["input_schema"]
		sanitizeOpenAIToolSchema(params)
		fn := map[string]any{
			"name":        tool["name"],
			"description": tool["description"],
			"parameters":  params,
		}
		if strict {
			applyStrictModeToParams(params)
			fn["strict"] = true
		}
		result = append(result, map[string]any{"type": "function", "function": fn})
	}
	if len(result) > openAIMaxTools {
		result = result[:openAIMaxTools]
	}
	out["tools"] = result
	return nil
}

// applyStrictModeToParams mutates a JSON Schema so it satisfies OpenAI's
// strict-mode tool-call requirements: every `type: "object"` schema gets
// `additionalProperties: false` and every property is listed in `required`.
// Properties not in the source `required` set are marked nullable via a type
// union so the model can still pass null to preserve "optional" semantics.
func applyStrictModeToParams(node any) {
	m, ok := node.(map[string]any)
	if !ok {
		return
	}
	if isObjectType(m["type"]) {
		if props, ok := m["properties"].(map[string]any); ok && len(props) > 0 {
			m["additionalProperties"] = false
			existing := map[string]struct{}{}
			if reqArr, ok := m["required"].([]any); ok {
				for _, r := range reqArr {
					if s, ok := r.(string); ok {
						existing[s] = struct{}{}
					}
				}
			}
			names := make([]string, 0, len(props))
			for name := range props {
				names = append(names, name)
			}
			sort.Strings(names)
			required := make([]any, 0, len(names))
			for _, name := range names {
				required = append(required, name)
				if _, wasRequired := existing[name]; !wasRequired {
					makeNullable(props[name])
				}
			}
			m["required"] = required
		} else {
			// Empty object subschemas ({type: object} with no properties).
			// DeepSeek's tool-schema validator rejects every shape of
			// "no-property object" we can otherwise produce:
			//   - `additionalProperties: false`   → "An object with no properties is not allowed"
			//   - `additionalProperties: true`    → same error (the literal absence of properties is what's rejected)
			//   - `additionalProperties: {schema}` → "invalid type: map, expected a boolean"
			//   - `properties: {}` (empty map)    → same "no properties" error
			//
			// The only way past the validator is to actually have a
			// property. We inject a single nullable string placeholder; the
			// model is expected to pass `{"_reserved": null}` for what was
			// originally an "any object" field. Trade-off: a source schema
			// that genuinely wanted to accept arbitrary content (rare —
			// only some MCP-server tools) loses that affordance against
			// DeepSeek. The alternative is a 400 on the whole request.
			//
			// Only runs for DeepSeek targets (gated on
			// openRouterStrictTools), so OpenAI / OpenRouter / Anthropic
			// targets keep their unmodified empty-object semantics.
			m["properties"] = map[string]any{
				"_reserved": map[string]any{
					"type":        []any{"string", "null"},
					"description": "Reserved placeholder. The source schema accepted any object here; DeepSeek's validator rejects schemas with no declared properties, so pass null.",
				},
			}
			m["required"] = []any{"_reserved"}
			m["additionalProperties"] = false
		}
	}
	// Belt-and-suspenders normalization for non-object schemas that carry
	// a stray `additionalProperties: <schema>` (uncommon but valid JSON
	// Schema noise). The strict-mode object-type branch above doesn't reach
	// them, and DeepSeek's parser requires `additionalProperties` to be a
	// boolean everywhere it appears.
	if _, isMap := m["additionalProperties"].(map[string]any); isMap {
		m["additionalProperties"] = true
	}
	if props, ok := m["properties"].(map[string]any); ok {
		for _, v := range props {
			applyStrictModeToParams(v)
		}
	}
	applyStrictModeToParams(m["items"])
	for _, key := range []string{"$defs", "definitions"} {
		if defs, ok := m[key].(map[string]any); ok {
			for _, v := range defs {
				applyStrictModeToParams(v)
			}
		}
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if arr, ok := m[key].([]any); ok {
			for _, v := range arr {
				applyStrictModeToParams(v)
			}
		}
	}
}

// isObjectType reports whether a JSON Schema `type` value is "object", either
// as a bare string or as part of a union like ["object", "null"]. The router's
// own strict-mode pass no longer produces `["object", "null"]` (see
// makeNullable — DeepSeek rejects it), but a hand-written source schema can
// still arrive in that shape, so we keep the union check for defensive
// coverage on the recursion's "is this still an object subtree?" question.
func isObjectType(typ any) bool {
	switch t := typ.(type) {
	case string:
		return t == "object"
	case []any:
		for _, v := range t {
			if s, _ := v.(string); s == "object" {
				return true
			}
		}
	}
	return false
}

// applyStrictToolsToBody mutates the `tools` array of a serialized OpenAI body
// to add `function.strict = true` and tighten each parameter schema. Best-effort:
// returns the input unchanged when `tools` cannot be parsed rather than failing
// the request.
func applyStrictToolsToBody(body []byte) ([]byte, error) {
	toolsResult := gjson.GetBytes(body, "tools")
	if !toolsResult.IsArray() {
		return body, nil
	}
	var tools []any
	if err := json.Unmarshal([]byte(toolsResult.Raw), &tools); err != nil {
		return body, nil
	}
	for _, t := range tools {
		tool, _ := t.(map[string]any)
		if tool == nil {
			continue
		}
		fn, _ := tool["function"].(map[string]any)
		if fn == nil {
			continue
		}
		applyStrictModeToParams(fn["parameters"])
		fn["strict"] = true
	}
	return sjson.SetBytes(body, "tools", tools)
}

// makeNullable adds "null" to a schema's `type` so strict mode can still send
// a null value for what was previously an optional property. No-op when the
// schema lacks a `type` keyword or already permits null.
//
// "object" and "array" are deliberately skipped: DeepSeek's tool-schema
// parser only accepts string|number|integer|boolean|null as members of a
// `type` array, so emitting `["object", "null"]` 400s upstream even though
// the union is standard JSON Schema. Strict mode forces every property
// into `required` anyway, so the model has to construct the value to
// invoke the tool either way — dropping the nullable affordance for these
// types removes a path the model can't usefully take.
func makeNullable(node any) {
	m, ok := node.(map[string]any)
	if !ok {
		return
	}
	switch t := m["type"].(type) {
	case string:
		if t == "null" || t == "object" || t == "array" {
			return
		}
		m["type"] = []any{t, "null"}
	case []any:
		for _, v := range t {
			if s, _ := v.(string); s == "null" {
				return
			}
		}
		// If a hand-written source schema already declared a type union
		// containing "object" or "array", DeepSeek would reject it on its
		// own merits — adding "null" doesn't worsen that, but adding it to
		// a union that's already DeepSeek-incompatible has no upside. Leave
		// it alone so the error message points at the source field, not at
		// the strict-mode pass.
		for _, v := range t {
			if s, _ := v.(string); s == "object" || s == "array" {
				return
			}
		}
		m["type"] = append(t, "null")
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

func pullAnthropicStopSequences(body []byte, out map[string]any) {
	r := gjson.GetBytes(body, "stop_sequences")
	if !r.Exists() {
		return
	}
	out["stop"] = r.Value()
}

func pullAnthropicToolChoice(body []byte, out map[string]any) {
	r := gjson.GetBytes(body, "tool_choice")
	if !r.Exists() || !r.IsObject() {
		return
	}
	switch r.Get("type").String() {
	case "auto":
		out["tool_choice"] = "auto"
	case "any":
		out["tool_choice"] = "required"
	case "tool":
		nameRes := r.Get("name")
		if nameRes.Type != gjson.String {
			return
		}
		name := nameRes.String()
		if name != "" {
			out["tool_choice"] = map[string]any{
				"type":     "function",
				"function": map[string]any{"name": name},
			}
		}
	}
}
