package translate

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// GeminiToOpenAIResponse converts a non-streaming Gemini response to OpenAI format.
func GeminiToOpenAIResponse(body []byte, requestModel string) ([]byte, error) {
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal gemini response: %w", err)
	}

	candidates, _ := resp["candidates"].([]any)
	var first map[string]any
	if len(candidates) > 0 {
		first, _ = candidates[0].(map[string]any)
	}

	textContent, toolCalls, leadingSig := extractGeminiParts(first)
	finishReason := mapGeminiFinishReason(stringField(first, "finishReason"), len(toolCalls) > 0)

	message := map[string]any{"role": "assistant"}
	if textContent != "" {
		message["content"] = textContent
	} else {
		message["content"] = nil
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	} else if leadingSig != "" {
		// Off-spec; litellm/openai-go pass through unknown fields.
		message["thought_signature"] = leadingSig
	}

	out := map[string]any{
		"id":      generateChatCmplID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   requestModel,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": geminiUsageToOpenAI(resp["usageMetadata"]),
	}
	return json.Marshal(out)
}

// GeminiToAnthropicResponse converts a non-streaming Gemini response to
// Anthropic Messages format.
func GeminiToAnthropicResponse(body []byte, requestModel string) ([]byte, error) {
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal gemini response: %w", err)
	}

	candidates, _ := resp["candidates"].([]any)
	var first map[string]any
	if len(candidates) > 0 {
		first, _ = candidates[0].(map[string]any)
	}

	blocks := buildAnthropicBlocksFromGemini(first)
	stopReason := mapGeminiFinishReasonToAnthropic(stringField(first, "finishReason"), blocksContainToolUse(blocks))

	out := map[string]any{
		"id":            generateAnthropicMsgID(),
		"type":          "message",
		"role":          "assistant",
		"model":         requestModel,
		"content":       blocks,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         geminiUsageToAnthropic(resp["usageMetadata"]),
	}
	return json.Marshal(out)
}

// GeminiToOpenAIError re-wraps a Gemini error envelope as an OpenAI error.
func GeminiToOpenAIError(body []byte) []byte {
	var g struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &g); err != nil {
		return body
	}
	if g.Error.Message == "" && g.Error.Status == "" {
		return body
	}
	out, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": g.Error.Message,
			"type":    strings.ToLower(g.Error.Status),
			"param":   nil,
			"code":    g.Error.Code,
		},
	})
	if err != nil {
		return body
	}
	return out
}

// extractGeminiParts walks candidate.content.parts and produces the OpenAI view.
// thoughtSignature is smuggled on function.thought_signature; leadingSig is
// set from the first text part when no functionCalls are present.
func extractGeminiParts(candidate map[string]any) (textContent string, toolCalls []any, leadingSig string) {
	if candidate == nil {
		return "", nil, ""
	}
	content, _ := candidate["content"].(map[string]any)
	parts, _ := content["parts"].([]any)

	var texts []string
	var firstTextSig string
	for _, p := range parts {
		part, _ := p.(map[string]any)
		if part == nil {
			continue
		}
		if fc, ok := part["functionCall"].(map[string]any); ok {
			tc := geminiFunctionCallToToolCall(fc, stringField(part, "thoughtSignature"))
			toolCalls = append(toolCalls, tc)
			continue
		}
		if t, ok := part["text"].(string); ok && t != "" {
			texts = append(texts, t)
			if firstTextSig == "" {
				if sig, _ := part["thoughtSignature"].(string); sig != "" {
					firstTextSig = sig
				}
			}
		}
	}
	textContent = strings.Join(texts, "")
	if len(toolCalls) == 0 {
		leadingSig = firstTextSig
	}
	return textContent, toolCalls, leadingSig
}

func geminiFunctionCallToToolCall(fc map[string]any, signature string) map[string]any {
	name, _ := fc["name"].(string)
	args := fc["args"]
	var argStr string
	if args == nil {
		argStr = "{}"
	} else {
		b, err := json.Marshal(args)
		if err != nil {
			argStr = "{}"
		} else {
			argStr = string(b)
		}
	}
	fn := map[string]any{
		"name":      name,
		"arguments": argStr,
	}
	tc := map[string]any{
		"id":       embedSignatureInID(generateToolCallID(), signature),
		"type":     "function",
		"function": fn,
	}
	if signature != "" {
		// Also smuggled in tc.id for typed SDKs that drop unknown fields.
		fn["thought_signature"] = signature
		tc["thought_signature"] = signature
	}
	return tc
}

func buildAnthropicBlocksFromGemini(candidate map[string]any) []any {
	if candidate == nil {
		return []any{}
	}
	content, _ := candidate["content"].(map[string]any)
	parts, _ := content["parts"].([]any)

	var blocks []any
	for _, p := range parts {
		part, _ := p.(map[string]any)
		if part == nil {
			continue
		}
		if fc, ok := part["functionCall"].(map[string]any); ok {
			sig, _ := part["thoughtSignature"].(string)
			block := map[string]any{
				"type":  "tool_use",
				"id":    embedSignatureInID(generateToolUseID(), sig),
				"name":  fc["name"],
				"input": fc["args"],
			}
			if block["input"] == nil {
				block["input"] = map[string]any{}
			}
			if sig != "" {
				block["thought_signature"] = sig
			}
			blocks = append(blocks, block)
			continue
		}
		if t, ok := part["text"].(string); ok && t != "" {
			block := map[string]any{"type": "text", "text": t}
			if sig, _ := part["thoughtSignature"].(string); sig != "" {
				block["thought_signature"] = sig
			}
			blocks = append(blocks, block)
		}
	}
	if blocks == nil {
		return []any{}
	}
	return blocks
}

func blocksContainToolUse(blocks []any) bool {
	for _, b := range blocks {
		bm, _ := b.(map[string]any)
		if t, _ := bm["type"].(string); t == "tool_use" {
			return true
		}
	}
	return false
}

// mapGeminiFinishReason converts Gemini finishReason to OpenAI finish_reason.
func mapGeminiFinishReason(reason string, hasToolCalls bool) string {
	switch reason {
	case "STOP":
		if hasToolCalls {
			return "tool_calls"
		}
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "OTHER", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "content_filter"
	case "":
		return "stop"
	default:
		return "stop"
	}
}

func mapGeminiFinishReasonToAnthropic(reason string, hasToolUse bool) string {
	switch reason {
	case "STOP":
		if hasToolUse {
			return "tool_use"
		}
		return "end_turn"
	case "MAX_TOKENS":
		return "max_tokens"
	case "SAFETY", "RECITATION", "OTHER", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "stop_sequence"
	default:
		return "end_turn"
	}
}

func geminiUsageToOpenAI(meta any) map[string]any {
	m, _ := meta.(map[string]any)
	if m == nil {
		return map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	}
	prompt := numField(m, "promptTokenCount")
	completion := numField(m, "candidatesTokenCount")
	total := numField(m, "totalTokenCount")
	if total == 0 {
		total = prompt + completion
	}
	return map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      total,
	}
}

func geminiUsageToAnthropic(meta any) map[string]any {
	m, _ := meta.(map[string]any)
	if m == nil {
		return map[string]any{"input_tokens": 0, "output_tokens": 0}
	}
	return map[string]any{
		"input_tokens":  numField(m, "promptTokenCount"),
		"output_tokens": numField(m, "candidatesTokenCount"),
	}
}

func numField(m map[string]any, k string) int {
	v, ok := m[k]
	if !ok {
		return 0
	}
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

func stringField(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return s
}

func generateChatCmplID() string {
	return "chatcmpl-" + randomHex(8)
}

func generateToolCallID() string {
	return "call_" + randomHex(4)
}

func generateAnthropicMsgID() string {
	return "msg_" + randomHex(8)
}

func generateToolUseID() string {
	return "toolu_" + randomHex(8)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(buf)
}
