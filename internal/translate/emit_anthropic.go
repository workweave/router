package translate

import (
	"fmt"
	"net/http"
	"strings"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// PrepareAnthropic builds an Anthropic Messages request body.
func (e *RequestEnvelope) PrepareAnthropic(in http.Header, opts EmitOptions) (providers.PreparedRequest, error) {
	var body []byte
	var err error
	switch e.format {
	case FormatOpenAI:
		body, err = e.buildAnthropicFromOpenAI(opts)
		if err != nil {
			return providers.PreparedRequest{}, fmt.Errorf("build anthropic from openai: %w", err)
		}
	case FormatAnthropic:
		body, err = e.buildAnthropicFromAnthropic(opts)
		if err != nil {
			return providers.PreparedRequest{}, fmt.Errorf("marshal anthropic body: %w", err)
		}
	default:
		return providers.PreparedRequest{}, fmt.Errorf("unsupported source format for Anthropic emit: %d", e.format)
	}
	return providers.PreparedRequest{Body: body, Headers: deriveAnthropicHeaders(in, opts)}, nil
}

func deriveAnthropicHeaders(in http.Header, opts EmitOptions) http.Header {
	h := make(http.Header)
	if v := in.Get("anthropic-version"); v != "" {
		h.Set("anthropic-version", v)
	} else {
		h.Set("anthropic-version", "2023-06-01")
	}
	if v := filterBetaHeader(in.Get("anthropic-beta"), opts.TargetModel); v != "" {
		h.Set("anthropic-beta", v)
	}
	return h
}

func filterBetaHeader(beta, targetModel string) string {
	if beta == "" {
		return ""
	}
	spec := router.Lookup(targetModel)
	return joinKept(beta, func(token string) bool {
		return betaCompatible(token, spec)
	})
}

func betaCompatible(token string, spec router.ModelSpec) bool {
	if strings.Contains(token, "context-1m") {
		return spec.Supports(router.CapExtendedContext)
	}
	if strings.Contains(token, "interleaved-thinking") ||
		strings.Contains(token, "adaptive-thinking") {
		return spec.Supports(router.CapAdaptiveThinking)
	}
	if strings.Contains(token, "thinking") {
		return spec.Supports(router.CapAdaptiveThinking) ||
			spec.Supports(router.CapExtendedThinking)
	}
	return true
}

func joinKept(beta string, keep func(string) bool) string {
	if beta == "" {
		return ""
	}
	parts := strings.Split(beta, ",")
	kept := parts[:0]
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t == "" || !keep(t) {
			continue
		}
		kept = append(kept, t)
	}
	return strings.Join(kept, ",")
}

func (e *RequestEnvelope) buildAnthropicFromOpenAI(opts EmitOptions) ([]byte, error) {
	jw := newJSONWriter()
	jw.Obj()
	jw.Key("model")
	jw.Str(opts.TargetModel)

	if r := gjson.GetBytes(e.body, "stream"); r.Exists() {
		jw.Key("stream")
		jw.Raw(r.Raw)
	}

	writeAnthropicSystemAndMessages(jw, e.body)
	writeAnthropicMaxTokens(jw, e.body, opts.TargetModel)
	writeAnthropicStopSequences(jw, e.body)

	// tool_choice "none" suppresses tools entirely — Anthropic has no direct
	// equivalent, so omitting tools is the only way to prevent tool calls.
	suppressTools := gjson.GetBytes(e.body, "tool_choice").String() == "none"
	if !suppressTools {
		writeAnthropicTools(jw, e.body)
		writeAnthropicToolChoice(jw, e.body)
	}
	writeAnthropicSharedParams(jw, e.body)

	jw.EndObj()
	return jw.Bytes(), nil
}

// writeAnthropicSystemAndMessages walks the OpenAI messages array, extracts
// system-role messages into the Anthropic "system" field, and writes the
// remaining messages as Anthropic-formatted content.
func writeAnthropicSystemAndMessages(jw *jsonWriter, body []byte) {
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() {
		return
	}

	// Collect system text blocks and non-system message raws in one pass.
	type pendingToolBatch struct {
		active bool
		blocks []string // raw JSON tool_result objects
	}

	var systemBlocks []string
	var msgParts []string // raw JSON message objects, flushed after tool batching
	var pending pendingToolBatch

	flushToolBatch := func() {
		if !pending.active {
			return
		}
		// Emit a single user message with all accumulated tool_result blocks.
		tw := newJSONWriter()
		tw.Obj()
		tw.Key("role")
		tw.Str("user")
		tw.Key("content")
		tw.Arr()
		for _, b := range pending.blocks {
			tw.Raw(b)
		}
		tw.EndArr()
		tw.EndObj()
		msgParts = append(msgParts, string(tw.Bytes()))
		pending.active = false
		pending.blocks = pending.blocks[:0]
	}

	messages.ForEach(func(_, msg gjson.Result) bool {
		role := msg.Get("role").String()
		content := msg.Get("content")

		switch role {
		case "system":
			// Collect text from system messages into the top-level system field.
			if content.Type == gjson.String {
				if s := content.String(); s != "" {
					sb := newJSONWriter()
					sb.Obj()
					sb.Key("type")
					sb.Str("text")
					sb.Key("text")
					sb.Str(s)
					sb.EndObj()
					systemBlocks = append(systemBlocks, string(sb.Bytes()))
				}
			} else if content.IsArray() {
				content.ForEach(func(_, part gjson.Result) bool {
					if part.Get("type").String() == "text" {
						if t := part.Get("text").String(); t != "" {
							sb := newJSONWriter()
							sb.Obj()
							sb.Key("type")
							sb.Str("text")
							sb.Key("text")
							sb.Str(t)
							sb.EndObj()
							systemBlocks = append(systemBlocks, string(sb.Bytes()))
						}
					}
					return true
				})
			}

		case "tool":
			// Merge consecutive tool messages into a single Anthropic user message.
			toolCallID := msg.Get("tool_call_id").String()
			blockRaw := buildToolResultBlock(toolCallID, content)
			pending.active = true
			pending.blocks = append(pending.blocks, blockRaw)

		case "assistant":
			flushToolBatch()
			msgParts = append(msgParts, buildAnthropicAssistantMessage(msg))

		default: // "user" and anything unrecognized
			flushToolBatch()
			msgParts = append(msgParts, buildAnthropicUserMessage(role, content))
		}
		return true
	})
	flushToolBatch()

	if len(systemBlocks) > 0 {
		jw.Key("system")
		jw.Arr()
		for _, b := range systemBlocks {
			jw.Raw(b)
		}
		jw.EndArr()
	}

	if len(msgParts) > 0 {
		jw.Key("messages")
		jw.Arr()
		for _, m := range msgParts {
			jw.Raw(m)
		}
		jw.EndArr()
	}
}

// buildToolResultBlock constructs a single Anthropic tool_result JSON object.
func buildToolResultBlock(toolUseID string, content gjson.Result) string {
	jw := newJSONWriter()
	jw.Obj()
	jw.Key("type")
	jw.Str("tool_result")
	jw.Key("tool_use_id")
	jw.Str(sanitizeToolUseID(toolUseID))

	switch content.Type {
	case gjson.String:
		jw.Key("content")
		jw.Str(content.String())
	case gjson.JSON:
		if content.IsArray() {
			// Walk parts: convert text and image_url to Anthropic content blocks.
			var parts []string
			content.ForEach(func(_, part gjson.Result) bool {
				switch part.Get("type").String() {
				case "text":
					if t := part.Get("text").String(); t != "" {
						pb := newJSONWriter()
						pb.Obj()
						pb.Key("type")
						pb.Str("text")
						pb.Key("text")
						pb.Str(t)
						pb.EndObj()
						parts = append(parts, string(pb.Bytes()))
					}
				case "image_url":
					urlStr := part.Get("image_url.url").String()
					if urlStr != "" {
						parts = append(parts, buildAnthropicImageBlock(urlStr))
					}
				}
				return true
			})
			if len(parts) > 0 {
				jw.Key("content")
				jw.Arr()
				for _, p := range parts {
					jw.Raw(p)
				}
				jw.EndArr()
			} else {
				jw.Key("content")
				jw.Str("")
			}
		} else {
			jw.Key("content")
			jw.Str("")
		}
	default:
		// null or missing content
		jw.Key("content")
		jw.Str("")
	}

	jw.EndObj()
	return string(jw.Bytes())
}

// buildAnthropicAssistantMessage converts an OpenAI assistant message to
// Anthropic format, handling both plain-text content and tool_calls.
func buildAnthropicAssistantMessage(msg gjson.Result) string {
	jw := newJSONWriter()
	jw.Obj()
	jw.Key("role")
	jw.Str("assistant")

	toolCalls := msg.Get("tool_calls")
	if !toolCalls.Exists() || !toolCalls.IsArray() || toolCalls.Get("#").Int() == 0 {
		// No tool calls: content is string or array.
		content := msg.Get("content")
		jw.Key("content")
		writeAnthropicContentValue(jw, content)
		jw.EndObj()
		return string(jw.Bytes())
	}

	// Has tool calls: build content array with optional text prefix + tool_use blocks.
	jw.Key("content")
	jw.Arr()
	if text := msg.Get("content").String(); text != "" {
		jw.Obj()
		jw.Key("type")
		jw.Str("text")
		jw.Key("text")
		jw.Str(text)
		jw.EndObj()
	}
	toolCalls.ForEach(func(_, tc gjson.Result) bool {
		id := sanitizeToolUseID(tc.Get("id").String())
		name := tc.Get("function.name").String()
		argsStr := tc.Get("function.arguments").String()

		jw.Obj()
		jw.Key("type")
		jw.Str("tool_use")
		jw.Key("id")
		jw.Str(id)
		jw.Key("name")
		jw.Str(name)
		jw.Key("input")
		if gjson.Valid(argsStr) {
			jw.Raw(argsStr)
		} else {
			jw.Raw("{}")
		}
		jw.EndObj()
		return true
	})
	jw.EndArr()

	jw.EndObj()
	return string(jw.Bytes())
}

// buildAnthropicUserMessage converts an OpenAI user (or other non-system,
// non-tool, non-assistant) message to Anthropic format.
func buildAnthropicUserMessage(role string, content gjson.Result) string {
	jw := newJSONWriter()
	jw.Obj()
	jw.Key("role")
	jw.Str(role)
	jw.Key("content")
	writeAnthropicContentValue(jw, content)
	jw.EndObj()
	return string(jw.Bytes())
}

// writeAnthropicContentValue writes a content value (string or array) in
// Anthropic format. String content passes through verbatim; array content has
// image_url parts converted to Anthropic image blocks.
func writeAnthropicContentValue(jw *jsonWriter, content gjson.Result) {
	if content.Type == gjson.String {
		jw.Str(content.String())
		return
	}
	if !content.IsArray() {
		// null or other scalar: pass through raw
		if content.Raw != "" {
			jw.Raw(content.Raw)
		} else {
			jw.Null()
		}
		return
	}
	jw.Arr()
	content.ForEach(func(_, part gjson.Result) bool {
		switch part.Get("type").String() {
		case "text":
			jw.Obj()
			jw.Key("type")
			jw.Str("text")
			jw.Key("text")
			jw.Str(part.Get("text").String())
			jw.EndObj()
		case "image_url":
			urlStr := part.Get("image_url.url").String()
			if urlStr != "" {
				jw.Raw(buildAnthropicImageBlock(urlStr))
			}
		}
		return true
	})
	jw.EndArr()
}

// buildAnthropicImageBlock returns a raw JSON Anthropic image content block
// for the given URL string (data: or regular URL).
func buildAnthropicImageBlock(urlStr string) string {
	jw := newJSONWriter()
	jw.Obj()
	jw.Key("type")
	jw.Str("image")
	jw.Key("source")
	jw.Obj()
	if mime, data, ok := parseDataURL(urlStr); ok {
		jw.Key("type")
		jw.Str("base64")
		jw.Key("media_type")
		jw.Str(mime)
		jw.Key("data")
		jw.Str(data)
	} else {
		jw.Key("type")
		jw.Str("url")
		jw.Key("url")
		jw.Str(urlStr)
	}
	jw.EndObj()
	jw.EndObj()
	return string(jw.Bytes())
}

func writeAnthropicMaxTokens(jw *jsonWriter, body []byte, targetModel string) {
	if r := gjson.GetBytes(body, "max_tokens"); r.Exists() {
		jw.Key("max_tokens")
		jw.Raw(r.Raw)
		return
	}
	if r := gjson.GetBytes(body, "max_completion_tokens"); r.Exists() {
		jw.Key("max_tokens")
		jw.Raw(r.Raw)
		return
	}
	jw.Key("max_tokens")
	jw.Int(defaultOutputTokens(targetModel))
}

func writeAnthropicStopSequences(jw *jsonWriter, body []byte) {
	r := gjson.GetBytes(body, "stop")
	if !r.Exists() {
		return
	}
	if r.Type == gjson.String {
		jw.Key("stop_sequences")
		jw.Arr()
		jw.Str(r.String())
		jw.EndArr()
		return
	}
	if r.IsArray() {
		jw.Key("stop_sequences")
		jw.Raw(r.Raw)
	}
}

func writeAnthropicTools(jw *jsonWriter, body []byte) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() || tools.Get("#").Int() == 0 {
		return
	}
	jw.Key("tools")
	jw.Arr()
	tools.ForEach(func(_, tool gjson.Result) bool {
		fn := tool.Get("function")
		if !fn.Exists() {
			return true
		}
		jw.Obj()
		jw.Key("name")
		jw.Str(fn.Get("name").String())
		if desc := fn.Get("description"); desc.Exists() {
			jw.Key("description")
			jw.Raw(desc.Raw)
		}
		if params := fn.Get("parameters"); params.Exists() {
			jw.Key("input_schema")
			jw.Raw(params.Raw)
		}
		jw.EndObj()
		return true
	})
	jw.EndArr()
}

func writeAnthropicToolChoice(jw *jsonWriter, body []byte) {
	r := gjson.GetBytes(body, "tool_choice")
	if !r.Exists() {
		return
	}
	if r.Type == gjson.String {
		switch r.String() {
		case "auto":
			jw.Key("tool_choice")
			jw.Raw(`{"type":"auto"}`)
		case "required":
			jw.Key("tool_choice")
			jw.Raw(`{"type":"any"}`)
		case "none":
			// Handled upstream — tools and tool_choice both suppressed.
		}
		return
	}
	if r.IsObject() {
		if name := r.Get("function.name").String(); name != "" {
			tw := newJSONWriter()
			tw.Obj()
			tw.Key("type")
			tw.Str("tool")
			tw.Key("name")
			tw.Str(name)
			tw.EndObj()
			jw.Key("tool_choice")
			jw.Raw(string(tw.Bytes()))
		}
	}
}

func writeAnthropicSharedParams(jw *jsonWriter, body []byte) {
	for _, key := range []string{"temperature", "top_p", "top_k"} {
		if r := gjson.GetBytes(body, key); r.Exists() {
			jw.Key(key)
			jw.Raw(r.Raw)
		}
	}
}

func (e *RequestEnvelope) buildAnthropicFromAnthropic(opts EmitOptions) ([]byte, error) {
	body, err := hoistAnthropicSystemMessages(e.body)
	if err != nil {
		return nil, fmt.Errorf("hoist system messages: %w", err)
	}
	ov := resolveAnthropicOverrides(body, opts)
	return applyOverrides(body, ov)
}

// hoistAnthropicSystemMessages moves any role:"system" entries out of the
// "messages" array and merges their text into the top-level "system" field.
// Anthropic's Messages API rejects a system role inside messages (400
// "role 'system' must precede an 'assistant' message or end the array"); a
// system-bearing body can reach this same-format emit path on a mid-session
// switch back to an Anthropic model. The OpenAI->Anthropic emit path already
// hoists in writeAnthropicSystemAndMessages; this is the same guarantee for the
// same-format path. No-op when no system message is present.
func hoistAnthropicSystemMessages(body []byte) ([]byte, error) {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return body, nil
	}

	var hoisted []string // text extracted from in-array system messages, in order
	var kept []string    // raw non-system message objects
	for _, msg := range msgs.Array() {
		if msg.Get("role").String() == "system" {
			hoisted = append(hoisted, anthropicSystemTexts(msg.Get("content"))...)
			continue
		}
		kept = append(kept, msg.Raw)
	}
	if len(hoisted) == 0 {
		return body, nil
	}

	// Merge: existing top-level system blocks first, then the hoisted text.
	sw := newJSONWriter()
	sw.Arr()
	switch existing := gjson.GetBytes(body, "system"); {
	case existing.Type == gjson.String:
		if s := existing.String(); s != "" {
			writeAnthropicTextBlock(sw, s)
		}
	case existing.IsArray():
		existing.ForEach(func(_, b gjson.Result) bool {
			sw.Raw(b.Raw)
			return true
		})
	}
	for _, t := range hoisted {
		writeAnthropicTextBlock(sw, t)
	}
	sw.EndArr()

	out, err := sjson.SetRawBytes(body, "messages", []byte("["+strings.Join(kept, ",")+"]"))
	if err != nil {
		return nil, fmt.Errorf("rebuild messages: %w", err)
	}
	out, err = sjson.SetRawBytes(out, "system", sw.Bytes())
	if err != nil {
		return nil, fmt.Errorf("set system: %w", err)
	}
	return out, nil
}

// anthropicSystemTexts extracts text strings from a system message's content,
// which may be a plain string or an array of content blocks.
func anthropicSystemTexts(content gjson.Result) []string {
	if content.Type == gjson.String {
		if s := content.String(); s != "" {
			return []string{s}
		}
		return nil
	}
	if !content.IsArray() {
		return nil
	}
	var out []string
	content.ForEach(func(_, part gjson.Result) bool {
		if part.Get("type").String() == "text" {
			if t := part.Get("text").String(); t != "" {
				out = append(out, t)
			}
		}
		return true
	})
	return out
}

// writeAnthropicTextBlock writes a {"type":"text","text":...} object.
func writeAnthropicTextBlock(jw *jsonWriter, text string) {
	jw.Obj()
	jw.Key("type")
	jw.Str("text")
	jw.Key("text")
	jw.Str(text)
	jw.EndObj()
}

// sanitizeToolUseID replaces characters that Anthropic rejects in tool_use.id
// (required pattern: ^[a-zA-Z0-9_-]+$). Non-Anthropic upstreams (e.g.
// Kimi-k2.6) emit IDs like "functions.Read:0" containing dots and colons; when
// the router switches a session back to Anthropic those IDs cause a 400.
//
// Length is NOT clamped here: this helper is shared by the Anthropic and Gemini
// emit paths, where a Gemini thoughtSignature smuggled into the id by
// embedSignatureInID makes the id intentionally longer than 64 bytes. OpenAI's
// 64-char limit is enforced separately in clampOpenAIToolCallID.
func sanitizeToolUseID(id string) string {
	if id == "" {
		return id
	}
	b := []byte(id)
	changed := false
	for i, c := range b {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			b[i] = '_'
			changed = true
		}
	}
	if !changed {
		return id
	}
	return string(b)
}
