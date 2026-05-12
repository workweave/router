package translate

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"workweave/router/internal/providers"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// PrepareGemini builds a fully-prepared request body for Google Gemini's native
// REST surface (POST /v1beta/models/{model}:generateContent). Required for
// multi-turn tool use against Gemini 3.x preview models, which need the opaque
// thought_signature field round-tripped — Gemini's OpenAI-compat surface does
// not return that field.
//
// Source format may be OpenAI (Chat Completions wire) or Anthropic Messages.
// In both cases we walk the parsed body map (e.ensureSrc) and write a fresh
// Gemini native body.
func (e *RequestEnvelope) PrepareGemini(_ http.Header, opts EmitOptions) (providers.PreparedRequest, error) {
	// Same-format passthrough: a Gemini-source envelope already carries
	// canonical Gemini wire-format bytes. The synthetic top-level "model"
	// and "stream" fields injected by the Gemini handler get stripped here
	// by sjson before forwarding, since neither belongs in the upstream
	// generateContent body.
	if e.format == FormatGemini {
		body := e.body
		var err error
		body, err = sjson.DeleteBytes(body, "model")
		if err != nil {
			return providers.PreparedRequest{}, fmt.Errorf("strip model field: %w", err)
		}
		body, err = sjson.DeleteBytes(body, "stream")
		if err != nil {
			return providers.PreparedRequest{}, fmt.Errorf("strip stream field: %w", err)
		}
		headers := make(http.Header)
		if e.Stream() {
			headers.Set(GeminiStreamHintHeader, "true")
		}
		return providers.PreparedRequest{Body: body, Headers: headers}, nil
	}

	src, err := e.ensureSrc()
	if err != nil {
		return providers.PreparedRequest{}, err
	}

	out := make(map[string]any)

	switch e.format {
	case FormatOpenAI:
		if err := pullOpenAISystemAndContents(src, out); err != nil {
			return providers.PreparedRequest{}, err
		}
		if err := pullOpenAIToolsToGemini(src, out); err != nil {
			return providers.PreparedRequest{}, err
		}
		pullOpenAIToolChoiceToGemini(e.body, out)
		pullOpenAIGenerationConfig(e.body, out, opts.TargetModel)
	case FormatAnthropic:
		pullAnthropicSystemToGemini(src, out)
		if err := pullAnthropicContentsToGemini(src, out); err != nil {
			return providers.PreparedRequest{}, err
		}
		if err := pullAnthropicToolsToGemini(src, out); err != nil {
			return providers.PreparedRequest{}, err
		}
		pullAnthropicToolChoiceToGemini(e.body, out)
		pullAnthropicGenerationConfig(e.body, out, opts.TargetModel)
	default:
		return providers.PreparedRequest{}, fmt.Errorf("unsupported source format for Gemini emit: %d", e.format)
	}

	body, err := json.Marshal(out)
	if err != nil {
		return providers.PreparedRequest{}, fmt.Errorf("marshal gemini body: %w", err)
	}
	// Hint to the native client whether to hit :streamGenerateContent vs
	// :generateContent. Carried as a synthetic header on PreparedRequest so
	// the proxy.Service stays ignorant of provider-specific URL composition.
	headers := make(http.Header)
	if e.Stream() {
		headers.Set(GeminiStreamHintHeader, "true")
	}
	return providers.PreparedRequest{Body: body, Headers: headers}, nil
}

// GeminiStreamHintHeader is the synthetic header PrepareGemini sets when the
// inbound request asked for streaming. The google native client consumes it
// to choose between :generateContent and :streamGenerateContent and strips
// it before forwarding upstream.
const GeminiStreamHintHeader = "X-Router-Gemini-Stream"

// ----- OpenAI → Gemini -----

// pullOpenAISystemAndContents walks OpenAI messages, concatenating role:system
// messages into systemInstruction and converting the rest into Gemini contents.
func pullOpenAISystemAndContents(src map[string]any, out map[string]any) error {
	msgs, _ := src["messages"].([]any)
	if len(msgs) == 0 {
		return nil
	}

	// First pass: build a map of tool_call_id → function name so role:tool
	// messages can recover the function name for functionResponse.
	toolNames := openAICollectToolCallNames(msgs)

	var sysParts []string
	var contents []any
	for _, raw := range msgs {
		msg, _ := raw.(map[string]any)
		if msg == nil {
			continue
		}
		role, _ := msg["role"].(string)
		switch role {
		case "system":
			if s := openAITextContent(msg["content"]); s != "" {
				sysParts = append(sysParts, s)
			}
		case "user":
			parts := openAIUserToGeminiParts(msg["content"])
			if len(parts) > 0 {
				contents = append(contents, map[string]any{"role": "user", "parts": parts})
			}
		case "assistant":
			parts, err := openAIAssistantToGeminiParts(msg)
			if err != nil {
				return err
			}
			if len(parts) > 0 {
				contents = append(contents, map[string]any{"role": "model", "parts": parts})
			}
		case "tool":
			tcID, _ := msg["tool_call_id"].(string)
			name := toolNames[tcID]
			result := openAITextContent(msg["content"])
			contents = append(contents, map[string]any{
				"role":  "user",
				"parts": []any{map[string]any{"functionResponse": map[string]any{"name": name, "response": map[string]any{"result": result}}}},
			})
		}
	}

	if len(sysParts) > 0 {
		out["systemInstruction"] = map[string]any{
			"parts": []any{map[string]any{"text": strings.Join(sysParts, "\n")}},
		}
	}
	if len(contents) > 0 {
		out["contents"] = contents
	}
	return nil
}

func openAICollectToolCallNames(msgs []any) map[string]string {
	out := map[string]string{}
	for _, raw := range msgs {
		msg, _ := raw.(map[string]any)
		if msg == nil {
			continue
		}
		if r, _ := msg["role"].(string); r != "assistant" {
			continue
		}
		tcs, _ := msg["tool_calls"].([]any)
		for _, t := range tcs {
			tc, _ := t.(map[string]any)
			if tc == nil {
				continue
			}
			id, _ := tc["id"].(string)
			fn, _ := tc["function"].(map[string]any)
			name, _ := fn["name"].(string)
			if id != "" {
				out[id] = name
			}
		}
	}
	return out
}

// openAITextContent returns the textual content from an OpenAI message.content
// field, accepting both the bare-string and the array-of-parts shapes.
func openAITextContent(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			pm, _ := p.(map[string]any)
			if pm == nil {
				continue
			}
			if t, _ := pm["type"].(string); t == "text" {
				if s, _ := pm["text"].(string); s != "" {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// openAIUserToGeminiParts turns OpenAI user content into Gemini Parts (text +
// inlineData). data: URLs are decoded to inline base64 with the embedded
// mime type.
func openAIUserToGeminiParts(content any) []any {
	switch c := content.(type) {
	case string:
		if c == "" {
			return nil
		}
		return []any{map[string]any{"text": c}}
	case []any:
		var parts []any
		for _, p := range c {
			pm, _ := p.(map[string]any)
			if pm == nil {
				continue
			}
			switch t, _ := pm["type"].(string); t {
			case "text":
				if s, _ := pm["text"].(string); s != "" {
					parts = append(parts, map[string]any{"text": s})
				}
			case "image_url":
				img, _ := pm["image_url"].(map[string]any)
				url, _ := img["url"].(string)
				if part := dataURLToInlinePart(url); part != nil {
					parts = append(parts, part)
				}
			}
		}
		return parts
	}
	return nil
}

// dataURLToInlinePart parses a "data:<mime>;base64,<payload>" URL into a
// Gemini inlineData part. Plain http(s):// URLs are unsupported on the native
// surface; they are dropped (Gemini supports fileData with a Files API URI,
// not arbitrary public URLs).
func dataURLToInlinePart(url string) map[string]any {
	if !strings.HasPrefix(url, "data:") {
		return nil
	}
	rest := strings.TrimPrefix(url, "data:")
	mime, payload, ok := strings.Cut(rest, ";base64,")
	if !ok {
		return nil
	}
	if mime == "" {
		mime = "image/jpeg"
	}
	// Validate base64 once so the upstream rejects don't surface as opaque
	// 400s; we re-emit as base64 verbatim because Gemini wants the raw
	// base64 string in inlineData.data, not bytes.
	if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
		return nil
	}
	return map[string]any{
		"inlineData": map[string]any{"mimeType": mime, "data": payload},
	}
}

// openAIAssistantToGeminiParts converts an OpenAI assistant message to Gemini
// model-role Parts. Text content becomes a text part; each tool_call becomes
// a functionCall part. thought_signature smuggled on tool_calls is preserved
// as thoughtSignature on the corresponding part — the load-bearing round-trip
// for Gemini 3.x multi-turn tool use.
func openAIAssistantToGeminiParts(msg map[string]any) ([]any, error) {
	var parts []any
	if s := openAITextContent(msg["content"]); s != "" {
		parts = append(parts, map[string]any{"text": s})
	}
	tcs, _ := msg["tool_calls"].([]any)
	for _, t := range tcs {
		tc, _ := t.(map[string]any)
		if tc == nil {
			continue
		}
		fn, _ := tc["function"].(map[string]any)
		name, _ := fn["name"].(string)
		argsStr, _ := fn["arguments"].(string)
		var args any = map[string]any{}
		if argsStr != "" {
			if err := json.Unmarshal([]byte(argsStr), &args); err != nil {
				return nil, fmt.Errorf("parse tool_call arguments: %w", err)
			}
		}
		part := map[string]any{
			"functionCall": map[string]any{"name": name, "args": args},
		}
		// thought_signature may live either on the tool_call itself or on
		// the function object; round-trip whichever shape we receive.
		if sig, _ := tc["thought_signature"].(string); sig != "" {
			part["thoughtSignature"] = sig
		} else if sig, _ := fn["thought_signature"].(string); sig != "" {
			part["thoughtSignature"] = sig
		}
		parts = append(parts, part)
	}
	return parts, nil
}

// ----- OpenAI tools / tool_choice / generationConfig → Gemini -----

func pullOpenAIToolsToGemini(src map[string]any, out map[string]any) error {
	tools, _ := src["tools"].([]any)
	if len(tools) == 0 {
		return nil
	}
	var decls []any
	for _, t := range tools {
		tool, _ := t.(map[string]any)
		fn, _ := tool["function"].(map[string]any)
		if fn == nil {
			continue
		}
		decl := map[string]any{"name": fn["name"]}
		if d, ok := fn["description"]; ok {
			decl["description"] = d
		}
		if p, ok := fn["parameters"]; ok && p != nil {
			decl["parameters"] = sanitizeSchemaForGemini(p)
		}
		decls = append(decls, decl)
	}
	if len(decls) == 0 {
		return nil
	}
	out["tools"] = []any{map[string]any{"functionDeclarations": decls}}
	return nil
}

func pullOpenAIToolChoiceToGemini(body []byte, out map[string]any) {
	r := gjson.GetBytes(body, "tool_choice")
	if !r.Exists() {
		return
	}
	if r.Type == gjson.String {
		switch r.String() {
		case "auto":
			out["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{"mode": "AUTO"}}
		case "none":
			out["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{"mode": "NONE"}}
		case "required":
			out["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{"mode": "ANY"}}
		}
		return
	}
	if r.IsObject() && r.Get("type").String() == "function" {
		name := r.Get("function.name").String()
		if name == "" {
			return
		}
		out["toolConfig"] = map[string]any{
			"functionCallingConfig": map[string]any{
				"mode":                 "ANY",
				"allowedFunctionNames": []any{name},
			},
		}
	}
}

func pullOpenAIGenerationConfig(body []byte, out map[string]any, model string) {
	gc := make(map[string]any)
	if r := gjson.GetBytes(body, "temperature"); r.Exists() && r.Type == gjson.Number {
		gc["temperature"] = r.Num
	}
	if r := gjson.GetBytes(body, "top_p"); r.Exists() && r.Type == gjson.Number {
		gc["topP"] = r.Num
	}
	if r := gjson.GetBytes(body, "max_tokens"); r.Exists() && r.Type == gjson.Number {
		gc["maxOutputTokens"] = clampToModelOutputCap(int64(r.Num), model)
	}
	if r := gjson.GetBytes(body, "max_completion_tokens"); r.Exists() && r.Type == gjson.Number {
		gc["maxOutputTokens"] = clampToModelOutputCap(int64(r.Num), model)
	}
	if r := gjson.GetBytes(body, "stop"); r.Exists() {
		gc["stopSequences"] = stopToArray(r)
	}
	if r := gjson.GetBytes(body, "response_format"); r.Exists() && r.IsObject() {
		if r.Get("type").String() == "json_object" {
			gc["responseMimeType"] = "application/json"
		}
	}
	if r := gjson.GetBytes(body, "reasoning_effort"); r.Exists() && r.Type == gjson.String {
		if tc, ok := mapReasoningEffortToThinkingConfig(r.String()); ok {
			gc["thinkingConfig"] = tc
		}
	}
	if len(gc) > 0 {
		out["generationConfig"] = gc
	}
}

func clampToModelOutputCap(v int64, model string) int64 {
	cap := modelMaxOutputTokens[model]
	if cap == 0 {
		cap = defaultMaxOutputTokenCap
	}
	if v > int64(cap) {
		return int64(cap)
	}
	return v
}

func stopToArray(r gjson.Result) []any {
	if r.IsArray() {
		var out []any
		r.ForEach(func(_, v gjson.Result) bool {
			out = append(out, v.String())
			return true
		})
		return out
	}
	if r.Type == gjson.String {
		return []any{r.String()}
	}
	return nil
}

// mapReasoningEffortToThinkingConfig mirrors the table Google publishes for
// their OpenAI-compat surface. "none" passes through with thinkingBudget=0;
// Gemini 3.x rejects it but we let the upstream surface its own error rather
// than masking it here.
func mapReasoningEffortToThinkingConfig(effort string) (map[string]any, bool) {
	switch effort {
	case "none":
		return map[string]any{"thinkingBudget": 0}, true
	case "low":
		return map[string]any{"thinkingBudget": 1024}, true
	case "medium":
		return map[string]any{"thinkingBudget": 8192}, true
	case "high":
		return map[string]any{"thinkingBudget": 24576}, true
	}
	return nil, false
}

// ----- Anthropic → Gemini -----

func pullAnthropicSystemToGemini(src map[string]any, out map[string]any) {
	system := src["system"]
	var parts []string
	switch s := system.(type) {
	case string:
		if s != "" {
			parts = append(parts, s)
		}
	case []any:
		for _, b := range s {
			block, _ := b.(map[string]any)
			if block == nil {
				continue
			}
			if t, _ := block["type"].(string); t == "text" {
				if text, _ := block["text"].(string); text != "" {
					parts = append(parts, text)
				}
			}
		}
	}
	if len(parts) == 0 {
		return
	}
	out["systemInstruction"] = map[string]any{
		"parts": []any{map[string]any{"text": strings.Join(parts, "\n")}},
	}
}

// pullAnthropicContentsToGemini converts Anthropic messages into Gemini
// contents. tool_use blocks on assistant messages become functionCall parts
// (round-tripping thought_signature); tool_result blocks on user messages
// become functionResponse parts. Tracks tool_use_id → function name so a
// later tool_result block can recover the name Gemini requires.
func pullAnthropicContentsToGemini(src map[string]any, out map[string]any) error {
	msgs, _ := src["messages"].([]any)
	if len(msgs) == 0 {
		return nil
	}
	toolNames := anthropicCollectToolUseNames(msgs)

	var contents []any
	for _, raw := range msgs {
		msg, _ := raw.(map[string]any)
		if msg == nil {
			continue
		}
		role, _ := msg["role"].(string)
		switch role {
		case "user":
			parts, err := anthropicUserToGeminiParts(msg["content"], toolNames)
			if err != nil {
				return err
			}
			if len(parts) > 0 {
				contents = append(contents, map[string]any{"role": "user", "parts": parts})
			}
		case "assistant":
			parts, err := anthropicAssistantToGeminiParts(msg["content"])
			if err != nil {
				return err
			}
			if len(parts) > 0 {
				contents = append(contents, map[string]any{"role": "model", "parts": parts})
			}
		}
	}
	if len(contents) > 0 {
		out["contents"] = contents
	}
	return nil
}

func anthropicCollectToolUseNames(msgs []any) map[string]string {
	out := map[string]string{}
	for _, raw := range msgs {
		msg, _ := raw.(map[string]any)
		if msg == nil {
			continue
		}
		if r, _ := msg["role"].(string); r != "assistant" {
			continue
		}
		blocks, _ := msg["content"].([]any)
		for _, b := range blocks {
			block, _ := b.(map[string]any)
			if block == nil {
				continue
			}
			if t, _ := block["type"].(string); t != "tool_use" {
				continue
			}
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			if id != "" {
				out[id] = name
			}
		}
	}
	return out
}

func anthropicUserToGeminiParts(content any, toolNames map[string]string) ([]any, error) {
	switch c := content.(type) {
	case string:
		if c == "" {
			return nil, nil
		}
		return []any{map[string]any{"text": c}}, nil
	case []any:
		var parts []any
		for _, b := range c {
			block, _ := b.(map[string]any)
			if block == nil {
				continue
			}
			switch t, _ := block["type"].(string); t {
			case "text":
				if text, _ := block["text"].(string); text != "" {
					parts = append(parts, map[string]any{"text": text})
				}
			case "image":
				if part := anthropicImageToInlinePart(block); part != nil {
					parts = append(parts, part)
				}
			case "tool_result":
				id, _ := block["tool_use_id"].(string)
				name := toolNames[id]
				result := toolResultContent(block["content"])
				parts = append(parts, map[string]any{
					"functionResponse": map[string]any{"name": name, "response": map[string]any{"result": result}},
				})
			}
		}
		return parts, nil
	}
	return nil, nil
}

func anthropicImageToInlinePart(block map[string]any) map[string]any {
	src, _ := block["source"].(map[string]any)
	if src == nil {
		return nil
	}
	if t, _ := src["type"].(string); t != "base64" {
		return nil
	}
	data, _ := src["data"].(string)
	mime, _ := src["media_type"].(string)
	if data == "" {
		return nil
	}
	if mime == "" {
		mime = "image/jpeg"
	}
	return map[string]any{"inlineData": map[string]any{"mimeType": mime, "data": data}}
}

func anthropicAssistantToGeminiParts(content any) ([]any, error) {
	switch c := content.(type) {
	case string:
		if c == "" {
			return nil, nil
		}
		return []any{map[string]any{"text": c}}, nil
	case []any:
		var parts []any
		for _, b := range c {
			block, _ := b.(map[string]any)
			if block == nil {
				continue
			}
			switch t, _ := block["type"].(string); t {
			case "text":
				if text, _ := block["text"].(string); text != "" {
					parts = append(parts, map[string]any{"text": text})
				}
			case "tool_use":
				name, _ := block["name"].(string)
				input := block["input"]
				if input == nil {
					input = map[string]any{}
				}
				part := map[string]any{
					"functionCall": map[string]any{"name": name, "args": input},
				}
				if sig, _ := block["thought_signature"].(string); sig != "" {
					part["thoughtSignature"] = sig
				}
				parts = append(parts, part)
			}
		}
		return parts, nil
	}
	return nil, nil
}

func pullAnthropicToolsToGemini(src map[string]any, out map[string]any) error {
	tools, _ := src["tools"].([]any)
	if len(tools) == 0 {
		return nil
	}
	var decls []any
	for _, t := range tools {
		tool, _ := t.(map[string]any)
		if tool == nil {
			continue
		}
		name, _ := tool["name"].(string)
		if name == "" {
			continue
		}
		decl := map[string]any{"name": name}
		if d, ok := tool["description"]; ok {
			decl["description"] = d
		}
		if p, ok := tool["input_schema"]; ok && p != nil {
			decl["parameters"] = sanitizeSchemaForGemini(p)
		}
		decls = append(decls, decl)
	}
	if len(decls) == 0 {
		return nil
	}
	out["tools"] = []any{map[string]any{"functionDeclarations": decls}}
	return nil
}

// geminiUnsupportedSchemaKeys lists JSON Schema keywords that Google's
// function-calling API rejects with 400 "Cannot find field". Claude Code
// tool definitions routinely include these (Anthropic accepts full JSON
// Schema), so they must be stripped before sending upstream. The set is
// derived from upstream error responses observed in prod plus Google's
// documented OpenAPI 3.0 schema subset for function declarations.
//
// Keep the list conservative — strip only what we've seen Google reject;
// fields like description / nullable / enum / format are valid and must
// pass through.
var geminiUnsupportedSchemaKeys = map[string]struct{}{
	"$schema":               {},
	"$id":                   {},
	"$ref":                  {},
	"$defs":                 {},
	"definitions":           {},
	"additionalProperties":  {},
	"propertyNames":         {},
	"unevaluatedProperties": {},
	"patternProperties":     {},
	"dependencies":          {},
	"dependentRequired":     {},
	"dependentSchemas":      {},
	"if":                    {},
	"then":                  {},
	"else":                  {},
	"not":                   {},
	"allOf":                 {},
	"oneOf":                 {},
	"const":                 {},
	"contentEncoding":       {},
	"contentMediaType":      {},
	"contentSchema":         {},
	"readOnly":              {},
	"writeOnly":             {},
	"examples":              {},
	"deprecated":            {},
}

// sanitizeSchemaForGemini returns a deep copy of v with JSON Schema fields
// Google's function-calling surface rejects removed. Walks into properties,
// items, and anyOf children recursively. Non-map / non-slice nodes are
// passed through unchanged.
//
// The function does NOT short-circuit when a node has no unsupported keys —
// we always return a copy so the caller can mutate the result without
// touching the original input_schema (which the translate envelope holds
// onto for other emitters that DO accept full JSON Schema).
func sanitizeSchemaForGemini(v any) any {
	switch node := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(node))
		for k, child := range node {
			if _, drop := geminiUnsupportedSchemaKeys[k]; drop {
				continue
			}
			out[k] = sanitizeSchemaForGemini(child)
		}
		return out
	case []any:
		out := make([]any, len(node))
		for i, child := range node {
			out[i] = sanitizeSchemaForGemini(child)
		}
		return out
	default:
		return v
	}
}

func pullAnthropicToolChoiceToGemini(body []byte, out map[string]any) {
	r := gjson.GetBytes(body, "tool_choice")
	if !r.Exists() || !r.IsObject() {
		return
	}
	switch r.Get("type").String() {
	case "auto":
		out["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{"mode": "AUTO"}}
	case "any":
		out["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{"mode": "ANY"}}
	case "none":
		out["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{"mode": "NONE"}}
	case "tool":
		name := r.Get("name").String()
		if name == "" {
			return
		}
		out["toolConfig"] = map[string]any{
			"functionCallingConfig": map[string]any{
				"mode":                 "ANY",
				"allowedFunctionNames": []any{name},
			},
		}
	}
}

func pullAnthropicGenerationConfig(body []byte, out map[string]any, model string) {
	gc := make(map[string]any)
	if r := gjson.GetBytes(body, "temperature"); r.Exists() && r.Type == gjson.Number {
		gc["temperature"] = r.Num
	}
	if r := gjson.GetBytes(body, "top_p"); r.Exists() && r.Type == gjson.Number {
		gc["topP"] = r.Num
	}
	if r := gjson.GetBytes(body, "max_tokens"); r.Exists() && r.Type == gjson.Number {
		gc["maxOutputTokens"] = clampToModelOutputCap(int64(r.Num), model)
	}
	if r := gjson.GetBytes(body, "stop_sequences"); r.Exists() {
		gc["stopSequences"] = stopToArray(r)
	}
	if len(gc) > 0 {
		out["generationConfig"] = gc
	}
}
