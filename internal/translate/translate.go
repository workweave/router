// Package translate converts request and response bodies between the OpenAI
// Chat Completions and Anthropic Messages wire formats. Request-side
// translation lives in RequestEnvelope (envelope.go, emit_*.go); this file
// retains response-side helpers.
package translate

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// AnthropicToOpenAIResponse converts a non-streaming Anthropic Messages response
// to an OpenAI Chat Completion response. requestModel is the fallback when the
// upstream body omits a model field.
func AnthropicToOpenAIResponse(body []byte, requestModel string) ([]byte, error) {
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal anthropic response: %w", err)
	}

	msgID, _ := resp["id"].(string)
	model, _ := resp["model"].(string)
	if model == "" {
		model = requestModel
	}

	message := buildOpenAIMessage(resp)
	stopReason := mapStopReason(resp["stop_reason"])

	usage := translateUsage(resp["usage"])

	out := map[string]any{
		"id":      msgID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": stopReason,
			},
		},
		"usage": usage,
	}
	return json.Marshal(out)
}

func buildOpenAIMessage(resp map[string]any) map[string]any {
	content, _ := resp["content"].([]any)
	message := map[string]any{"role": "assistant"}

	var textParts []string
	var toolCalls []any

	for _, block := range content {
		b, ok := block.(map[string]any)
		if !ok {
			continue
		}
		bType, _ := b["type"].(string)
		switch bType {
		case "text":
			if text, _ := b["text"].(string); text != "" {
				textParts = append(textParts, text)
			}
		case "tool_use":
			input, _ := json.Marshal(b["input"])
			id, _ := b["id"].(string)
			name, _ := b["name"].(string)
			// Non-streaming tool_calls omit `index`; that field is for streaming deltas.
			toolCalls = append(toolCalls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": string(input),
				},
			})
		}
	}

	if len(textParts) > 0 {
		message["content"] = strings.Join(textParts, "\n")
	} else {
		message["content"] = nil
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	return message
}

// mapStopReason converts an Anthropic stop_reason to an OpenAI finish_reason.
func mapStopReason(reason any) string {
	s, _ := reason.(string)
	switch s {
	case "end_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

// AnthropicToOpenAIError re-wraps an Anthropic error as OpenAI format. Returns
// the input unchanged when it isn't a valid Anthropic error envelope.
func AnthropicToOpenAIError(body []byte) []byte {
	var anthropic struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &anthropic); err != nil {
		return body
	}
	if anthropic.Error.Type == "" && anthropic.Error.Message == "" {
		return body
	}
	out, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": anthropic.Error.Message,
			"type":    anthropic.Error.Type,
			"param":   nil,
			"code":    nil,
		},
	})
	if err != nil {
		return body
	}
	return out
}

func translateUsage(usage any) map[string]any {
	u, _ := usage.(map[string]any)
	if u == nil {
		return map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	}
	input, _ := u["input_tokens"].(float64)
	output, _ := u["output_tokens"].(float64)
	return map[string]any{
		"prompt_tokens":     int(input),
		"completion_tokens": int(output),
		"total_tokens":      int(input + output),
	}
}

// OpenAIToAnthropicResponse converts a non-streaming OpenAI Chat Completion
// response to an Anthropic Messages response. requestModel is the fallback when
// the upstream body omits a model field.
func OpenAIToAnthropicResponse(body []byte, requestModel string) ([]byte, error) {
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal openai response: %w", err)
	}

	id, _ := resp["id"].(string)
	if id == "" {
		id = "msg_translated"
	}
	model, _ := resp["model"].(string)
	if model == "" {
		model = requestModel
	}

	choices, _ := resp["choices"].([]any)
	var firstChoice map[string]any
	if len(choices) > 0 {
		firstChoice, _ = choices[0].(map[string]any)
	}
	message, _ := firstChoice["message"].(map[string]any)

	content := buildAnthropicContent(message)
	stopReason := openAIFinishToAnthropicResponse(firstChoice["finish_reason"])

	usage := openAIUsageToAnthropicResponse(resp["usage"])

	out := map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         usage,
	}
	return json.Marshal(out)
}

func buildAnthropicContent(message map[string]any) []any {
	if message == nil {
		return []any{}
	}
	var blocks []any
	if text, _ := message["content"].(string); text != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}
	toolCalls, _ := message["tool_calls"].([]any)
	for _, tc := range toolCalls {
		call, _ := tc.(map[string]any)
		if call == nil {
			continue
		}
		fn, _ := call["function"].(map[string]any)
		id, _ := call["id"].(string)
		name, _ := fn["name"].(string)
		argsStr, _ := fn["arguments"].(string)
		var input any
		if json.Unmarshal([]byte(argsStr), &input) != nil {
			input = map[string]any{}
		}
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  name,
			"input": input,
		})
	}
	if blocks == nil {
		blocks = []any{}
	}
	return blocks
}

// openAIFinishToAnthropicResponse is the non-streaming variant; it takes an
// `any` directly out of the JSON map.
func openAIFinishToAnthropicResponse(reason any) string {
	s, _ := reason.(string)
	switch s {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "stop", "":
		return "end_turn"
	default:
		return "end_turn"
	}
}

func openAIUsageToAnthropicResponse(usage any) map[string]any {
	u, _ := usage.(map[string]any)
	if u == nil {
		return map[string]any{"input_tokens": 0, "output_tokens": 0}
	}
	prompt, _ := u["prompt_tokens"].(float64)
	completion, _ := u["completion_tokens"].(float64)
	out := map[string]any{
		"input_tokens":  int(prompt),
		"output_tokens": int(completion),
	}
	if details, _ := u["prompt_tokens_details"].(map[string]any); details != nil {
		if cr, _ := details["cached_tokens"].(float64); cr > 0 {
			out["cache_read_input_tokens"] = int(cr)
		}
	}
	return out
}

// OpenAIToAnthropicError re-wraps an OpenAI error as Anthropic format. Returns
// the input unchanged when it isn't a valid OpenAI error envelope.
func OpenAIToAnthropicError(body []byte) []byte {
	var openai struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &openai); err != nil {
		return body
	}
	if openai.Error.Type == "" && openai.Error.Message == "" {
		return body
	}
	out, err := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    openai.Error.Type,
			"message": openai.Error.Message,
		},
	})
	if err != nil {
		return body
	}
	return out
}
