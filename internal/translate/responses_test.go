package translate_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestResponsesToChatCompletions_InstructionsAndInput(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5",
		"instructions": "be helpful",
		"stream": true,
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hi"}]}
		]
	}`)

	out, isStream, model, err := translate.ResponsesToChatCompletions(body)
	require.NoError(t, err)
	assert.True(t, isStream)
	assert.Equal(t, "gpt-5", model)

	root := gjson.ParseBytes(out)
	assert.Equal(t, "gpt-5", root.Get("model").Str)
	assert.True(t, root.Get("stream").Bool())
	assert.True(t, root.Get("stream_options.include_usage").Bool())

	messages := root.Get("messages").Array()
	require.Len(t, messages, 2)
	assert.Equal(t, "system", messages[0].Get("role").Str)
	assert.Equal(t, "be helpful", messages[0].Get("content").Str)
	assert.Equal(t, "user", messages[1].Get("role").Str)
	assert.Equal(t, "hi", messages[1].Get("content").Str)
}

func TestResponsesToChatCompletions_FunctionCallRoundTrip(t *testing.T) {
	// Codex re-sends prior tool calls + their outputs in the input array.
	body := []byte(`{
		"model": "gpt-5",
		"input": [
			{"type": "message", "role": "user", "content": "do the thing"},
			{"type": "function_call", "call_id": "call_123", "name": "do_thing", "arguments": "{\"x\":1}"},
			{"type": "function_call_output", "call_id": "call_123", "output": "done"}
		]
	}`)

	out, _, _, err := translate.ResponsesToChatCompletions(body)
	require.NoError(t, err)

	messages := gjson.GetBytes(out, "messages").Array()
	require.Len(t, messages, 3)

	// Assistant function_call → assistant message with tool_calls.
	assert.Equal(t, "assistant", messages[1].Get("role").Str)
	tc := messages[1].Get("tool_calls.0")
	assert.Equal(t, "call_123", tc.Get("id").Str)
	assert.Equal(t, "do_thing", tc.Get("function.name").Str)
	assert.Equal(t, `{"x":1}`, tc.Get("function.arguments").Str)

	// function_call_output → tool role message keyed by tool_call_id.
	assert.Equal(t, "tool", messages[2].Get("role").Str)
	assert.Equal(t, "call_123", messages[2].Get("tool_call_id").Str)
	assert.Equal(t, "done", messages[2].Get("content").Str)
}

func TestResponsesToChatCompletions_ToolsFlatToNested(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5",
		"input": "hi",
		"tools": [
			{"type": "function", "name": "search", "description": "search docs", "parameters": {"type": "object"}}
		]
	}`)

	out, _, _, err := translate.ResponsesToChatCompletions(body)
	require.NoError(t, err)

	tools := gjson.GetBytes(out, "tools").Array()
	require.Len(t, tools, 1)
	assert.Equal(t, "function", tools[0].Get("type").Str)
	assert.Equal(t, "search", tools[0].Get("function.name").Str)
	assert.Equal(t, "search docs", tools[0].Get("function.description").Str)
	assert.True(t, tools[0].Get("function.parameters").IsObject())
}

func TestResponsesToChatCompletions_StripsRoutingBadgeFromAssistantHistory(t *testing.T) {
	// Codex re-sends every prior assistant turn in the input array. The badge
	// we prepend on egress must not survive ingress, or the upstream sees
	// per-turn router-injected bytes that break prompt-cache reuse.
	body := []byte(`{
		"model": "gpt-5",
		"input": [
			{"type": "message", "role": "user", "content": "hi"},
			{"type": "message", "role": "assistant", "content": [
				{"type": "output_text", "text": "**WEAVE ROUTER** — claude-opus-4-7 ← gpt-5.5\n\nHello there!"}
			]},
			{"type": "message", "role": "user", "content": "again"}
		]
	}`)

	out, _, _, err := translate.ResponsesToChatCompletions(body)
	require.NoError(t, err)

	messages := gjson.GetBytes(out, "messages").Array()
	require.Len(t, messages, 3)
	assert.Equal(t, "assistant", messages[1].Get("role").Str)
	assert.Equal(t, "Hello there!", messages[1].Get("content").Str)
}

func TestResponsesToChatCompletions_StripsBadgeFromStringContent(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5",
		"input": [
			{"type": "message", "role": "assistant", "content": "**WEAVE ROUTER** — claude-opus-4-7\n\nbody"}
		]
	}`)

	out, _, _, err := translate.ResponsesToChatCompletions(body)
	require.NoError(t, err)

	messages := gjson.GetBytes(out, "messages").Array()
	require.Len(t, messages, 1)
	assert.Equal(t, "body", messages[0].Get("content").Str)
}

func TestResponsesToChatCompletions_LeavesUserContentAlone(t *testing.T) {
	// User content that happens to start with the marker bytes (e.g. someone
	// pasting our log line in) must not be silently mutated.
	body := []byte(`{
		"model": "gpt-5",
		"input": [
			{"type": "message", "role": "user", "content": "**WEAVE ROUTER** — something\n\nplease explain"}
		]
	}`)

	out, _, _, err := translate.ResponsesToChatCompletions(body)
	require.NoError(t, err)

	messages := gjson.GetBytes(out, "messages").Array()
	require.Len(t, messages, 1)
	assert.Contains(t, messages[0].Get("content").Str, "**WEAVE ROUTER**")
}

func TestResponsesToChatCompletions_ReasoningAndMaxOutput(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5",
		"input": "hi",
		"max_output_tokens": 4096,
		"reasoning": {"effort": "high"}
	}`)

	out, _, _, err := translate.ResponsesToChatCompletions(body)
	require.NoError(t, err)

	assert.Equal(t, int64(4096), gjson.GetBytes(out, "max_completion_tokens").Int())
	assert.Equal(t, "high", gjson.GetBytes(out, "reasoning_effort").Str)
}

func TestResponsesWriter_StreamingText(t *testing.T) {
	rec := httptest.NewRecorder()
	// Empty initial model + no x-router-model header means the badge can't
	// resolve a routed pick and stays silent — lets this test focus on the
	// chunk translation contract.
	w := translate.NewResponsesWriter(rec, "")

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)

	// Simulate chat-completions chunks from the existing path.
	chunks := []string{
		`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}` + "\n\n",
		"data: [DONE]\n\n",
	}
	for _, c := range chunks {
		_, err := w.Write([]byte(c))
		require.NoError(t, err)
	}
	require.NoError(t, w.Finalize())

	events := parseSSEEvents(t, rec.Body.Bytes())
	types := eventTypes(events)
	assert.Contains(t, types, "response.created")
	assert.Contains(t, types, "response.output_item.added")
	assert.Contains(t, types, "response.content_part.added")
	assert.Contains(t, types, "response.output_text.delta")
	assert.Contains(t, types, "response.output_text.done")
	assert.Contains(t, types, "response.content_part.done")
	assert.Contains(t, types, "response.output_item.done")
	assert.Contains(t, types, "response.completed")

	// Concatenated text deltas, skipping the routing badge prefix that the
	// writer prepends on the first delta whenever a routed model is known.
	// Chunks here carry model="gpt-5" so the badge resolves and fires.
	var combined strings.Builder
	for _, e := range events {
		if e["type"] != "response.output_text.delta" {
			continue
		}
		d := e["delta"].(string)
		if strings.HasPrefix(d, "**WEAVE ROUTER**") {
			continue
		}
		combined.WriteString(d)
	}
	assert.Equal(t, "Hello world", combined.String())

	// Final completed event carries usage.
	final := events[len(events)-1]
	assert.Equal(t, "response.completed", final["type"])
	usage := final["response"].(map[string]any)["usage"].(map[string]any)
	assert.EqualValues(t, 3, usage["input_tokens"])
	assert.EqualValues(t, 2, usage["output_tokens"])
}

func TestResponsesWriter_PrependsBadgeOnSwap(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewResponsesWriter(rec, "gpt-5.5")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("x-router-model", "claude-opus-4-7")
	w.WriteHeader(200)

	for _, c := range []string{
		`data: {"choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n",
		"data: [DONE]\n\n",
	} {
		_, err := w.Write([]byte(c))
		require.NoError(t, err)
	}
	require.NoError(t, w.Finalize())

	events := parseSSEEvents(t, rec.Body.Bytes())

	var deltas []string
	for _, e := range events {
		if e["type"] == "response.output_text.delta" {
			deltas = append(deltas, e["delta"].(string))
		}
	}
	require.Len(t, deltas, 3)
	// Format: **WEAVE ROUTER** — claude-opus-4-7 ← gpt-5.5\n\n
	assert.Contains(t, deltas[0], "**WEAVE ROUTER**")
	assert.Contains(t, deltas[0], "claude-opus-4-7")
	assert.Contains(t, deltas[0], "← gpt-5.5")
	assert.True(t, strings.HasSuffix(deltas[0], "\n\n"))
	assert.Equal(t, "Hello", deltas[1])
	assert.Equal(t, " world", deltas[2])

	// response.completed appears exactly once.
	completedCount := 0
	for _, e := range events {
		if e["type"] == "response.completed" {
			completedCount++
		}
	}
	assert.Equal(t, 1, completedCount)
}

func TestResponsesWriter_BadgeWithoutSwapShowsModelOnly(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewResponsesWriter(rec, "claude-opus-4-7")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("x-router-model", "claude-opus-4-7")
	w.WriteHeader(200)

	_, err := w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n"))
	require.NoError(t, err)
	_, err = w.Write([]byte("data: [DONE]\n\n"))
	require.NoError(t, err)
	require.NoError(t, w.Finalize())

	events := parseSSEEvents(t, rec.Body.Bytes())
	var firstDelta string
	for _, e := range events {
		if e["type"] == "response.output_text.delta" {
			firstDelta = e["delta"].(string)
			break
		}
	}
	assert.Contains(t, firstDelta, "**WEAVE ROUTER**")
	assert.Contains(t, firstDelta, "claude-opus-4-7")
	assert.NotContains(t, firstDelta, "←")
}

func TestResponsesWriter_UsesRoutedModelFromHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewResponsesWriter(rec, "gpt-5")

	// Simulate the proxy stamping its routing decision on the writer headers
	// before any body bytes flow through.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("x-router-model", "claude-opus-4-7")
	w.Header().Set("x-router-provider", "anthropic")
	w.WriteHeader(200)

	_, err := w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n"))
	require.NoError(t, err)
	_, err = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n"))
	require.NoError(t, err)
	_, err = w.Write([]byte("data: [DONE]\n\n"))
	require.NoError(t, err)
	require.NoError(t, w.Finalize())

	events := parseSSEEvents(t, rec.Body.Bytes())

	// response.created and response.completed both carry the routed model.
	var created, completed map[string]any
	for _, e := range events {
		switch e["type"] {
		case "response.created":
			created = e["response"].(map[string]any)
		case "response.completed":
			completed = e["response"].(map[string]any)
		}
	}
	require.NotNil(t, created)
	require.NotNil(t, completed)
	assert.Equal(t, "claude-opus-4-7", created["model"])
	assert.Equal(t, "claude-opus-4-7", completed["model"])
}

func TestResponsesWriter_StreamingToolCall(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewResponsesWriter(rec, "gpt-5")
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)

	chunks := []string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_a","function":{"name":"do","arguments":""}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"x\":"}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n",
		"data: [DONE]\n\n",
	}
	for _, c := range chunks {
		_, err := w.Write([]byte(c))
		require.NoError(t, err)
	}
	require.NoError(t, w.Finalize())

	events := parseSSEEvents(t, rec.Body.Bytes())
	types := eventTypes(events)
	assert.Contains(t, types, "response.output_item.added")
	assert.Contains(t, types, "response.function_call_arguments.delta")
	assert.Contains(t, types, "response.function_call_arguments.done")
	assert.Contains(t, types, "response.completed")

	// Args reassembled.
	var args strings.Builder
	for _, e := range events {
		if e["type"] == "response.function_call_arguments.delta" {
			args.WriteString(e["delta"].(string))
		}
	}
	assert.Equal(t, `{"x":1}`, args.String())

	// Final item carries call_id and full arguments.
	for _, e := range events {
		if e["type"] == "response.function_call_arguments.done" {
			assert.Equal(t, `{"x":1}`, e["arguments"])
		}
	}
}

func parseSSEEvents(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var events []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(payload), &m))
		events = append(events, m)
	}
	return events
}

func eventTypes(events []map[string]any) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		if s, ok := e["type"].(string); ok {
			out = append(out, s)
		}
	}
	return out
}
