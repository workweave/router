package translate_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the OUTPUT-progress classification the OpenAI→openaicompat
// passthrough output-stall watchdog depends on (see
// httputil.DefaultOutputStallTimeout). On that path the body is OpenAI→OpenAI,
// so no translator parses the stream — the OpenAIRoutingMarkerWriter is the only
// layer that can tell an output-bearing Chat Completions chunk from a keepalive.
// Content, reasoning, tool_calls, and a terminal finish_reason must count as
// progress; the role-only opening delta, null-valued deltas (the GLM-5.1
// keepalive shape), usage-only chunks, and [DONE] must NOT.

func newMarkerStreamingWriter(t *testing.T) (*translate.OpenAIRoutingMarkerWriter, *int) {
	t.Helper()
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/event-stream")
	w := translate.NewOpenAIRoutingMarkerWriter(rec, "deepseek/deepseek-v4-flash", "")
	w.WriteHeader(http.StatusOK)
	count := 0
	require.True(t, w.ArmOutputProgress(func() { count++ }),
		"ArmOutputProgress must report armed for a streaming writer")
	return w, &count
}

func TestMarkerOutputProgress_ContentCounts(t *testing.T) {
	w, count := newMarkerStreamingWriter(t)
	_, err := w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n"))
	require.NoError(t, err)
	assert.Positive(t, *count, "a content delta must mark output progress")
}

func TestMarkerOutputProgress_ReasoningCounts(t *testing.T) {
	w, count := newMarkerStreamingWriter(t)
	_, err := w.Write([]byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"},\"finish_reason\":null}]}\n\n"))
	require.NoError(t, err)
	assert.Positive(t, *count, "a reasoning_content delta must mark output progress")
}

func TestMarkerOutputProgress_ToolCallsCount(t *testing.T) {
	w, count := newMarkerStreamingWriter(t)
	_, err := w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Bash\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\n\n"))
	require.NoError(t, err)
	assert.Positive(t, *count, "a tool_calls delta must mark output progress")
}

func TestMarkerOutputProgress_FinishCounts(t *testing.T) {
	w, count := newMarkerStreamingWriter(t)
	_, err := w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
	require.NoError(t, err)
	assert.Positive(t, *count, "a terminal finish_reason must mark output progress")
}

func TestMarkerOutputProgress_KeepalivesDoNotCount(t *testing.T) {
	w, count := newMarkerStreamingWriter(t)
	frames := []string{
		": OPENROUTER PROCESSING\n\n",
		"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":null,\"tool_calls\":null},\"finish_reason\":null}]}\n\n",
		"data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":0}}\n\n",
		"data: [DONE]\n\n",
	}
	for _, f := range frames {
		_, err := w.Write([]byte(f))
		require.NoError(t, err)
	}
	assert.Zero(t, *count, "keepalive comment / role-only / null / usage-only / [DONE] frames must not mark output progress")
}

func TestMarkerOutputProgress_EventSpanningWrites(t *testing.T) {
	// A single SSE event split across two Write calls must still be classified
	// once, on the Write that completes the event boundary.
	w, count := newMarkerStreamingWriter(t)
	_, err := w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hel"))
	require.NoError(t, err)
	assert.Zero(t, *count, "an incomplete event must not mark before its boundary arrives")
	_, err = w.Write([]byte("lo\"},\"finish_reason\":null}]}\n\n"))
	require.NoError(t, err)
	assert.Equal(t, 1, *count, "the completed event must mark exactly once")
}

func TestMarkerOutputProgress_NotArmedWhenNotStreaming(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/json")
	w := translate.NewOpenAIRoutingMarkerWriter(rec, "deepseek/deepseek-v4-flash", "")
	w.WriteHeader(http.StatusOK)
	assert.False(t, w.ArmOutputProgress(func() {}),
		"ArmOutputProgress must report not-armed for a non-streaming writer")
}
