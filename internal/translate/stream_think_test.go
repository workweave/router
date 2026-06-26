package translate_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workweave/router/internal/translate"

	"github.com/tidwall/gjson"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// feedChat writes each frame as an SSE data event to the translator.
func feedChat(t *testing.T, w *translate.AnthropicSSETranslator, frames ...string) {
	t.Helper()
	for _, f := range frames {
		_, err := w.Write([]byte("data: " + f + "\n\n"))
		require.NoError(t, err)
	}
	_, err := w.Write([]byte("data: [DONE]\n\n"))
	require.NoError(t, err)
}

// collectDeltas concatenates all thinking_delta and text_delta values from
// an Anthropic SSE stream (deltas can split arbitrarily across frames).
func collectDeltas(out string) (thinking, text string) {
	for _, line := range strings.Split(out, "\n") {
		const p = "data: "
		if !strings.HasPrefix(line, p) {
			continue
		}
		data := line[len(p):]
		d := gjson.Get(data, "delta")
		switch d.Get("type").String() {
		case "thinking_delta":
			thinking += d.Get("thinking").String()
		case "text_delta":
			text += d.Get("text").String()
		}
	}
	return thinking, text
}

func TestThinkTagStreaming_RoutesLeadingThinkToThinking(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewAnthropicSSETranslator(rec, "xiaomi/mimo-v2.5-pro", nil).
		WithThinkTagReasoning(true)
	require.NoError(t, w.Prelude(true))

	// <think> split across deltas, then the real answer.
	feedChat(t, w,
		`{"choices":[{"delta":{"content":"<think>rea"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"content":"soning here</","finish_reason":null}]}`,
		`{"choices":[{"delta":{"content":"think>the answer"},"finish_reason":"stop"}]}`,
	)
	require.NoError(t, w.Finalize())

	out := rec.Body.String()
	// A thinking block opens and a text block opens, in that order.
	thinkStart := strings.Index(out, `"content_block":{"type":"thinking"`)
	textStart := strings.Index(out, `"content_block":{"type":"text"`)
	require.GreaterOrEqual(t, thinkStart, 0, "expected a thinking content block")
	require.GreaterOrEqual(t, textStart, 0, "expected a text content block")
	assert.Less(t, thinkStart, textStart, "thinking block must precede text block")

	// The reasoning surfaces as thinking, the answer as text.
	thinking, text := collectDeltas(out)
	assert.Equal(t, "reasoning here", thinking)
	assert.Equal(t, "the answer", text)
	// The raw tags must not leak into the wire output.
	assert.NotContains(t, out, "<think>")
	assert.NotContains(t, out, "</think>")
}

func TestThinkTagStreaming_MidProseStaysText(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewAnthropicSSETranslator(rec, "xiaomi/mimo-v2.5-pro", nil).
		WithThinkTagReasoning(true)
	require.NoError(t, w.Prelude(true))

	feedChat(t, w,
		`{"choices":[{"delta":{"content":"here is <think>not reasoning</think> mention"},"finish_reason":"stop"}]}`,
	)
	require.NoError(t, w.Finalize())

	out := rec.Body.String()
	// No thinking block — the tag does not open the content.
	assert.NotContains(t, out, `"content_block":{"type":"thinking"`)
	assert.Contains(t, out, `"content_block":{"type":"text"`)
}

func TestThinkTagStreaming_DisabledPassesThrough(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewAnthropicSSETranslator(rec, "xiaomi/mimo-v2.5-pro", nil)
	require.NoError(t, w.Prelude(true))

	feedChat(t, w,
		`{"choices":[{"delta":{"content":"<think>reasoning</think>answer"},"finish_reason":"stop"}]}`,
	)
	require.NoError(t, w.Finalize())

	out := rec.Body.String()
	// Without the flag, the <think> tag stays in the text channel.
	assert.NotContains(t, out, `"content_block":{"type":"thinking"`)
	assert.Contains(t, out, "<think>")
}

func TestThinkTagStreaming_ThinkingThenToolCall(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewAnthropicSSETranslator(rec, "xiaomi/mimo-v2.5-pro", nil).
		WithThinkTagReasoning(true)
	require.NoError(t, w.Prelude(true))

	feedChat(t, w,
		`{"choices":[{"delta":{"content":"<think>plan</think>"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"ls\"}"}}]},"finish_reason":"tool_calls"}]}`,
	)
	require.NoError(t, w.Finalize())

	out := rec.Body.String()
	thinkStart := strings.Index(out, `"content_block":{"type":"thinking"`)
	toolStart := strings.Index(out, `"content_block":{"type":"tool_use"`)
	require.GreaterOrEqual(t, thinkStart, 0, "expected a thinking content block")
	require.GreaterOrEqual(t, toolStart, 0, "expected a tool_use content block")
	assert.Less(t, thinkStart, toolStart, "thinking must precede tool_use")
	assert.Contains(t, out, "plan")
}

func TestThinkTagNonStreaming_SplitsThinkingAndText(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewAnthropicSSETranslator(rec, "xiaomi/mimo-v2.5-pro", nil).
		WithThinkTagReasoning(true)
	require.NoError(t, w.Prelude(false)) // buffered path
	w.WriteHeader(http.StatusOK)

	body := `{"id":"cmpl-1","model":"xiaomi/mimo-v2.5-pro","choices":[{"message":{"role":"assistant","content":"<think>my reasoning</think>final answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":7}}`
	_, err := w.Write([]byte(body))
	require.NoError(t, err)
	require.NoError(t, w.Finalize())

	var doc map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
	blocks, _ := doc["content"].([]any)
	require.Len(t, blocks, 2)

	think, _ := blocks[0].(map[string]any)
	assert.Equal(t, "thinking", think["type"])
	assert.Equal(t, "my reasoning", think["thinking"])

	text, _ := blocks[1].(map[string]any)
	assert.Equal(t, "text", text["type"])
	assert.Equal(t, "final answer", text["text"])
}

func TestThinkTagNonStreaming_ReasoningAndTagFoldIntoOneBlock(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewAnthropicSSETranslator(rec, "xiaomi/mimo-v2.5-pro", nil).
		WithThinkTagReasoning(true)
	require.NoError(t, w.Prelude(false)) // buffered path
	w.WriteHeader(http.StatusOK)

	// Upstream fills both reasoning_content and a leading <think> in content.
	// The buffered path must fold both into a single thinking block, matching
	// the streaming translator (appendThinking reuses an open thinking block).
	body := `{"id":"cmpl-3","model":"xiaomi/mimo-v2.5-pro","choices":[{"message":{"role":"assistant","reasoning_content":"native reasoning ","content":"<think>tag reasoning</think>final answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":7}}`
	_, err := w.Write([]byte(body))
	require.NoError(t, err)
	require.NoError(t, w.Finalize())

	var doc map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
	blocks, _ := doc["content"].([]any)
	require.Len(t, blocks, 2)

	think, _ := blocks[0].(map[string]any)
	assert.Equal(t, "thinking", think["type"])
	assert.Equal(t, "native reasoning tag reasoning", think["thinking"])

	text, _ := blocks[1].(map[string]any)
	assert.Equal(t, "text", text["type"])
	assert.Equal(t, "final answer", text["text"])
}

func TestThinkTagNonStreaming_NoLeadingThinkUnchanged(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewAnthropicSSETranslator(rec, "xiaomi/mimo-v2.5-pro", nil).
		WithThinkTagReasoning(true)
	require.NoError(t, w.Prelude(false))
	w.WriteHeader(http.StatusOK)

	body := `{"id":"cmpl-2","model":"xiaomi/mimo-v2.5-pro","choices":[{"message":{"role":"assistant","content":"plain answer with <think> mid"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":7}}`
	_, err := w.Write([]byte(body))
	require.NoError(t, err)
	require.NoError(t, w.Finalize())

	var doc map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
	blocks, _ := doc["content"].([]any)
	require.Len(t, blocks, 1)
	text, _ := blocks[0].(map[string]any)
	assert.Equal(t, "text", text["type"])
	assert.Equal(t, "plain answer with <think> mid", text["text"])
}

func TestThinkTagStreaming_UnclosedFlushedAsThinking(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewAnthropicSSETranslator(rec, "xiaomi/mimo-v2.5-pro", nil).
		WithThinkTagReasoning(true)
	require.NoError(t, w.Prelude(true))

	// finish_reason=length, never closed the tag.
	feedChat(t, w,
		`{"choices":[{"delta":{"content":"<think>truncated reasoning"},"finish_reason":"length"}]}`,
	)
	require.NoError(t, w.Finalize())

	out := rec.Body.String()
	assert.Contains(t, out, `"content_block":{"type":"thinking"`)
	thinking, _ := collectDeltas(out)
	assert.Equal(t, "truncated reasoning", thinking)
	assert.NotContains(t, out, "<think>")
}
