package translate_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	benchAnthropicStream []byte
	benchOpenAIStream    []byte
)

func init() {
	var b []byte
	b = append(b, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_bench\",\"model\":\"claude-sonnet-4-20250514\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":100,\"output_tokens\":1}}}\n\n"...)
	b = append(b, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"...)
	for i := 0; i < 50; i++ {
		b = append(b, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello \"}}\n\n"...)
	}
	b = append(b, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"...)
	b = append(b, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":50}}\n\n"...)
	b = append(b, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"...)
	benchAnthropicStream = b

	var o []byte
	o = append(o, "data: {\"id\":\"chatcmpl-bench\",\"object\":\"chat.completion.chunk\",\"created\":1234567890,\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n"...)
	for i := 0; i < 50; i++ {
		o = append(o, "data: {\"id\":\"chatcmpl-bench\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello \"},\"finish_reason\":null}]}\n\n"...)
	}
	o = append(o, "data: {\"id\":\"chatcmpl-bench\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":50,\"total_tokens\":150}}\n\n"...)
	o = append(o, "data: [DONE]\n\n"...)
	benchOpenAIStream = o
}

func TestTranslateResponse_NonStreaming(t *testing.T) {
	anthropicResp := `{
		"id": "msg_abc",
		"type": "message",
		"role": "assistant",
		"content": [{"type": "text", "text": "Hello!"}],
		"model": "claude-sonnet-4-20250514",
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 10, "output_tokens": 5}
	}`

	out, err := translate.AnthropicToOpenAIResponse([]byte(anthropicResp), "claude-sonnet-4-20250514")
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(out, &doc))

	assert.Equal(t, "msg_abc", doc["id"])
	assert.Equal(t, "chat.completion", doc["object"])
	assert.Equal(t, "claude-sonnet-4-20250514", doc["model"])

	choices, _ := doc["choices"].([]any)
	require.Len(t, choices, 1)
	choice, _ := choices[0].(map[string]any)
	assert.Equal(t, "stop", choice["finish_reason"])

	message, _ := choice["message"].(map[string]any)
	assert.Equal(t, "assistant", message["role"])
	assert.Equal(t, "Hello!", message["content"])

	usage, _ := doc["usage"].(map[string]any)
	assert.Equal(t, float64(10), usage["prompt_tokens"])
	assert.Equal(t, float64(5), usage["completion_tokens"])
	assert.Equal(t, float64(15), usage["total_tokens"])
}

func TestTranslateResponse_ToolUse(t *testing.T) {
	anthropicResp := `{
		"id": "msg_abc",
		"type": "message",
		"role": "assistant",
		"content": [
			{"type": "text", "text": "Let me check."},
			{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"location": "SF"}}
		],
		"model": "claude-sonnet-4-20250514",
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 50, "output_tokens": 30}
	}`

	out, err := translate.AnthropicToOpenAIResponse([]byte(anthropicResp), "claude-sonnet-4-20250514")
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(out, &doc))

	choices, _ := doc["choices"].([]any)
	require.Len(t, choices, 1)
	choice, _ := choices[0].(map[string]any)
	assert.Equal(t, "tool_calls", choice["finish_reason"])

	message, _ := choice["message"].(map[string]any)
	assert.Equal(t, "Let me check.", message["content"])

	toolCalls, _ := message["tool_calls"].([]any)
	require.Len(t, toolCalls, 1)
	tc, _ := toolCalls[0].(map[string]any)
	assert.Equal(t, "toolu_1", tc["id"])
	assert.Equal(t, "function", tc["type"])
	assert.NotContains(t, tc, "index",
		"OpenAI's non-streaming tool_calls only have id/type/function; index is for streaming deltas")
	fn, _ := tc["function"].(map[string]any)
	assert.Equal(t, "get_weather", fn["name"])
	assert.Contains(t, fn["arguments"], "SF")
}

func TestTranslateResponse_StopReasons(t *testing.T) {
	tests := []struct {
		anthropic string
		openai    string
	}{
		{"end_turn", "stop"},
		{"max_tokens", "length"},
		{"stop_sequence", "stop"},
		{"tool_use", "tool_calls"},
	}
	for _, tt := range tests {
		body := `{"id":"x","content":[{"type":"text","text":"hi"}],"model":"m","stop_reason":"` + tt.anthropic + `"}`
		out, err := translate.AnthropicToOpenAIResponse([]byte(body), "m")
		require.NoError(t, err)
		var doc map[string]any
		require.NoError(t, json.Unmarshal(out, &doc))
		choices, _ := doc["choices"].([]any)
		require.Len(t, choices, 1)
		choice, _ := choices[0].(map[string]any)
		assert.Equal(t, tt.openai, choice["finish_reason"], "anthropic %q must map to openai %q", tt.anthropic, tt.openai)
	}
}

func TestSSETranslator_StreamingTextResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	translator := translate.NewSSETranslator(rec, "claude-sonnet-4-20250514", nil)

	translator.Header().Set("Content-Type", "text/event-stream")
	translator.WriteHeader(http.StatusOK)

	events := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-sonnet-4-20250514\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\" world\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}

	for _, event := range events {
		_, err := translator.Write([]byte(event))
		require.NoError(t, err)
	}

	require.NoError(t, translator.Finalize())

	body := rec.Body.String()
	assert.Contains(t, body, `"object":"chat.completion.chunk"`)
	assert.Contains(t, body, `"role":"assistant"`)
	assert.Contains(t, body, `"content":"Hello"`)
	assert.Contains(t, body, `"content":" world"`)
	assert.Contains(t, body, `"finish_reason":"stop"`)
	assert.Contains(t, body, "data: [DONE]")
	assert.Contains(t, body, `"msg_1"`)
}

func TestSSETranslator_StreamingToolUse(t *testing.T) {
	rec := httptest.NewRecorder()
	translator := translate.NewSSETranslator(rec, "claude-sonnet-4-20250514", nil)

	translator.Header().Set("Content-Type", "text/event-stream")
	translator.WriteHeader(http.StatusOK)

	events := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_2\",\"model\":\"claude-sonnet-4-20250514\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":20,\"output_tokens\":1}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"get_weather\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"loc\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"ation\\\":\\\"SF\\\"}\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":15}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}

	for _, event := range events {
		_, err := translator.Write([]byte(event))
		require.NoError(t, err)
	}

	require.NoError(t, translator.Finalize())

	body := rec.Body.String()
	assert.Contains(t, body, `"tool_calls"`)
	assert.Contains(t, body, `"toolu_1"`)
	assert.Contains(t, body, `"get_weather"`)
	assert.Contains(t, body, `"finish_reason":"tool_calls"`)
	assert.Contains(t, body, "data: [DONE]")
}

func TestSSETranslator_StreamingMultipleToolUse(t *testing.T) {
	rec := httptest.NewRecorder()
	translator := translate.NewSSETranslator(rec, "claude-sonnet-4-20250514", nil)

	translator.Header().Set("Content-Type", "text/event-stream")
	translator.WriteHeader(http.StatusOK)

	events := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_3\",\"model\":\"claude-sonnet-4-20250514\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":20,\"output_tokens\":1}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_a\",\"name\":\"get_weather\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"loc\\\":\\\"SF\\\"}\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_b\",\"name\":\"get_weather\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"loc\\\":\\\"NYC\\\"}\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":30}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	for _, event := range events {
		_, err := translator.Write([]byte(event))
		require.NoError(t, err)
	}
	require.NoError(t, translator.Finalize())

	indexes := collectToolCallIndexes(t, rec.Body.Bytes(), "toolu_a", "toolu_b")
	assert.Equal(t, []int{0, 0}, indexes["toolu_a"],
		"toolu_a's start chunk and its single input_json_delta must both be index=0")
	assert.Equal(t, []int{1, 1}, indexes["toolu_b"],
		"toolu_b's start chunk and its single input_json_delta must both be index=1; emitting both as index=0 would make OpenAI clients merge them into one call")
}

func collectToolCallIndexes(t *testing.T, body []byte, ids ...string) map[string][]int {
	t.Helper()
	out := make(map[string][]int, len(ids))
	for _, id := range ids {
		out[id] = nil
	}
	var currentID string
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		raw := strings.TrimPrefix(line, "data: ")
		if raw == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
			continue
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		toolCalls, _ := delta["tool_calls"].([]any)
		if len(toolCalls) == 0 {
			continue
		}
		tc, _ := toolCalls[0].(map[string]any)
		idxFloat, _ := tc["index"].(float64)
		if id, _ := tc["id"].(string); id != "" {
			currentID = id
		}
		if currentID != "" {
			out[currentID] = append(out[currentID], int(idxFloat))
		}
	}
	return out
}

func TestSSETranslator_NonStreamingResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	translator := translate.NewSSETranslator(rec, "claude-sonnet-4-20250514", nil)

	translator.Header().Set("Content-Type", "application/json")
	translator.WriteHeader(http.StatusOK)

	anthropicResp := `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"Hello!"}],"model":"claude-sonnet-4-20250514","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`
	_, err := translator.Write([]byte(anthropicResp))
	require.NoError(t, err)

	require.NoError(t, translator.Finalize())

	var doc map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
	assert.Equal(t, "chat.completion", doc["object"])
	assert.Equal(t, "msg_1", doc["id"])

	choices, _ := doc["choices"].([]any)
	require.Len(t, choices, 1)
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	assert.Equal(t, "Hello!", message["content"])
}

func TestSSETranslator_TranslatesErrorBodyToOpenAIShape(t *testing.T) {
	rec := httptest.NewRecorder()
	translator := translate.NewSSETranslator(rec, "claude-sonnet-4-20250514", nil)

	translator.Header().Set("Content-Type", "application/json")
	translator.WriteHeader(http.StatusBadRequest)

	errBody := `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens must be positive"}}`
	_, err := translator.Write([]byte(errBody))
	require.NoError(t, err)

	require.NoError(t, translator.Finalize())

	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got),
		"error body must be valid JSON in the inbound (OpenAI) shape")

	errObj, ok := got["error"].(map[string]any)
	require.True(t, ok, "OpenAI errors live under top-level `error`, not `error.error`")
	assert.Equal(t, "max_tokens must be positive", errObj["message"])
	assert.Equal(t, "invalid_request_error", errObj["type"])
	assert.Contains(t, errObj, "param", "OpenAI error shape requires `param`")
	assert.Contains(t, errObj, "code", "OpenAI error shape requires `code`")
	assert.NotContains(t, got, "type", "Anthropic top-level `type:\"error\"` must not leak through")
}

func TestSSETranslator_StreamingErrorAlsoTranslated(t *testing.T) {
	rec := httptest.NewRecorder()
	translator := translate.NewSSETranslator(rec, "claude-opus-4-7", nil)

	translator.Header().Set("Content-Type", "text/event-stream")
	translator.WriteHeader(http.StatusTooManyRequests)

	errBody := `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`
	_, err := translator.Write([]byte(errBody))
	require.NoError(t, err)
	require.NoError(t, translator.Finalize())

	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	errObj, _ := got["error"].(map[string]any)
	require.NotNil(t, errObj)
	assert.Equal(t, "slow down", errObj["message"])
	assert.Equal(t, "rate_limit_error", errObj["type"])
}

func TestSSETranslator_DropsStaleContentLengthAndEncoding(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		statusCode  int
		body        string
	}{
		{
			name:        "non-streaming success",
			contentType: "application/json",
			statusCode:  http.StatusOK,
			body:        `{"id":"msg_1","content":[{"type":"text","text":"hi"}],"model":"m","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
		},
		{
			name:        "non-streaming error",
			contentType: "application/json",
			statusCode:  http.StatusBadRequest,
			body:        `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`,
		},
		{
			name:        "streaming success",
			contentType: "text/event-stream",
			statusCode:  http.StatusOK,
			body:        "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			translator := translate.NewSSETranslator(rec, "claude-opus-4-7", nil)

			translator.Header().Set("Content-Type", tc.contentType)
			translator.Header().Set("Content-Length", "9999")
			translator.Header().Set("Content-Encoding", "gzip")

			translator.WriteHeader(tc.statusCode)
			_, err := translator.Write([]byte(tc.body))
			require.NoError(t, err)
			require.NoError(t, translator.Finalize())

			assert.Empty(t, rec.Header().Get("Content-Length"),
				"upstream Content-Length is stale once the body is re-encoded; must be deleted so net/http computes the right value")
			assert.Empty(t, rec.Header().Get("Content-Encoding"),
				"upstream Content-Encoding is meaningless once we re-encode")
		})
	}
}

func TestSSETranslator_NonAnthropicErrorBodyPassesThrough(t *testing.T) {
	rec := httptest.NewRecorder()
	translator := translate.NewSSETranslator(rec, "claude-opus-4-7", nil)

	translator.Header().Set("Content-Type", "application/json")
	translator.WriteHeader(http.StatusBadGateway)

	body := `<html><body>502 Bad Gateway</body></html>`
	_, err := translator.Write([]byte(body))
	require.NoError(t, err)
	require.NoError(t, translator.Finalize())

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Equal(t, body, rec.Body.String(),
		"unparseable error bodies must pass through verbatim so operators see real upstream messages")
}

func TestSSETranslator_PartialEventBuffering(t *testing.T) {
	rec := httptest.NewRecorder()
	translator := translate.NewSSETranslator(rec, "claude-sonnet-4-20250514", nil)

	translator.Header().Set("Content-Type", "text/event-stream")
	translator.WriteHeader(http.StatusOK)

	fullEvent := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-sonnet-4-20250514\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n"

	_, err := translator.Write([]byte(fullEvent[:30]))
	require.NoError(t, err)
	assert.Empty(t, rec.Body.String(), "partial event must not produce output")

	_, err = translator.Write([]byte(fullEvent[30:]))
	require.NoError(t, err)
	assert.Contains(t, rec.Body.String(), `"chat.completion.chunk"`, "complete event must produce output")
}

func TestAnthropicSSETranslator_StreamingTextResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	translator := translate.NewAnthropicSSETranslator(rec, "gpt-4o", nil)

	translator.Header().Set("Content-Type", "text/event-stream")
	translator.WriteHeader(http.StatusOK)

	events := []string{
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1234567890,\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n",
		"data: [DONE]\n\n",
	}

	for _, event := range events {
		_, err := translator.Write([]byte(event))
		require.NoError(t, err)
	}

	require.NoError(t, translator.Finalize())

	body := rec.Body.String()
	assert.Contains(t, body, "event: message_start")
	assert.Contains(t, body, `"chatcmpl-1"`)
	assert.Contains(t, body, `"type":"text_delta"`)
	assert.Contains(t, body, `"text":"Hello"`)
	assert.Contains(t, body, `"text":" world"`)
	assert.Contains(t, body, `"stop_reason":"end_turn"`)
	assert.Contains(t, body, "event: message_stop")
}

func TestAnthropicSSETranslator_StreamingToolUse(t *testing.T) {
	rec := httptest.NewRecorder()
	translator := translate.NewAnthropicSSETranslator(rec, "gpt-4o", nil)

	translator.Header().Set("Content-Type", "text/event-stream")
	translator.WriteHeader(http.StatusOK)

	events := []string{
		"data: {\"id\":\"chatcmpl-2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":null,\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"chatcmpl-2\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"loc\"}}]},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"chatcmpl-2\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"ation\\\":\\\"SF\\\"}\"}}]},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"chatcmpl-2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":15,\"total_tokens\":35}}\n\n",
		"data: [DONE]\n\n",
	}

	for _, event := range events {
		_, err := translator.Write([]byte(event))
		require.NoError(t, err)
	}

	require.NoError(t, translator.Finalize())

	body := rec.Body.String()
	assert.Contains(t, body, "event: message_start")
	assert.Contains(t, body, `"type":"tool_use"`)
	assert.Contains(t, body, `"call_1"`)
	assert.Contains(t, body, `"get_weather"`)
	assert.Contains(t, body, `"type":"input_json_delta"`)
	assert.Contains(t, body, `"stop_reason":"tool_use"`)
	assert.Contains(t, body, "event: message_stop")
}

func TestAnthropicSSETranslator_NonStreamingResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	translator := translate.NewAnthropicSSETranslator(rec, "gpt-4o", nil)

	translator.Header().Set("Content-Type", "application/json")
	translator.WriteHeader(http.StatusOK)

	openAIResp := `{"id":"chatcmpl-1","object":"chat.completion","created":1234567890,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"Hello!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	_, err := translator.Write([]byte(openAIResp))
	require.NoError(t, err)

	require.NoError(t, translator.Finalize())

	var doc map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
	assert.Equal(t, "message", doc["type"])
	assert.Equal(t, "assistant", doc["role"])
	assert.Equal(t, "end_turn", doc["stop_reason"])

	content, _ := doc["content"].([]any)
	require.Len(t, content, 1)
	block, _ := content[0].(map[string]any)
	assert.Equal(t, "text", block["type"])
	assert.Equal(t, "Hello!", block["text"])
}

func TestAnthropicSSETranslator_EmptyStreamEmitsSyntheticMessage(t *testing.T) {
	rec := httptest.NewRecorder()
	translator := translate.NewAnthropicSSETranslator(rec, "gpt-4o", nil)

	translator.Header().Set("Content-Type", "text/event-stream")
	translator.WriteHeader(http.StatusOK)

	_, err := translator.Write([]byte("data: [DONE]\n\n"))
	require.NoError(t, err)
	require.NoError(t, translator.Finalize())

	body := rec.Body.String()
	assert.Contains(t, body, "event: message_start")
	assert.Contains(t, body, "event: message_delta")
	assert.Contains(t, body, "event: message_stop")
}

func TestAnthropicSSETranslator_RoutingMarkerEmittedBeforeUpstreamContent(t *testing.T) {
	rec := httptest.NewRecorder()
	marker := "✦ **Weave Router** → deepseek/deepseek-v4-pro (openrouter) · reason: switched to save on cache reads\n\n"
	translator := translate.NewAnthropicSSETranslator(rec, "gpt-4o", nil).
		WithRoutingMarker(marker)

	translator.Header().Set("Content-Type", "text/event-stream")
	translator.WriteHeader(http.StatusOK)

	events := []string{
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
		"data: [DONE]\n\n",
	}
	for _, e := range events {
		_, err := translator.Write([]byte(e))
		require.NoError(t, err)
	}
	require.NoError(t, translator.Finalize())

	body := rec.Body.String()
	// Marker's trailing newlines get JSON-escaped in the SSE data field;
	// match the prose only.
	markerProse := "✦ **Weave Router** → deepseek/deepseek-v4-pro (openrouter) · reason: switched to save on cache reads"
	startIdx := strings.Index(body, "event: message_start")
	markerIdx := strings.Index(body, markerProse)
	helloIdx := strings.Index(body, `"text":"Hello"`)
	require.GreaterOrEqual(t, startIdx, 0, "message_start must be emitted")
	require.GreaterOrEqual(t, markerIdx, 0, "marker text must appear in the stream")
	require.GreaterOrEqual(t, helloIdx, 0, "upstream content must appear in the stream")
	assert.Less(t, startIdx, markerIdx, "marker must follow message_start")
	assert.Less(t, markerIdx, helloIdx, "marker must precede the upstream's first text delta")

	// Marker takes index 0; upstream content shifts to index 1.
	idx0Start := strings.Index(body, `"index":0,"content_block":{"type":"text"`)
	idx1Start := strings.Index(body, `"index":1,"content_block":{"type":"text"`)
	require.GreaterOrEqual(t, idx0Start, 0, "marker should open content_block at index 0")
	require.GreaterOrEqual(t, idx1Start, 0, "upstream content should open content_block at index 1")
	assert.Less(t, idx0Start, idx1Start)
}

func TestAnthropicSSETranslator_RoutingMarkerEmittedOnEmptyUpstream(t *testing.T) {
	rec := httptest.NewRecorder()
	marker := "✦ **Weave Router** → gpt-5-mini (openai) · reason: first turn\n\n"
	markerProse := "✦ **Weave Router** → gpt-5-mini (openai) · reason: first turn"
	translator := translate.NewAnthropicSSETranslator(rec, "claude-opus-4-7", nil).
		WithRoutingMarker(marker)

	translator.Header().Set("Content-Type", "text/event-stream")
	translator.WriteHeader(http.StatusOK)

	_, err := translator.Write([]byte("data: [DONE]\n\n"))
	require.NoError(t, err)
	require.NoError(t, translator.Finalize())

	body := rec.Body.String()
	assert.Contains(t, body, "event: message_start")
	assert.Contains(t, body, markerProse)
	assert.Contains(t, body, "event: message_stop")
}

func TestAnthropicSSETranslator_NoMarkerWhenNotConfigured(t *testing.T) {
	rec := httptest.NewRecorder()
	translator := translate.NewAnthropicSSETranslator(rec, "gpt-4o", nil)

	translator.Header().Set("Content-Type", "text/event-stream")
	translator.WriteHeader(http.StatusOK)

	events := []string{
		"data: {\"id\":\"chatcmpl-3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"chatcmpl-3\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
		"data: [DONE]\n\n",
	}
	for _, e := range events {
		_, err := translator.Write([]byte(e))
		require.NoError(t, err)
	}
	require.NoError(t, translator.Finalize())

	body := rec.Body.String()
	assert.NotContains(t, body, "Weave Router")
	assert.Contains(t, body, `"text":"hi"`)
	// Upstream's first block takes index 0 when no marker is configured.
	assert.Contains(t, body, `"index":0,"content_block":{"type":"text"`)
}

func TestAnthropicSSETranslator_ClosingMarkerEmittedAfterContent(t *testing.T) {
	rec := httptest.NewRecorder()
	closingProse := "✦ saved $0.0234 vs claude-opus-4-7"
	closingFn := func(u translate.Usage) string {
		require.Equal(t, 100, u.InputTokens)
		require.Equal(t, 20, u.OutputTokens)
		require.Equal(t, 50, u.CacheReadTokens)
		return closingProse
	}
	translator := translate.NewAnthropicSSETranslator(rec, "gpt-4o", nil).
		WithClosingMarker(closingFn)

	translator.Header().Set("Content-Type", "text/event-stream")
	translator.WriteHeader(http.StatusOK)

	events := []string{
		"data: {\"id\":\"chatcmpl-c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer body\"},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"chatcmpl-c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"prompt_tokens_details\":{\"cached_tokens\":50}}}\n\n",
		"data: [DONE]\n\n",
	}
	for _, e := range events {
		_, err := translator.Write([]byte(e))
		require.NoError(t, err)
	}
	require.NoError(t, translator.Finalize())

	body := rec.Body.String()
	answerIdx := strings.Index(body, `"text":"answer body"`)
	closingIdx := strings.Index(body, closingProse)
	deltaIdx := strings.Index(body, "event: message_delta")
	require.GreaterOrEqual(t, answerIdx, 0)
	require.GreaterOrEqual(t, closingIdx, 0)
	require.GreaterOrEqual(t, deltaIdx, 0)
	assert.Less(t, answerIdx, closingIdx, "closing marker follows upstream content")
	assert.Less(t, closingIdx, deltaIdx, "closing marker precedes message_delta")
}

func TestAnthropicSSETranslator_ClosingMarkerSkippedWhenCallbackReturnsEmpty(t *testing.T) {
	rec := httptest.NewRecorder()
	translator := translate.NewAnthropicSSETranslator(rec, "gpt-4o", nil).
		WithClosingMarker(func(translate.Usage) string { return "" })

	translator.Header().Set("Content-Type", "text/event-stream")
	translator.WriteHeader(http.StatusOK)

	events := []string{
		"data: {\"id\":\"chatcmpl-c2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"chatcmpl-c2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n",
		"data: [DONE]\n\n",
	}
	for _, e := range events {
		_, err := translator.Write([]byte(e))
		require.NoError(t, err)
	}
	require.NoError(t, translator.Finalize())

	body := rec.Body.String()
	assert.Contains(t, body, "event: message_delta")
	assert.Contains(t, body, "event: message_stop")
	// No closing-marker emission means index 1 stays unopened.
	assert.NotContains(t, body, `"index":1`)
}

func TestPrepareAnthropic_ToolResultStringContent(t *testing.T) {
	openAIReq := `{
		"model": "gpt-4o",
		"messages": [
			{"role": "user", "content": "What is the weather?"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"location\":\"SF\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "72°F and sunny"}
		],
		"max_tokens": 1024
	}`

	env, err := translate.ParseOpenAI([]byte(openAIReq))
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-sonnet-4-20250514"})
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &doc))

	msgs, _ := doc["messages"].([]any)
	require.Len(t, msgs, 3, "user + assistant + user(tool_result)")

	toolUserMsg, _ := msgs[2].(map[string]any)
	assert.Equal(t, "user", toolUserMsg["role"])

	blocks, _ := toolUserMsg["content"].([]any)
	require.Len(t, blocks, 1)

	block, _ := blocks[0].(map[string]any)
	assert.Equal(t, "tool_result", block["type"])
	assert.Equal(t, "call_1", block["tool_use_id"])
	assert.Equal(t, "72°F and sunny", block["content"],
		"string tool content must be preserved verbatim through translation")
}

func TestPrepareAnthropic_ToolResultArrayContent(t *testing.T) {
	openAIReq := `{
		"model": "gpt-4o",
		"messages": [
			{"role": "user", "content": "Read the file"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "call_abc", "type": "function", "function": {"name": "read_file", "arguments": "{\"path\":\"/tmp/data.txt\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call_abc", "content": [
				{"type": "text", "text": "line one"},
				{"type": "text", "text": "line two"}
			]}
		],
		"max_tokens": 1024
	}`

	env, err := translate.ParseOpenAI([]byte(openAIReq))
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-sonnet-4-20250514"})
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &doc))

	msgs, _ := doc["messages"].([]any)
	toolUserMsg, _ := msgs[2].(map[string]any)
	blocks, _ := toolUserMsg["content"].([]any)
	require.Len(t, blocks, 1)

	block, _ := blocks[0].(map[string]any)
	assert.Equal(t, "tool_result", block["type"])
	assert.Equal(t, "call_abc", block["tool_use_id"])

	contentBlocks, _ := block["content"].([]any)
	require.Len(t, contentBlocks, 2,
		"array-form tool content must produce Anthropic content blocks, not a joined string")
	first, _ := contentBlocks[0].(map[string]any)
	assert.Equal(t, "text", first["type"])
	assert.Equal(t, "line one", first["text"])
	second, _ := contentBlocks[1].(map[string]any)
	assert.Equal(t, "text", second["type"])
	assert.Equal(t, "line two", second["text"])
}

func TestPrepareAnthropic_ToolResultArrayPreservesImageBlocks(t *testing.T) {
	openAIReq := `{
		"model": "gpt-4o",
		"messages": [
			{"role": "user", "content": "Describe the image"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "call_img", "type": "function", "function": {"name": "analyze", "arguments": "{}"}}
			]},
			{"role": "tool", "tool_call_id": "call_img", "content": [
				{"type": "image_url", "image_url": {"url": "https://example.com/cat.png"}},
				{"type": "text", "text": "a photo of a cat"}
			]}
		],
		"max_tokens": 1024
	}`

	env, err := translate.ParseOpenAI([]byte(openAIReq))
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-sonnet-4-20250514"})
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &doc))

	msgs, _ := doc["messages"].([]any)
	toolUserMsg, _ := msgs[2].(map[string]any)
	toolBlocks, _ := toolUserMsg["content"].([]any)
	resultBlock, _ := toolBlocks[0].(map[string]any)

	contentBlocks, _ := resultBlock["content"].([]any)
	require.Len(t, contentBlocks, 2,
		"both image and text parts must be preserved as Anthropic content blocks")

	imgBlock, _ := contentBlocks[0].(map[string]any)
	assert.Equal(t, "image", imgBlock["type"])
	imgSrc, _ := imgBlock["source"].(map[string]any)
	assert.Equal(t, "url", imgSrc["type"])
	assert.Equal(t, "https://example.com/cat.png", imgSrc["url"])

	txtBlock, _ := contentBlocks[1].(map[string]any)
	assert.Equal(t, "text", txtBlock["type"])
	assert.Equal(t, "a photo of a cat", txtBlock["text"])
}

func TestPrepareAnthropic_ConsecutiveToolResultsMergeIntoOneUserMessage(t *testing.T) {
	openAIReq := `{
		"model": "gpt-4o",
		"messages": [
			{"role": "user", "content": "Check weather in two cities"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "call_sf", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"SF\"}"}},
				{"id": "call_ny", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"NYC\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call_sf", "content": "62°F fog"},
			{"role": "tool", "tool_call_id": "call_ny", "content": [{"type": "text", "text": "85°F humid"}]}
		],
		"max_tokens": 1024
	}`

	env, err := translate.ParseOpenAI([]byte(openAIReq))
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-sonnet-4-20250514"})
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &doc))

	msgs, _ := doc["messages"].([]any)
	require.Len(t, msgs, 3, "user + assistant + single user with both tool_results")

	toolUserMsg, _ := msgs[2].(map[string]any)
	assert.Equal(t, "user", toolUserMsg["role"])

	blocks, _ := toolUserMsg["content"].([]any)
	require.Len(t, blocks, 2, "both tool results must be merged into one user message")

	first, _ := blocks[0].(map[string]any)
	assert.Equal(t, "call_sf", first["tool_use_id"])
	assert.Equal(t, "62°F fog", first["content"])

	second, _ := blocks[1].(map[string]any)
	assert.Equal(t, "call_ny", second["tool_use_id"])
	nyContent, _ := second["content"].([]any)
	require.Len(t, nyContent, 1,
		"array-form content in second tool result must produce Anthropic content blocks")
	nyText, _ := nyContent[0].(map[string]any)
	assert.Equal(t, "text", nyText["type"])
	assert.Equal(t, "85°F humid", nyText["text"])
}

func TestPrepareAnthropic_ToolResultWithNilContentBecomesEmptyString(t *testing.T) {
	openAIReq := `{
		"model": "gpt-4o",
		"messages": [
			{"role": "user", "content": "Run the command"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "call_run", "type": "function", "function": {"name": "exec", "arguments": "{}"}}
			]},
			{"role": "tool", "tool_call_id": "call_run", "content": null}
		],
		"max_tokens": 1024
	}`

	env, err := translate.ParseOpenAI([]byte(openAIReq))
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-sonnet-4-20250514"})
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &doc))

	msgs, _ := doc["messages"].([]any)
	toolUserMsg, _ := msgs[2].(map[string]any)
	blocks, _ := toolUserMsg["content"].([]any)
	block, _ := blocks[0].(map[string]any)
	assert.Equal(t, "", block["content"],
		"null/missing content must produce an empty string, not nil or a panic")
}

func TestPrepareAnthropic_FullToolConversationRoundTrip(t *testing.T) {
	openAIReq := `{
		"model": "gpt-4o",
		"messages": [
			{"role": "system", "content": "You are a helpful assistant."},
			{"role": "user", "content": "Search for recent PRs"},
			{"role": "assistant", "content": "I'll search for that.", "tool_calls": [
				{"id": "call_search", "type": "function", "function": {"name": "search_prs", "arguments": "{\"query\":\"recent\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call_search", "content": [
				{"type": "text", "text": "Found 3 PRs:"},
				{"type": "text", "text": "#101 Fix auth bug\n#102 Add caching\n#103 Update docs"}
			]},
			{"role": "user", "content": "Tell me about PR #101"}
		],
		"max_tokens": 2048
	}`

	env, err := translate.ParseOpenAI([]byte(openAIReq))
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-sonnet-4-20250514"})
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &doc))

	system, _ := doc["system"].([]any)
	require.Len(t, system, 1, "system prompt must be extracted")
	sysBlock, _ := system[0].(map[string]any)
	assert.Equal(t, "You are a helpful assistant.", sysBlock["text"])

	msgs, _ := doc["messages"].([]any)
	require.Len(t, msgs, 4, "user + assistant(tool_use) + user(tool_result) + user")

	roles := make([]string, len(msgs))
	for i, m := range msgs {
		msg, _ := m.(map[string]any)
		roles[i], _ = msg["role"].(string)
	}
	assert.Equal(t, []string{"user", "assistant", "user", "user"}, roles,
		"Anthropic messages must alternate correctly with tool results wrapped in user messages")

	assistantMsg, _ := msgs[1].(map[string]any)
	assistantBlocks, _ := assistantMsg["content"].([]any)
	require.Len(t, assistantBlocks, 2, "assistant must have text + tool_use blocks")
	toolUseBlock, _ := assistantBlocks[1].(map[string]any)
	assert.Equal(t, "tool_use", toolUseBlock["type"])
	assert.Equal(t, "search_prs", toolUseBlock["name"])

	toolResultMsg, _ := msgs[2].(map[string]any)
	toolBlocks, _ := toolResultMsg["content"].([]any)
	require.Len(t, toolBlocks, 1)
	resultBlock, _ := toolBlocks[0].(map[string]any)
	assert.Equal(t, "tool_result", resultBlock["type"])

	contentBlocks, _ := resultBlock["content"].([]any)
	require.Len(t, contentBlocks, 2,
		"multi-part array tool content must survive the full OpenAI->Anthropic translation pipeline as content blocks")
	cb0, _ := contentBlocks[0].(map[string]any)
	assert.Equal(t, "Found 3 PRs:", cb0["text"])
	cb1, _ := contentBlocks[1].(map[string]any)
	assert.Contains(t, cb1["text"], "#101 Fix auth bug")
}

func TestPrepareAnthropic_ToolResultPreservesImageURL(t *testing.T) {
	openAIReq := `{
		"model": "gpt-4o",
		"messages": [
			{"role": "user", "content": "Analyze this"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "call_v", "type": "function", "function": {"name": "screenshot", "arguments": "{}"}}
			]},
			{"role": "tool", "tool_call_id": "call_v", "content": [
				{"type": "text", "text": "Captured screenshot:"},
				{"type": "image_url", "image_url": {"url": "https://example.com/page.png"}},
				{"type": "image_url", "image_url": {"url": "data:image/jpeg;base64,/9j/AAAA"}}
			]}
		],
		"max_tokens": 1024
	}`

	env, err := translate.ParseOpenAI([]byte(openAIReq))
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-sonnet-4-20250514"})
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &doc))

	msgs, _ := doc["messages"].([]any)
	toolUserMsg, _ := msgs[2].(map[string]any)
	toolBlocks, _ := toolUserMsg["content"].([]any)
	resultBlock, _ := toolBlocks[0].(map[string]any)

	contentBlocks, _ := resultBlock["content"].([]any)
	require.Len(t, contentBlocks, 3,
		"text + URL image + base64 image must all be preserved")

	txtBlock, _ := contentBlocks[0].(map[string]any)
	assert.Equal(t, "text", txtBlock["type"])
	assert.Equal(t, "Captured screenshot:", txtBlock["text"])

	urlImg, _ := contentBlocks[1].(map[string]any)
	assert.Equal(t, "image", urlImg["type"])
	urlSrc, _ := urlImg["source"].(map[string]any)
	assert.Equal(t, "url", urlSrc["type"])
	assert.Equal(t, "https://example.com/page.png", urlSrc["url"])

	b64Img, _ := contentBlocks[2].(map[string]any)
	assert.Equal(t, "image", b64Img["type"])
	b64Src, _ := b64Img["source"].(map[string]any)
	assert.Equal(t, "base64", b64Src["type"])
	assert.Equal(t, "image/jpeg", b64Src["media_type"])
	assert.Equal(t, "/9j/AAAA", b64Src["data"])
}

func BenchmarkSSETranslatorStream(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		tr := translate.NewSSETranslator(rec, "claude-sonnet-4-20250514", nil)
		tr.Header().Set("Content-Type", "text/event-stream")
		tr.WriteHeader(http.StatusOK)
		if _, err := tr.Write(benchAnthropicStream); err != nil {
			b.Fatal(err)
		}
		if err := tr.Finalize(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAnthropicSSETranslatorStream(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		tr := translate.NewAnthropicSSETranslator(rec, "gpt-4o", nil)
		tr.Header().Set("Content-Type", "text/event-stream")
		tr.WriteHeader(http.StatusOK)
		if _, err := tr.Write(benchOpenAIStream); err != nil {
			b.Fatal(err)
		}
		if err := tr.Finalize(); err != nil {
			b.Fatal(err)
		}
	}
}
