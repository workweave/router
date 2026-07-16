package translate

import (
	"strconv"

	"github.com/tidwall/gjson"
)

// buildAnthropicFromGemini converts a native Gemini generateContent body into
// an Anthropic Messages request. Product Gemini→non-Google dispatch remains
// deferred at the proxy layer.
func (e *RequestEnvelope) buildAnthropicFromGemini(opts EmitOptions) ([]byte, error) {
	jw := newJSONWriter()
	jw.Obj()
	jw.Key("model")
	jw.Str(opts.TargetModel)

	if r := gjson.GetBytes(e.body, "stream"); r.Exists() {
		jw.Key("stream")
		jw.Raw(r.Raw)
	}

	writeAnthropicFromGeminiContents(jw, e.body)
	writeAnthropicMaxTokens(jw, e.body, opts.TargetModel)

	jw.EndObj()
	return jw.Bytes(), nil
}

// writeAnthropicFromGeminiContents maps systemInstruction + contents[] onto
// Anthropic system + messages. functionCall/functionResponse become
// tool_use/tool_result with synthetic ids paired by tool name order.
func writeAnthropicFromGeminiContents(jw *jsonWriter, body []byte) {
	if sys := geminiSystemText(body); sys != "" {
		sb := newJSONWriter()
		sb.Obj()
		sb.Key("type")
		sb.Str("text")
		sb.Key("text")
		sb.Str(sys)
		sb.EndObj()
		jw.Key("system")
		jw.Arr()
		jw.Raw(string(sb.Bytes()))
		jw.EndArr()
	}

	contents := gjson.GetBytes(body, "contents")
	if !contents.IsArray() {
		return
	}

	// name → most recent synthetic tool_use id, so functionResponse can pair.
	toolIDsByName := make(map[string]string)
	toolSeq := 0

	jw.Key("messages")
	jw.Arr()
	contents.ForEach(func(_, entry gjson.Result) bool {
		role := entry.Get("role").String()
		parts := entry.Get("parts")
		switch role {
		case "model":
			raw, next := buildAnthropicAssistantFromGeminiParts(parts, toolIDsByName, toolSeq)
			toolSeq = next
			if raw != "" {
				jw.Raw(raw)
			}
		default: // "user", empty, or unrecognized — treat as user
			if raw := buildAnthropicUserFromGeminiParts(parts, toolIDsByName); raw != "" {
				jw.Raw(raw)
			}
		}
		return true
	})
	jw.EndArr()
}

func buildAnthropicAssistantFromGeminiParts(parts gjson.Result, toolIDsByName map[string]string, toolSeq int) (string, int) {
	if !parts.IsArray() {
		return "", toolSeq
	}
	var blocks []string
	parts.ForEach(func(_, part gjson.Result) bool {
		if text := part.Get("text").String(); text != "" {
			blocks = append(blocks, anthropicTextBlock(text))
			return true
		}
		call := part.Get("functionCall")
		if !call.Exists() {
			call = part.Get("function_call")
		}
		if !call.Exists() {
			return true
		}
		name := call.Get("name").String()
		if name == "" {
			return true
		}
		id := sanitizeToolUseID("gemini_" + name + "_" + strconv.Itoa(toolSeq))
		toolSeq++
		toolIDsByName[name] = id
		args := call.Get("args")
		if !args.Exists() {
			args = call.Get("arguments")
		}
		blocks = append(blocks, anthropicToolUseBlock(id, name, args))
		return true
	})
	if len(blocks) == 0 {
		return "", toolSeq
	}
	jw := newJSONWriter()
	jw.Obj()
	jw.Key("role")
	jw.Str("assistant")
	jw.Key("content")
	jw.Arr()
	for _, b := range blocks {
		jw.Raw(b)
	}
	jw.EndArr()
	jw.EndObj()
	return string(jw.Bytes()), toolSeq
}

func buildAnthropicUserFromGeminiParts(parts gjson.Result, toolIDsByName map[string]string) string {
	if !parts.IsArray() {
		return ""
	}
	var blocks []string
	parts.ForEach(func(_, part gjson.Result) bool {
		if text := part.Get("text").String(); text != "" {
			blocks = append(blocks, anthropicTextBlock(text))
			return true
		}
		resp := part.Get("functionResponse")
		if !resp.Exists() {
			resp = part.Get("function_response")
		}
		if !resp.Exists() {
			return true
		}
		name := resp.Get("name").String()
		id := toolIDsByName[name]
		if id == "" {
			// No matching prior functionCall — still emit a tool_result with the tool output.
			id = sanitizeToolUseID("gemini_" + name + "_orphan")
		}
		blocks = append(blocks, anthropicToolResultBlock(id, geminiFunctionResponseText(resp)))
		return true
	})
	if len(blocks) == 0 {
		return ""
	}
	jw := newJSONWriter()
	jw.Obj()
	jw.Key("role")
	jw.Str("user")
	jw.Key("content")
	jw.Arr()
	for _, b := range blocks {
		jw.Raw(b)
	}
	jw.EndArr()
	jw.EndObj()
	return string(jw.Bytes())
}

func anthropicTextBlock(text string) string {
	jw := newJSONWriter()
	jw.Obj()
	jw.Key("type")
	jw.Str("text")
	jw.Key("text")
	jw.Str(text)
	jw.EndObj()
	return string(jw.Bytes())
}

func anthropicToolUseBlock(id, name string, args gjson.Result) string {
	jw := newJSONWriter()
	jw.Obj()
	jw.Key("type")
	jw.Str("tool_use")
	jw.Key("id")
	jw.Str(id)
	jw.Key("name")
	jw.Str(name)
	jw.Key("input")
	if args.Exists() && args.IsObject() {
		jw.Raw(args.Raw)
	} else {
		jw.Raw("{}")
	}
	jw.EndObj()
	return string(jw.Bytes())
}

func anthropicToolResultBlock(toolUseID, content string) string {
	jw := newJSONWriter()
	jw.Obj()
	jw.Key("type")
	jw.Str("tool_result")
	jw.Key("tool_use_id")
	jw.Str(sanitizeToolUseID(toolUseID))
	jw.Key("content")
	jw.Str(content)
	jw.EndObj()
	return string(jw.Bytes())
}
