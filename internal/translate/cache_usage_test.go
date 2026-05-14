package translate_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/translate"
)

// fakeUsageSink records the last RecordUsage / RecordCacheUsage calls.
type fakeUsageSink struct {
	input         int
	output        int
	cacheCreation int
	cacheRead     int
}

func (f *fakeUsageSink) RecordUsage(input, output int) {
	f.input = input
	f.output = output
}

func (f *fakeUsageSink) RecordCacheUsage(creation, read int) {
	f.cacheCreation = creation
	f.cacheRead = read
}

// Catches typos in the message_start cache_creation_input_tokens /
// cache_read_input_tokens JSON paths.
func TestSSETranslator_ForwardsAnthropicCacheTokens(t *testing.T) {
	rec := httptest.NewRecorder()
	sink := &fakeUsageSink{}
	translator := translate.NewSSETranslator(rec, "claude-sonnet-4-5", sink)

	translator.Header().Set("Content-Type", "text/event-stream")
	translator.WriteHeader(http.StatusOK)

	event := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-5\",\"usage\":{\"input_tokens\":150,\"output_tokens\":0,\"cache_creation_input_tokens\":512,\"cache_read_input_tokens\":2048}}}\n\n"
	_, err := translator.Write([]byte(event))
	require.NoError(t, err)

	assert.Equal(t, 150, sink.input)
	assert.Equal(t, 512, sink.cacheCreation)
	assert.Equal(t, 2048, sink.cacheRead)
}

// Catches typos in the prompt_tokens_details.cached_tokens nested path that
// gjson would otherwise silently return 0 for.
func TestAnthropicSSETranslator_ForwardsOpenAICachedTokens(t *testing.T) {
	rec := httptest.NewRecorder()
	sink := &fakeUsageSink{}
	translator := translate.NewAnthropicSSETranslator(rec, "gpt-4o", sink)

	translator.Header().Set("Content-Type", "text/event-stream")
	translator.WriteHeader(http.StatusOK)

	events := []string{
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":80,\"completion_tokens\":12,\"prompt_tokens_details\":{\"cached_tokens\":64}}}\n\n",
		"data: [DONE]\n\n",
	}
	for _, e := range events {
		_, err := translator.Write([]byte(e))
		require.NoError(t, err)
	}

	assert.Equal(t, 80, sink.input)
	assert.Equal(t, 12, sink.output)
	assert.Equal(t, 0, sink.cacheCreation)
	assert.Equal(t, 64, sink.cacheRead)
}

// Cross-format upstreams (OpenAI, Gemini) only learn the real input_tokens
// at the end of the stream, but Claude Code's subagent counter reads off
// message_start.usage.input_tokens. Without WithEstimatedInputTokens the
// counter shows zero tokens for every subagent turn.
func TestAnthropicSSETranslator_MessageStartCarriesEstimatedInputTokens(t *testing.T) {
	rec := httptest.NewRecorder()
	translator := translate.NewAnthropicSSETranslator(rec, "gpt-4o", nil).
		WithEstimatedInputTokens(1234)

	translator.Header().Set("Content-Type", "text/event-stream")
	translator.WriteHeader(http.StatusOK)

	events := []string{
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"},\"finish_reason\":null}]}\n\n",
		"data: [DONE]\n\n",
	}
	for _, e := range events {
		_, err := translator.Write([]byte(e))
		require.NoError(t, err)
	}

	body := rec.Body.String()
	startIdx := strings.Index(body, "event: message_start")
	deltaIdx := strings.Index(body, "event: content_block_delta")
	require.GreaterOrEqual(t, startIdx, 0, "message_start must be emitted")
	require.GreaterOrEqual(t, deltaIdx, startIdx, "message_start must precede the first delta")
	startSegment := body[startIdx:deltaIdx]
	assert.Contains(t, startSegment, `"usage":{"input_tokens":1234,"output_tokens":0}`)
}

// Catches the Gemini cachedContentTokenCount field being dropped on its way
// to the usage sink. Gemini implicit caching is the only signal we have that
// caching is working at all on the Gemini path, so this number must reach
// recordTurnUsage → session_pins / telemetry.
func TestGeminiSSETranslator_ForwardsCachedContentTokenCount(t *testing.T) {
	rec := httptest.NewRecorder()
	sink := &fakeUsageSink{}
	translator := translate.NewGeminiToOpenAISSETranslator(rec, "gemini-3.1-flash-lite-preview", sink)

	translator.Header().Set("Content-Type", "text/event-stream")
	translator.WriteHeader(http.StatusOK)

	chunks := []string{
		`data: {"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}` + "\n\n",
		`data: {"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2048,"candidatesTokenCount":4,"totalTokenCount":2052,"cachedContentTokenCount":1900}}` + "\n\n",
	}
	for _, c := range chunks {
		_, err := translator.Write([]byte(c))
		require.NoError(t, err)
	}
	require.NoError(t, translator.Finalize())

	assert.Equal(t, 2048, sink.input)
	assert.Equal(t, 4, sink.output)
	assert.Equal(t, 0, sink.cacheCreation, "Gemini reports only cache reads, not creation")
	assert.Equal(t, 1900, sink.cacheRead)

	// The serialized OpenAI chunk must carry prompt_tokens_details.cached_tokens
	// so the downstream AnthropicSSETranslator picks it up via gjson at the
	// path it already reads (stream.go:604).
	body := rec.Body.String()
	assert.Contains(t, body, `"prompt_tokens_details":{"cached_tokens":1900}`)
}

// Same field, non-streaming path (Gemini :generateContent returned as a single
// JSON body rather than SSE). Same propagation requirement.
func TestGeminiSSETranslator_NonStreamingForwardsCachedContentTokenCount(t *testing.T) {
	rec := httptest.NewRecorder()
	sink := &fakeUsageSink{}
	translator := translate.NewGeminiToOpenAISSETranslator(rec, "gemini-3.1-flash-lite-preview", sink)

	translator.Header().Set("Content-Type", "application/json")
	translator.WriteHeader(http.StatusOK)

	body := `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1500,"candidatesTokenCount":3,"totalTokenCount":1503,"cachedContentTokenCount":1200}}`
	_, err := translator.Write([]byte(body))
	require.NoError(t, err)
	require.NoError(t, translator.Finalize())

	assert.Equal(t, 1500, sink.input)
	assert.Equal(t, 3, sink.output)
	assert.Equal(t, 1200, sink.cacheRead)
}
