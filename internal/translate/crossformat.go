package translate

import (
	"strings"

	"github.com/tidwall/gjson"
)

// parseDataURL extracts mime type and base64 data from a data:{mime};base64,{data} URL.
// Returns ok=false if the URL doesn't match that pattern. Base64 content is not validated.
func parseDataURL(url string) (mime, data string, ok bool) {
	if !strings.HasPrefix(url, "data:") {
		return "", "", false
	}
	rest := strings.TrimPrefix(url, "data:")
	mime, data, ok = strings.Cut(rest, ";base64,")
	if !ok || data == "" {
		return "", "", false
	}
	return mime, data, true
}

// extractThoughtSignature extracts the thoughtSignature from a gjson Result
// representing a tool call or content block. Checks three sources in priority order:
//  1. part.thought_signature — explicit field
//  2. part.function.thought_signature — nested in function object (OpenAI tool_calls shape)
//  3. part.id — signature smuggled via embedSignatureInID
func extractThoughtSignature(part gjson.Result) string {
	if sig := part.Get("thought_signature").String(); sig != "" {
		return sig
	}
	if sig := part.Get("function.thought_signature").String(); sig != "" {
		return sig
	}
	if id := part.Get("id").String(); id != "" {
		if _, sig := extractSignatureFromID(id); sig != "" {
			return sig
		}
	}
	return ""
}

// toolResultContent flattens OpenAI/Anthropic tool message content to a single string.
// Callers that work with map[string]any (emit_openai, emit_gemini) use this form.
func toolResultContent(raw any) string {
	switch c := raw.(type) {
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
				if text, _ := pm["text"].(string); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// toolResultContentGJSON flattens tool result content to a single string.
// It is the gjson-based equivalent of toolResultContent(raw any).
func toolResultContentGJSON(content gjson.Result) string {
	switch content.Type {
	case gjson.String:
		return content.String()
	case gjson.JSON:
		if !content.IsArray() {
			return ""
		}
		var parts []string
		content.ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() == "text" {
				if text := block.Get("text").String(); text != "" {
					parts = append(parts, text)
				}
			}
			return true
		})
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}
