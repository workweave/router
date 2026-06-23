package translate_test

import (
	"net/http/httptest"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the OUTPUT-progress classification the openaicompat
// output-stall watchdog depends on (see httputil.DefaultOutputStallTimeout).
// The OpenAI→Anthropic SSE translator is the only layer that can tell an
// output-bearing Chat Completions delta from a keepalive / empty / role-only
// frame, so it owns the mark: text content, streamed reasoning, tool-call
// arguments, and the terminal finish must count as progress; keepalives and
// empty deltas must NOT (else the 2026-06-19 byte-alive/output-silent DeepInfra
// stall would keep resetting the watchdog forever).

func newChatStreamingWriter(t *testing.T) (*translate.AnthropicSSETranslator, *int) {
	t.Helper()
	w := translate.NewAnthropicSSETranslator(httptest.NewRecorder(), "deepseek/deepseek-v4-flash", nil)
	require.NoError(t, w.Prelude(true))
	count := 0
	require.True(t, w.ArmOutputProgress(func() { count++ }),
		"ArmOutputProgress must report armed for a streaming client")
	return w, &count
}

func TestAnthropicSSEOutputProgress_TextCounts(t *testing.T) {
	w, count := newChatStreamingWriter(t)
	_, err := w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n"))
	require.NoError(t, err)
	assert.Positive(t, *count, "a content delta must mark output progress")
}

func TestAnthropicSSEOutputProgress_ReasoningCounts(t *testing.T) {
	// Unlike the Responses path, streamed reasoning_content is real output for
	// OSS models (rendered as a thinking block), so it must count as progress.
	w, count := newChatStreamingWriter(t)
	_, err := w.Write([]byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"let me think\"},\"finish_reason\":null}]}\n\n"))
	require.NoError(t, err)
	assert.Positive(t, *count, "a reasoning_content delta must mark output progress")
}

func TestAnthropicSSEOutputProgress_ToolCallArgsCount(t *testing.T) {
	w, count := newChatStreamingWriter(t)
	_, err := w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Bash\",\"arguments\":\"{\\\"cmd\\\":1}\"}}]},\"finish_reason\":null}]}\n\n"))
	require.NoError(t, err)
	assert.Positive(t, *count, "a tool_calls delta must mark output progress")
}

func TestAnthropicSSEOutputProgress_FinishCounts(t *testing.T) {
	w, count := newChatStreamingWriter(t)
	_, err := w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
	require.NoError(t, err)
	assert.Positive(t, *count, "a terminal finish_reason must mark output progress")
}

func TestAnthropicSSEOutputProgress_KeepalivesDoNotCount(t *testing.T) {
	w, count := newChatStreamingWriter(t)
	// Role-only opening delta (no content), then a null-tool_calls plain delta
	// (the GLM-5.1 keepalive shape), then a usage-only chunk. None is output.
	frames := []string{
		"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":null,\"tool_calls\":null},\"finish_reason\":null}]}\n\n",
		"data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":0}}\n\n",
	}
	for _, f := range frames {
		_, err := w.Write([]byte(f))
		require.NoError(t, err)
	}
	assert.Zero(t, *count, "role-only / null / usage-only frames must not mark output progress")
}

func TestAnthropicSSEOutputProgress_NotArmedWhenNotStreaming(t *testing.T) {
	// The buffered (non-streaming) path translates only at Finalize, so it has
	// nothing to mark mid-stream; arming there would guarantee a false trip.
	w := translate.NewAnthropicSSETranslator(httptest.NewRecorder(), "deepseek/deepseek-v4-flash", nil)
	require.NoError(t, w.Prelude(false))
	assert.False(t, w.ArmOutputProgress(func() {}),
		"ArmOutputProgress must report not-armed for a non-streaming client")
}
