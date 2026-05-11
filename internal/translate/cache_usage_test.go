package translate_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/translate"
)

// fakeUsageSink records the last RecordUsage / RecordCacheUsage calls.
// Translators forward parsed token counts to whichever sink the proxy
// installed (in prod: *otel.UsageExtractor); these tests verify the
// translator passes the right JSON-extracted values to the sink without
// constructing a real extractor.
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

// SSETranslator forwards Anthropic message_start cache tokens to the sink
// (cache_creation_input_tokens, cache_read_input_tokens). Catches typos in
// either JSON path.
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

// AnthropicSSETranslator forwards OpenAI prompt_tokens_details.cached_tokens
// to the sink as cacheRead (no cache_creation on OpenAI). Catches typos in
// the nested JSON path that gjson would otherwise silently return 0 for.
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
