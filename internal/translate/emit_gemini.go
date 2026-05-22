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

// PrepareGemini builds a Gemini native REST request body. The native surface is
// required for multi-turn tool use against Gemini 3.x preview models because
// OpenAI-compat does not return the opaque thought_signature field.
func (e *RequestEnvelope) PrepareGemini(_ http.Header, opts EmitOptions) (providers.PreparedRequest, error) {
	// Strip synthetic top-level "model" and "stream" — belonging to routing, not Gemini.
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
		// Same-format Gemini requests still need tool schema sanitization:
		// the inbound tools may carry JSON Schema keywords Gemini rejects.
		body, err = sanitizeGeminiTools(body)
		if err != nil {
			return providers.PreparedRequest{}, fmt.Errorf("sanitize gemini tools: %w", err)
		}
		headers := make(http.Header)
		if e.Stream() {
			headers.Set(GeminiStreamHintHeader, "true")
		}
		return providers.PreparedRequest{Body: body, Headers: headers}, nil
	}

	jw := newJSONWriter()
	jw.Obj()
	switch e.format {
	case FormatOpenAI:
		if err := writeGeminiFromOpenAI(jw, e.body, opts); err != nil {
			return providers.PreparedRequest{}, err
		}
	case FormatAnthropic:
		writeGeminiFromAnthropic(jw, e.body, opts)
	default:
		return providers.PreparedRequest{}, fmt.Errorf("unsupported source format for Gemini emit: %d", e.format)
	}
	jw.EndObj()
	body := jw.Bytes()

	// Synthetic header so proxy.Service stays ignorant of provider URL composition.
	headers := make(http.Header)
	if e.Stream() {
		headers.Set(GeminiStreamHintHeader, "true")
	}
	return providers.PreparedRequest{Body: body, Headers: headers}, nil
}

// GeminiStreamHintHeader is the synthetic header PrepareGemini sets when the
// inbound request asked for streaming. The native Gemini client consumes it
// to pick between :generateContent and :streamGenerateContent and strips it
// before forwarding.
const GeminiStreamHintHeader = "X-Router-Gemini-Stream"

// sanitizeGeminiTools walks the tools array of an already-Gemini-format request
// body and runs sanitizeSchemaForGemini on every function declaration's
// parameters. The same-format path (FormatGemini) bypasses the per-field
// translation helpers, so tool schemas must be sanitized in-place.
func sanitizeGeminiTools(body []byte) ([]byte, error) {
	// Quick check: if there are no tools, return unchanged.
	if !gjson.GetBytes(body, "tools").Exists() {
		return body, nil
	}
	var err error
	body, err = sjson.SetBytes(body, "tools", sanitizeGeminiToolsRaw(gjson.GetBytes(body, "tools").Value()))
	if err != nil {
		return nil, fmt.Errorf("set sanitized tools: %w", err)
	}
	return body, nil
}

// sanitizeGeminiToolsRaw is the gjson-compatible walker for the tools array.
func sanitizeGeminiToolsRaw(v any) any {
	tools, ok := v.([]any)
	if !ok {
		return v
	}
	out := make([]any, len(tools))
	for i, t := range tools {
		tool, _ := t.(map[string]any)
		if tool == nil {
			out[i] = t
			continue
		}
		fds, _ := tool["functionDeclarations"].([]any)
		if len(fds) == 0 {
			out[i] = t
			continue
		}
		sanitized := make([]any, len(fds))
		for j, fd := range fds {
			fdMap, _ := fd.(map[string]any)
			if fdMap == nil {
				sanitized[j] = fd
				continue
			}
			if params, ok := fdMap["parameters"]; ok && params != nil {
				fdMap = copyMap(fdMap)
				fdMap["parameters"] = sanitizeSchemaForGemini(inlineSchemaDefs(params))
			}
			sanitized[j] = fdMap
		}
		toolCopy := copyMap(tool)
		toolCopy["functionDeclarations"] = sanitized
		out[i] = toolCopy
	}
	return out
}

// copyMap returns a shallow copy of m so that modifying the copy does not
// mutate the original, which gjson may be sharing with the caller.
func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ----- OpenAI → Gemini -----

// writeGeminiFromOpenAI translates an OpenAI-format body into Gemini fields
// written directly into jw (caller has already opened the root object).
func writeGeminiFromOpenAI(jw *jsonWriter, body []byte, opts EmitOptions) error {
	msgs := gjson.GetBytes(body, "messages")

	// First pass: build tool_call ID → function name map for role:tool messages,
	// and detect any tool_call lacking a thoughtSignature. Gemini 3.x rejects
	// requests whose history carries a functionCall without one, so when even
	// one is missing we drop ALL tool_call + role:tool blocks. Mirrors the
	// guard in writeGeminiFromAnthropic — covers OpenAI-surface clients whose
	// assistant history was produced by a non-Gemini provider before a
	// mid-session router switch.
	toolNames := make(map[string]string)
	anyToolCallMissingSig := false
	msgs.ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() != "assistant" {
			return true
		}
		msg.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
			if id := tc.Get("id").String(); id != "" {
				toolNames[id] = tc.Get("function.name").String()
			}
			if extractThoughtSignature(tc) == "" {
				anyToolCallMissingSig = true
			}
			return true
		})
		return true
	})
	dropToolBlocks := anyToolCallMissingSig && isGemini3xModel(opts.TargetModel)

	// Collect system text.
	var sysParts []string
	msgs.ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() != "system" {
			return true
		}
		content := msg.Get("content")
		switch content.Type {
		case gjson.String:
			if s := content.String(); s != "" {
				sysParts = append(sysParts, s)
			}
		case gjson.JSON:
			content.ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() == "text" {
					if s := part.Get("text").String(); s != "" {
						sysParts = append(sysParts, s)
					}
				}
				return true
			})
		}
		return true
	})
	if len(sysParts) > 0 {
		jw.Key("systemInstruction")
		jw.Obj()
		jw.Key("parts")
		jw.Arr()
		jw.Obj()
		jw.Key("text")
		jw.Str(strings.Join(sysParts, "\n"))
		jw.EndObj()
		jw.EndArr()
		jw.EndObj()
	}

	// Second pass: build content entries, then post-process for role
	// alternation before emitting. Two-pass is necessary because dropping
	// sig-less tool turns can leave placeholder entries adjacent to real
	// content of the same role — e.g. the OpenAI per-tool_call_id
	// `role:"tool"` messages each contribute a user placeholder and the
	// following real user turn would land right after. The collapser merges
	// placeholders with real content of the same role.
	entries := make([]contentEntry, 0, 8)
	var walkErr error
	msgs.ForEach(func(_, msg gjson.Result) bool {
		role := msg.Get("role").String()
		switch role {
		case "system":
			return true
		case "user":
			parts := openAIUserPartsGJSON(msg.Get("content"))
			if len(parts) == 0 {
				return true
			}
			entries = append(entries, contentEntry{role: "user", parts: parts})
		case "assistant":
			parts, parseErr := openAIAssistantPartsGJSON(msg)
			if parseErr != nil {
				walkErr = parseErr
				return false
			}
			placeholder := false
			if dropToolBlocks {
				before := len(parts)
				parts = filterOutGeminiFunctionCallParts(parts)
				if before > 0 && len(parts) == 0 {
					parts = []string{geminiTextPart(droppedToolCallPlaceholder)}
					placeholder = true
				}
			}
			if len(parts) == 0 {
				return true
			}
			entries = append(entries, contentEntry{role: "model", parts: parts, placeholder: placeholder})
		case "tool":
			if dropToolBlocks {
				entries = append(entries, contentEntry{
					role:        "user",
					parts:       []string{geminiTextPart(droppedToolResultPlaceholder)},
					placeholder: true,
				})
				return true
			}
			tcID := msg.Get("tool_call_id").String()
			name := toolNames[tcID]
			if name == "" {
				return true
			}
			result := toolResultContentGJSON(msg.Get("content"))
			entries = append(entries, contentEntry{
				role:  "user",
				parts: []string{geminiFunctionResponsePart(name, result)},
			})
		}
		return true
	})
	if walkErr != nil {
		return walkErr
	}
	emitGeminiContents(jw, collapseConsecutiveRoles(entries))

	writeGeminiToolsFromOpenAI(jw, body)
	writeGeminiToolChoiceFromOpenAI(jw, body)
	writeGeminiGenerationConfigFromOpenAI(jw, body, opts.TargetModel)
	return nil
}

// openAIUserPartsGJSON converts an OpenAI user content value to raw JSON part strings.
// http(s) image URLs are unsupported on the native surface and dropped.
func openAIUserPartsGJSON(content gjson.Result) []string {
	switch content.Type {
	case gjson.String:
		s := content.String()
		if s == "" {
			return nil
		}
		return []string{geminiTextPart(s)}
	case gjson.JSON:
		var parts []string
		content.ForEach(func(_, p gjson.Result) bool {
			switch p.Get("type").String() {
			case "text":
				if s := p.Get("text").String(); s != "" {
					parts = append(parts, geminiTextPart(s))
				}
			case "image_url":
				urlStr := p.Get("image_url.url").String()
				mime, data, ok := parseDataURL(urlStr)
				if !ok {
					return true
				}
				if _, err := base64.StdEncoding.DecodeString(data); err != nil {
					return true
				}
				if mime == "" {
					mime = "image/jpeg"
				}
				pw := newJSONWriter()
				pw.Obj()
				pw.Key("inlineData")
				pw.Obj()
				pw.Key("mimeType")
				pw.Str(mime)
				pw.Key("data")
				pw.Str(data)
				pw.EndObj()
				pw.EndObj()
				parts = append(parts, string(pw.Bytes()))
			}
			return true
		})
		return parts
	}
	return nil
}

// openAIAssistantPartsGJSON converts an OpenAI assistant message to raw JSON part strings.
func openAIAssistantPartsGJSON(msg gjson.Result) ([]string, error) {
	var parts []string

	content := msg.Get("content")
	if text := openAIContentTextGJSON(content); text != "" {
		parts = append(parts, geminiTextPart(text))
	}

	// Inherit any sig from sibling tool_calls or message-level thought_signature
	// so every functionCall carries one on round-trip to Gemini 3.x.
	var inheritedSig string
	if sig := msg.Get("thought_signature").String(); sig != "" {
		inheritedSig = sig
	}
	if inheritedSig == "" {
		msg.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
			if sig := extractThoughtSignature(tc); sig != "" {
				inheritedSig = sig
				return false
			}
			return true
		})
	}

	var parseErr error
	msg.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
		name := tc.Get("function.name").String()
		argsStr := tc.Get("function.arguments").String()
		if argsStr != "" && !gjson.Valid(argsStr) {
			parseErr = fmt.Errorf("parse tool_call arguments: invalid JSON")
			return false
		}

		pw := newJSONWriter()
		pw.Obj()
		pw.Key("functionCall")
		pw.Obj()
		pw.Key("name")
		pw.Str(name)
		pw.Key("args")
		if argsStr != "" {
			pw.Raw(argsStr)
		} else {
			pw.Raw("{}")
		}
		pw.EndObj()
		sig := extractThoughtSignature(tc)
		if sig == "" {
			sig = inheritedSig
		}
		if sig != "" {
			pw.Key("thoughtSignature")
			pw.Str(sig)
		}
		pw.EndObj()
		parts = append(parts, string(pw.Bytes()))
		return true
	})

	return parts, parseErr
}

func geminiTextPart(text string) string {
	jw := newJSONWriter()
	jw.Obj()
	jw.Key("text")
	jw.Str(text)
	jw.EndObj()
	return string(jw.Bytes())
}

// writeGeminiToolsFromOpenAI writes the tools array into jw from an OpenAI body.
func writeGeminiToolsFromOpenAI(jw *jsonWriter, body []byte) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() || tools.Get("#").Int() == 0 {
		return
	}

	var decls []string
	tools.ForEach(func(_, t gjson.Result) bool {
		fn := t.Get("function")
		if !fn.Exists() {
			return true
		}
		name := fn.Get("name").String()
		if name == "" {
			return true
		}
		dw := newJSONWriter()
		dw.Obj()
		dw.Key("name")
		dw.Str(name)
		if desc := fn.Get("description"); desc.Exists() {
			dw.Key("description")
			dw.Str(desc.String())
		}
		if params := fn.Get("parameters"); params.Exists() {
			var schema any
			if err := json.Unmarshal([]byte(params.Raw), &schema); err == nil {
				schema = inlineSchemaDefs(schema)
				schema = sanitizeSchemaForGemini(schema)
				if schemaBytes, err := json.Marshal(schema); err == nil {
					dw.Key("parameters")
					dw.RawBytes(schemaBytes)
				}
			}
		}
		dw.EndObj()
		decls = append(decls, string(dw.Bytes()))
		return true
	})

	if len(decls) == 0 {
		return
	}
	jw.Key("tools")
	jw.Arr()
	jw.Obj()
	jw.Key("functionDeclarations")
	jw.Arr()
	for _, d := range decls {
		jw.Raw(d)
	}
	jw.EndArr()
	jw.EndObj()
	jw.EndArr()
}

// writeGeminiToolChoiceFromOpenAI writes toolConfig into jw from an OpenAI body.
func writeGeminiToolChoiceFromOpenAI(jw *jsonWriter, body []byte) {
	r := gjson.GetBytes(body, "tool_choice")
	if !r.Exists() {
		return
	}
	if r.Type == gjson.String {
		var mode string
		switch r.String() {
		case "auto":
			mode = "AUTO"
		case "none":
			mode = "NONE"
		case "required":
			mode = "ANY"
		}
		if mode == "" {
			return
		}
		jw.Key("toolConfig")
		jw.Obj()
		jw.Key("functionCallingConfig")
		jw.Obj()
		jw.Key("mode")
		jw.Str(mode)
		jw.EndObj()
		jw.EndObj()
		return
	}
	if r.IsObject() && r.Get("type").String() == "function" {
		name := r.Get("function.name").String()
		if name == "" {
			return
		}
		jw.Key("toolConfig")
		jw.Obj()
		jw.Key("functionCallingConfig")
		jw.Obj()
		jw.Key("mode")
		jw.Str("ANY")
		jw.Key("allowedFunctionNames")
		jw.Arr()
		jw.Str(name)
		jw.EndArr()
		jw.EndObj()
		jw.EndObj()
	}
}

// writeGeminiGenerationConfigFromOpenAI writes generationConfig into jw from an OpenAI body.
func writeGeminiGenerationConfigFromOpenAI(jw *jsonWriter, body []byte, model string) {
	// Collect all generation config fields; only write the object if non-empty.
	type field struct {
		key string
		raw string
	}
	var fields []field

	if r := gjson.GetBytes(body, "temperature"); r.Exists() && r.Type == gjson.Number {
		fw := newJSONWriter()
		fw.Float(r.Num)
		fields = append(fields, field{"temperature", string(fw.Bytes())})
	}
	if r := gjson.GetBytes(body, "top_p"); r.Exists() && r.Type == gjson.Number {
		fw := newJSONWriter()
		fw.Float(r.Num)
		fields = append(fields, field{"topP", string(fw.Bytes())})
	}
	// max_completion_tokens takes precedence over max_tokens if both present.
	if r := gjson.GetBytes(body, "max_completion_tokens"); r.Exists() && r.Type == gjson.Number {
		fw := newJSONWriter()
		fw.Int(clampToModelOutputCap(int64(r.Num), model))
		fields = append(fields, field{"maxOutputTokens", string(fw.Bytes())})
	} else if r := gjson.GetBytes(body, "max_tokens"); r.Exists() && r.Type == gjson.Number {
		fw := newJSONWriter()
		fw.Int(clampToModelOutputCap(int64(r.Num), model))
		fields = append(fields, field{"maxOutputTokens", string(fw.Bytes())})
	}
	if r := gjson.GetBytes(body, "stop"); r.Exists() {
		if raw := stopToArrayRaw(r); raw != "" {
			fields = append(fields, field{"stopSequences", raw})
		}
	}
	if r := gjson.GetBytes(body, "response_format"); r.Exists() && r.IsObject() {
		if r.Get("type").String() == "json_object" {
			fields = append(fields, field{"responseMimeType", `"application/json"`})
		}
	}
	if r := gjson.GetBytes(body, "reasoning_effort"); r.Exists() && r.Type == gjson.String {
		if raw := thinkingConfigRaw(r.String()); raw != "" {
			fields = append(fields, field{"thinkingConfig", raw})
		}
	}

	if len(fields) == 0 {
		return
	}
	jw.Key("generationConfig")
	jw.Obj()
	for _, f := range fields {
		jw.Key(f.key)
		jw.Raw(f.raw)
	}
	jw.EndObj()
}

// ----- Anthropic → Gemini -----

// writeGeminiFromAnthropic translates an Anthropic-format body into Gemini fields
// written directly into jw (caller has already opened the root object).
func writeGeminiFromAnthropic(jw *jsonWriter, body []byte, opts EmitOptions) {
	// System prompt.
	system := gjson.GetBytes(body, "system")
	var sysParts []string
	switch system.Type {
	case gjson.String:
		if stripped := stripAnthropicBillingHeader(system.String()); stripped != "" {
			sysParts = append(sysParts, stripped)
		}
	case gjson.JSON:
		system.ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() == "text" {
				if text := block.Get("text").String(); text != "" {
					if stripped := stripAnthropicBillingHeader(text); stripped != "" {
						sysParts = append(sysParts, stripped)
					}
				}
			}
			return true
		})
	}
	if len(sysParts) > 0 {
		jw.Key("systemInstruction")
		jw.Obj()
		jw.Key("parts")
		jw.Arr()
		jw.Obj()
		jw.Key("text")
		jw.Str(strings.Join(sysParts, "\n"))
		jw.EndObj()
		jw.EndArr()
		jw.EndObj()
	}

	msgs := gjson.GetBytes(body, "messages")

	// First pass: collect tool_use ID → name for tool_result recovery, and
	// detect any tool_use lacking a thoughtSignature. Gemini 3.x rejects any
	// request whose history contains a functionCall without a signature, so
	// when even one is missing we drop ALL tool_use/tool_result blocks from
	// the history. This prevents 400s on sticky-pin Gemini turns whose
	// history was produced by a different provider (mimo/qwen/etc.) before
	// a mid-session router switch.
	toolNames := make(map[string]string)
	anyToolUseMissingSig := false
	dropToolBlocks := false
	msgs.ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() != "assistant" {
			return true
		}
		msg.Get("content").ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() == "tool_use" {
				if id := block.Get("id").String(); id != "" {
					toolNames[id] = block.Get("name").String()
				}
				if extractThoughtSignature(block) == "" {
					anyToolUseMissingSig = true
				}
			}
			return true
		})
		return true
	})
	// Only Gemini 3.x preview models hard-require thoughtSignature on every
	// functionCall part. 2.x accepts sig-less calls, so don't molest those
	// requests — the lossless translation is correct there.
	if anyToolUseMissingSig && isGemini3xModel(opts.TargetModel) {
		dropToolBlocks = true
	}

	// Second pass: build entries, then collapse + emit. See contentEntry /
	// collapseConsecutiveRoles — they preserve role alternation when the
	// sig-less-tool drop guard would otherwise produce same-role runs.
	entries := make([]contentEntry, 0, 8)
	msgs.ForEach(func(_, msg gjson.Result) bool {
		role := msg.Get("role").String()
		switch role {
		case "user":
			parts := anthropicUserPartsGJSON(msg.Get("content"), toolNames)
			placeholder := false
			if dropToolBlocks {
				before := len(parts)
				parts = filterOutGeminiToolResponseParts(parts)
				if before > 0 && len(parts) == 0 {
					parts = []string{geminiTextPart(droppedToolResultPlaceholder)}
					placeholder = true
				}
			}
			if len(parts) == 0 {
				return true
			}
			entries = append(entries, contentEntry{role: "user", parts: parts, placeholder: placeholder})
		case "assistant":
			parts := anthropicAssistantPartsGJSON(msg.Get("content"))
			placeholder := false
			if dropToolBlocks {
				before := len(parts)
				parts = filterOutGeminiFunctionCallParts(parts)
				if before > 0 && len(parts) == 0 {
					parts = []string{geminiTextPart(droppedToolCallPlaceholder)}
					placeholder = true
				}
			}
			if len(parts) == 0 {
				return true
			}
			entries = append(entries, contentEntry{role: "model", parts: parts, placeholder: placeholder})
		}
		return true
	})
	emitGeminiContents(jw, collapseConsecutiveRoles(entries))

	writeGeminiToolsFromAnthropic(jw, body)
	writeGeminiToolChoiceFromAnthropic(jw, body)
	writeGeminiGenerationConfigFromAnthropic(jw, body, opts.TargetModel)
}

// contentEntry is a buffered Gemini `contents` array entry. Both `Prepare*`
// paths collect entries first so a post-pass can merge placeholders with
// real same-role content before emitting, preserving role alternation.
type contentEntry struct {
	role        string   // "user" or "model"
	parts       []string // pre-serialized Gemini part JSON objects
	placeholder bool     // synthesized by the sig-less-tool drop guard
}

// collapseConsecutiveRoles merges adjacent entries that share a role. When
// a placeholder neighbors real same-role content, the real content wins and
// the placeholder is dropped. Two real same-role entries merge their parts.
// Two placeholders collapse to one. Required because the sig-less-tool drop
// guard can otherwise emit user/user or model/model sequences (Gemini 400s
// on non-alternating roles).
func collapseConsecutiveRoles(in []contentEntry) []contentEntry {
	if len(in) == 0 {
		return in
	}
	out := make([]contentEntry, 0, len(in))
	for _, e := range in {
		if len(out) == 0 || out[len(out)-1].role != e.role {
			out = append(out, e)
			continue
		}
		prev := &out[len(out)-1]
		switch {
		case prev.placeholder && !e.placeholder:
			*prev = e
		case !prev.placeholder && e.placeholder:
			// keep prev, drop incoming placeholder
		default:
			prev.parts = append(prev.parts, e.parts...)
		}
	}
	return out
}

// emitGeminiContents writes the contents array from a collapsed entry slice.
// Skips writing the key entirely when there are no entries so absence and
// emptiness aren't conflated downstream.
func emitGeminiContents(jw *jsonWriter, entries []contentEntry) {
	if len(entries) == 0 {
		return
	}
	jw.Key("contents")
	jw.Arr()
	for _, e := range entries {
		jw.Obj()
		jw.Key("role")
		jw.Str(e.role)
		jw.Key("parts")
		jw.Arr()
		for _, p := range e.parts {
			jw.Raw(p)
		}
		jw.EndArr()
		jw.EndObj()
	}
	jw.EndArr()
}

// geminiFunctionResponsePart serializes a Gemini functionResponse part.
func geminiFunctionResponsePart(name, result string) string {
	pw := newJSONWriter()
	pw.Obj()
	pw.Key("functionResponse")
	pw.Obj()
	pw.Key("name")
	pw.Str(name)
	pw.Key("response")
	pw.Obj()
	pw.Key("result")
	pw.Str(result)
	pw.EndObj()
	pw.EndObj()
	pw.EndObj()
	return string(pw.Bytes())
}

// Placeholders inserted when Gemini 3.x sig-less tool blocks are dropped from
// the request. They preserve role alternation in the `contents` array so a
// run of dropped tool turns doesn't collapse adjacent assistant or user turns
// into back-to-back same-role entries (Gemini 400s on non-alternating roles).
const (
	droppedToolCallPlaceholder   = "[router: prior tool call omitted — provider's signed thinking state was unavailable for cross-model carry-over]"
	droppedToolResultPlaceholder = "[router: prior tool result omitted — paired with a dropped tool call]"
)

// isGemini3xModel returns true for Gemini 3.x preview models, which require
// thoughtSignature on every functionCall part across turns. 2.x and earlier
// accept sig-less calls.
func isGemini3xModel(model string) bool {
	return strings.HasPrefix(model, "gemini-3")
}

// filterOutGeminiFunctionCallParts strips functionCall parts from a slice of
// serialized Gemini parts. Used when the assistant history contains tool_use
// blocks without thoughtSignature — emitting them to Gemini 3.x would 400.
func filterOutGeminiFunctionCallParts(parts []string) []string {
	out := parts[:0]
	for _, p := range parts {
		if gjson.Get(p, "functionCall").Exists() {
			continue
		}
		out = append(out, p)
	}
	return out
}

// filterOutGeminiToolResponseParts strips functionResponse parts so tool_results
// dangle no longer reference functionCalls we dropped. Without this, Gemini
// rejects functionResponses whose matching functionCalls aren't in history.
func filterOutGeminiToolResponseParts(parts []string) []string {
	out := parts[:0]
	for _, p := range parts {
		if gjson.Get(p, "functionResponse").Exists() {
			continue
		}
		out = append(out, p)
	}
	return out
}

// anthropicUserPartsGJSON converts an Anthropic user content value to raw JSON part strings.
func anthropicUserPartsGJSON(content gjson.Result, toolNames map[string]string) []string {
	switch content.Type {
	case gjson.String:
		s := content.String()
		if s == "" {
			return nil
		}
		return []string{geminiTextPart(s)}
	case gjson.JSON:
		var parts []string
		content.ForEach(func(_, block gjson.Result) bool {
			switch block.Get("type").String() {
			case "text":
				if text := block.Get("text").String(); text != "" {
					parts = append(parts, geminiTextPart(text))
				}
			case "image":
				if block.Get("source.type").String() != "base64" {
					return true
				}
				data := block.Get("source.data").String()
				if data == "" {
					return true
				}
				mime := block.Get("source.media_type").String()
				if mime == "" {
					mime = "image/jpeg"
				}
				pw := newJSONWriter()
				pw.Obj()
				pw.Key("inlineData")
				pw.Obj()
				pw.Key("mimeType")
				pw.Str(mime)
				pw.Key("data")
				pw.Str(data)
				pw.EndObj()
				pw.EndObj()
				parts = append(parts, string(pw.Bytes()))
			case "tool_result":
				id := block.Get("tool_use_id").String()
				name := toolNames[id]
				if name == "" {
					return true
				}
				result := toolResultContentGJSON(block.Get("content"))
				pw := newJSONWriter()
				pw.Obj()
				pw.Key("functionResponse")
				pw.Obj()
				pw.Key("name")
				pw.Str(name)
				pw.Key("response")
				pw.Obj()
				pw.Key("result")
				pw.Str(result)
				pw.EndObj()
				pw.EndObj()
				pw.EndObj()
				parts = append(parts, string(pw.Bytes()))
			}
			return true
		})
		return parts
	}
	return nil
}

// anthropicAssistantPartsGJSON converts an Anthropic assistant content value to raw JSON part strings.
func anthropicAssistantPartsGJSON(content gjson.Result) []string {
	switch content.Type {
	case gjson.String:
		s := content.String()
		if s == "" {
			return nil
		}
		return []string{geminiTextPart(s)}
	case gjson.JSON:
		// First pass: find any thought_signature in the assistant turn so
		// functionCall blocks without their own sig can inherit it. Gemini 3.x
		// rejects requests with missing thoughtSignature on any functionCall
		// part — only one block per turn typically carries the original sig.
		var inheritedSig string
		content.ForEach(func(_, block gjson.Result) bool {
			if sig := extractThoughtSignature(block); sig != "" {
				inheritedSig = sig
				return false
			}
			return true
		})
		var parts []string
		content.ForEach(func(_, block gjson.Result) bool {
			switch block.Get("type").String() {
			case "text":
				text := block.Get("text").String()
				if text == "" {
					return true
				}
				pw := newJSONWriter()
				pw.Obj()
				pw.Key("text")
				pw.Str(text)
				if sig := block.Get("thought_signature").String(); sig != "" {
					pw.Key("thoughtSignature")
					pw.Str(sig)
				}
				pw.EndObj()
				parts = append(parts, string(pw.Bytes()))
			case "tool_use":
				name := block.Get("name").String()
				inputRaw := block.Get("input").Raw
				if inputRaw == "" || inputRaw == "null" {
					inputRaw = "{}"
				}
				pw := newJSONWriter()
				pw.Obj()
				pw.Key("functionCall")
				pw.Obj()
				pw.Key("name")
				pw.Str(name)
				pw.Key("args")
				pw.Raw(inputRaw)
				pw.EndObj()
				// thought_signature may live on the block or be smuggled in
				// block.id. Fall back to the assistant turn's first sig so
				// every functionCall carries one on round-trip.
				sig := extractThoughtSignature(block)
				if sig == "" {
					sig = inheritedSig
				}
				if sig != "" {
					pw.Key("thoughtSignature")
					pw.Str(sig)
				}
				pw.EndObj()
				parts = append(parts, string(pw.Bytes()))
			}
			return true
		})
		return parts
	}
	return nil
}

// writeGeminiToolsFromAnthropic writes the tools array into jw from an Anthropic body.
func writeGeminiToolsFromAnthropic(jw *jsonWriter, body []byte) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() || tools.Get("#").Int() == 0 {
		return
	}

	var decls []string
	tools.ForEach(func(_, t gjson.Result) bool {
		name := t.Get("name").String()
		if name == "" {
			return true
		}
		dw := newJSONWriter()
		dw.Obj()
		dw.Key("name")
		dw.Str(name)
		if desc := t.Get("description"); desc.Exists() {
			dw.Key("description")
			dw.Str(desc.String())
		}
		if params := t.Get("input_schema"); params.Exists() {
			var schema any
			if err := json.Unmarshal([]byte(params.Raw), &schema); err == nil {
				schema = inlineSchemaDefs(schema)
				schema = sanitizeSchemaForGemini(schema)
				if schemaBytes, err := json.Marshal(schema); err == nil {
					dw.Key("parameters")
					dw.RawBytes(schemaBytes)
				}
			}
		}
		dw.EndObj()
		decls = append(decls, string(dw.Bytes()))
		return true
	})

	if len(decls) == 0 {
		return
	}
	jw.Key("tools")
	jw.Arr()
	jw.Obj()
	jw.Key("functionDeclarations")
	jw.Arr()
	for _, d := range decls {
		jw.Raw(d)
	}
	jw.EndArr()
	jw.EndObj()
	jw.EndArr()
}

// writeGeminiToolChoiceFromAnthropic writes toolConfig into jw from an Anthropic body.
func writeGeminiToolChoiceFromAnthropic(jw *jsonWriter, body []byte) {
	r := gjson.GetBytes(body, "tool_choice")
	if !r.Exists() || !r.IsObject() {
		return
	}
	switch r.Get("type").String() {
	case "auto":
		jw.Key("toolConfig")
		jw.Obj()
		jw.Key("functionCallingConfig")
		jw.Obj()
		jw.Key("mode")
		jw.Str("AUTO")
		jw.EndObj()
		jw.EndObj()
	case "any":
		jw.Key("toolConfig")
		jw.Obj()
		jw.Key("functionCallingConfig")
		jw.Obj()
		jw.Key("mode")
		jw.Str("ANY")
		jw.EndObj()
		jw.EndObj()
	case "none":
		jw.Key("toolConfig")
		jw.Obj()
		jw.Key("functionCallingConfig")
		jw.Obj()
		jw.Key("mode")
		jw.Str("NONE")
		jw.EndObj()
		jw.EndObj()
	case "tool":
		name := r.Get("name").String()
		if name == "" {
			return
		}
		jw.Key("toolConfig")
		jw.Obj()
		jw.Key("functionCallingConfig")
		jw.Obj()
		jw.Key("mode")
		jw.Str("ANY")
		jw.Key("allowedFunctionNames")
		jw.Arr()
		jw.Str(name)
		jw.EndArr()
		jw.EndObj()
		jw.EndObj()
	}
}

// writeGeminiGenerationConfigFromAnthropic writes generationConfig into jw from an Anthropic body.
func writeGeminiGenerationConfigFromAnthropic(jw *jsonWriter, body []byte, model string) {
	type field struct {
		key string
		raw string
	}
	var fields []field

	if r := gjson.GetBytes(body, "temperature"); r.Exists() && r.Type == gjson.Number {
		fw := newJSONWriter()
		fw.Float(r.Num)
		fields = append(fields, field{"temperature", string(fw.Bytes())})
	}
	if r := gjson.GetBytes(body, "top_p"); r.Exists() && r.Type == gjson.Number {
		fw := newJSONWriter()
		fw.Float(r.Num)
		fields = append(fields, field{"topP", string(fw.Bytes())})
	}
	if r := gjson.GetBytes(body, "max_tokens"); r.Exists() && r.Type == gjson.Number {
		fw := newJSONWriter()
		fw.Int(clampToModelOutputCap(int64(r.Num), model))
		fields = append(fields, field{"maxOutputTokens", string(fw.Bytes())})
	}
	if r := gjson.GetBytes(body, "stop_sequences"); r.Exists() {
		if raw := stopToArrayRaw(r); raw != "" {
			fields = append(fields, field{"stopSequences", raw})
		}
	}

	if len(fields) == 0 {
		return
	}
	jw.Key("generationConfig")
	jw.Obj()
	for _, f := range fields {
		jw.Key(f.key)
		jw.Raw(f.raw)
	}
	jw.EndObj()
}

// clampToModelOutputCap caps v to the model's max output token limit.
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

// stopToArrayRaw serializes a stop gjson.Result into a raw JSON array string.
// Returns "" when no valid stops exist.
func stopToArrayRaw(r gjson.Result) string {
	var items []string
	if r.IsArray() {
		r.ForEach(func(_, v gjson.Result) bool {
			if s := v.String(); s != "" {
				sw := newJSONWriter()
				sw.Str(s)
				items = append(items, string(sw.Bytes()))
			}
			return true
		})
	} else if r.Type == gjson.String {
		if s := r.String(); s != "" {
			sw := newJSONWriter()
			sw.Str(s)
			items = append(items, string(sw.Bytes()))
		}
	}
	if len(items) == 0 {
		return ""
	}
	return "[" + strings.Join(items, ",") + "]"
}

// thinkingConfigRaw returns the raw JSON for a Gemini thinkingConfig given an
// OpenAI reasoning_effort string. Returns "" for unrecognised values.
func thinkingConfigRaw(effort string) string {
	var budget int64
	switch effort {
	case "none":
		budget = 0
	case "low":
		budget = 1024
	case "medium":
		budget = 8192
	case "high":
		budget = 24576
	default:
		return ""
	}
	fw := newJSONWriter()
	fw.Obj()
	fw.Key("thinkingBudget")
	fw.Int(budget)
	fw.EndObj()
	return string(fw.Bytes())
}

// sanitizeGeminiTools walks the tools array of an already-Gemini-format request
// body and runs sanitizeSchemaForGemini on every function declaration's
// parameters. The same-format path (FormatGemini) bypasses the per-field
// translation helpers, so tool schemas must be sanitized in-place.

// geminiSchemaAllowedKeys is the set of JSON Schema keywords that Gemini's
// function-calling API accepts, derived from the Schema struct in the Google
// genai Go SDK (googleapis/go-genai types.go). Any key not in this set is
// silently dropped — this is an allow-list, not a deny-list, so new JSON
// Schema keywords that tool authors add are rejected before they can 400.
var geminiSchemaAllowedKeys = map[string]struct{}{
	"type":             {},
	"nullable":         {},
	"description":      {},
	"format":           {},
	"enum":             {},
	"items":            {},
	"properties":       {},
	"required":         {},
	"title":            {},
	"default":          {},
	"example":          {},
	"pattern":          {},
	"anyOf":            {},
	"maxItems":         {},
	"maxLength":        {},
	"maxProperties":    {},
	"maximum":          {},
	"minItems":         {},
	"minLength":        {},
	"minProperties":    {},
	"minimum":          {},
	"propertyOrdering": {},
}

// sanitizeSchemaForGemini returns a deep copy of v containing only the JSON
// Schema fields that Gemini's function-calling API accepts. Uses an allow-list
// derived from the googleapis/go-genai Schema struct. Always returns a copy so
// the caller can mutate without touching the original input_schema (other
// emitters DO accept full JSON Schema).
func sanitizeSchemaForGemini(v any) any {
	return sanitizeSchemaFiltered(v, true)
}

// sanitizeSchemaFiltered is the recursive workhorse. When filterKeys is false,
// all map keys pass through unfiltered (used for user-defined property names
// inside "properties", which must not be checked against the schema-keyword
// allow-list).
func sanitizeSchemaFiltered(v any, filterKeys bool) any {
	switch node := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(node))
		for k, child := range node {
			if filterKeys {
				if _, ok := geminiSchemaAllowedKeys[k]; !ok {
					continue
				}
			}
			if k == "enum" && filterKeys {
				cleaned := filterStringEnum(child)
				if len(cleaned) == 0 {
					continue
				}
				out[k] = cleaned
				continue
			}
			// User-defined property names inside "properties" must not be
			// checked against the schema-keyword allow-list, but their values
			// (the per-property schemas) MUST be filtered. Only apply this
			// special case when filterKeys is true — when it is false we are
			// already inside a properties map and "properties" here is just
			// another user-defined property name whose value is a schema.
			if k == "properties" && filterKeys {
				out[k] = sanitizeSchemaFiltered(child, false)
				continue
			}
			// When we are inside a properties map (filterKeys=false), each value
			// is a JSON Schema. A boolean true/false is valid JSON Schema ("any
			// type" / "reject all") but Gemini's proto Schema rejects both.
			// Convert to empty Schema objects so they survive translation.
			if !filterKeys {
				if _, isBool := child.(bool); isBool {
					out[k] = map[string]any{}
					continue
				}
			}
			// Values of "default" and "example" are arbitrary JSON data, not JSON
			// Schema — pass them through without recursive filtering so object
			// keys like {"host":"localhost","port":8080} are not stripped.
			if (k == "default" || k == "example") && filterKeys {
				out[k] = child
				continue
			}
			out[k] = sanitizeSchemaFiltered(child, true)
		}
		// JSON Schema allows "type": ["array", "null"]; Gemini wants single Type + nullable bool.
		out = collapseTypeArray(out)
		// JSON Schema allows `items` to be a boolean (true = any, false = none).
		// Gemini's proto Schema.items is optional Schema and rejects booleans.
		out = collapseItemsBool(out)
		// Anthropic permits `{"type":"array"}` with no `items`; Gemini's strict
		// function-calling validator rejects it ("missing field"). Inject a
		// permissive default so the tool definition survives translation. Real
		// items schemas from the source were preserved by the loop above.
		if t, ok := out["type"].(string); ok && t == "array" {
			if existing, has := out["items"]; !has || existing == nil {
				out["items"] = map[string]any{"type": "string"}
			}
		}
		return out
	case []any:
		out := make([]any, len(node))
		for i, child := range node {
			out[i] = sanitizeSchemaFiltered(child, true)
		}
		return out
	default:
		return v
	}
}

// collapseTypeArray collapses JSON Schema ["array", "null"] into Gemini's
// single Type + nullable convention. Drops "type" entirely if only "null".
func collapseTypeArray(out map[string]any) map[string]any {
	types, ok := out["type"].([]any)
	if !ok {
		return out
	}
	var primary string
	hasNull := false
	for _, t := range types {
		s, ok := t.(string)
		if !ok {
			continue
		}
		if s == "null" {
			hasNull = true
		} else if primary == "" {
			primary = s
		}
	}
	if primary == "" {
		delete(out, "type")
		return out
	}
	out["type"] = primary
	if hasNull {
		out["nullable"] = true
	}
	return out
}

// collapseItemsBool converts JSON Schema boolean "items" (true = any schema,
// false = no items) into Gemini's Schema-or-null convention. `true` becomes
// an empty Schema (equivalent to "any type"). `false` is removed.
func collapseItemsBool(out map[string]any) map[string]any {
	v, ok := out["items"]
	if !ok {
		return out
	}
	switch v := v.(type) {
	case bool:
		if v {
			out["items"] = map[string]any{}
		} else {
			delete(out, "items")
		}
	}
	return out
}

// filterStringEnum returns enum entries that are non-empty strings. Google's
// function-calling surface requires TYPE_STRING enums and rejects empty-string
// entries ("enum[i]: cannot be empty"); both filter out here. Returns an empty
// slice when nothing survives so the caller can drop the field entirely.
func filterStringEnum(v any) []any {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(arr))
	for _, item := range arr {
		s, ok := item.(string)
		if !ok || s == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}
