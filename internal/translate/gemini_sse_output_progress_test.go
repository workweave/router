package translate_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the OUTPUT-progress classification the Google clients'
// output-stall watchdog depends on (see httputil.DefaultOutputStallTimeout).
// The Gemini→OpenAI SSE translator is the outermost writer the Google clients
// receive (standalone for Gemini→openaicompat, and wrapping an inner
// AnthropicSSETranslator for Anthropic→Google), so it owns the mark: a text
// delta, a tool-call chunk, and the terminal finish must count as progress;
// the role-only first chunk, usage-only chunks, and [DONE] must NOT (else a
// byte-alive/output-silent Gemini stream would keep resetting the watchdog).

func newGeminiStreamingWriter(t *testing.T) (*translate.GeminiToOpenAISSETranslator, *int) {
	t.Helper()
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/event-stream")
	w := translate.NewGeminiToOpenAISSETranslator(rec, "gemini-2.5-pro", nil)
	w.WriteHeader(http.StatusOK)
	count := 0
	require.True(t, w.ArmOutputProgress(func() { count++ }),
		"ArmOutputProgress must report armed for a streaming client")
	return w, &count
}

func geminiSSE(payload string) []byte {
	return []byte("data: " + payload + "\n\n")
}

func TestGeminiSSEOutputProgress_TextCounts(t *testing.T) {
	w, count := newGeminiStreamingWriter(t)
	_, err := w.Write(geminiSSE(`{"candidates":[{"content":{"parts":[{"text":"hello"}]}}]}`))
	require.NoError(t, err)
	// The role-only first chunk does not mark; only the text delta after it.
	assert.Equal(t, 1, *count, "a text delta must mark output progress exactly once")
}

func TestGeminiSSEOutputProgress_ToolCallCounts(t *testing.T) {
	w, count := newGeminiStreamingWriter(t)
	_, err := w.Write(geminiSSE(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"Bash","args":{"command":"ls"}}}]}}]}`))
	require.NoError(t, err)
	assert.Equal(t, 1, *count, "a functionCall delta must mark output progress")
}

func TestGeminiSSEOutputProgress_FinishCounts(t *testing.T) {
	w, count := newGeminiStreamingWriter(t)
	_, err := w.Write(geminiSSE(`{"candidates":[{"finishReason":"STOP"}]}`))
	require.NoError(t, err)
	assert.Positive(t, *count, "a terminal finishReason must mark output progress")
}

func TestGeminiSSEOutputProgress_UsageOnlyDoesNotCount(t *testing.T) {
	w, count := newGeminiStreamingWriter(t)
	// A trailing usage-only chunk: no parts, no finishReason.
	_, err := w.Write(geminiSSE(`{"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":0,"totalTokenCount":10}}`))
	require.NoError(t, err)
	assert.Zero(t, *count, "usage-only frames must not mark output progress")
}

func TestGeminiSSEOutputProgress_NotArmedWhenNotStreaming(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/json")
	w := translate.NewGeminiToOpenAISSETranslator(rec, "gemini-2.5-pro", nil)
	w.WriteHeader(http.StatusOK)
	assert.False(t, w.ArmOutputProgress(func() {}),
		"ArmOutputProgress must report not-armed for a non-streaming client")
}
