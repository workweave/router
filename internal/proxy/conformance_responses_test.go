package proxy_test

// OpenAI Responses API (/v1/responses) conformance cases. Reasoning gpt-5.x with
// tools routes here instead of /v1/chat/completions. These guard router #331
// (the request MUST stream upstream, or the header-timeout hang returns) and the
// Responses-SSE -> Anthropic translation, plus the #328 medium-effort promotion.

import (
	"net/http"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/providers/openai"

	"github.com/stretchr/testify/assert"
	"github.com/tidwall/gjson"
)

func openAIClient(baseURL string) providers.Client {
	return openai.NewClient("test-key", baseURL)
}

func TestConformance_OpenAIResponses(t *testing.T) {
	// gpt-5.x + tools + a thinking budget is what trips the Responses dispatch.
	const reasoningToolTurn = `"max_tokens":2048,"thinking":{"type":"enabled","budget_tokens":24576},"tools":` + weatherTool + `,"messages":[{"role":"user","content":"Weather in NYC?"}]`

	cases := []conformanceCase{
		{
			// Streaming client: full Responses-SSE -> Anthropic translation
			// (reasoning->thinking, output_text->text, function_call->tool_use)
			// plus the load-bearing stream:true guard.
			name:            "responses/toolcall_stream",
			provider:        providers.ProviderOpenAI,
			model:           "gpt-5.5",
			newClient:       openAIClient,
			inbound:         `{"model":"gpt-5.5","stream":true,` + reasoningToolTurn + `}`,
			stream:          true,
			upstreamFixture: "responses/toolcall.upstream.sse",
			wantUpstream: func(t *testing.T, path string, body []byte, _ http.Header) {
				assert.Equal(t, "/v1/responses", path, "gpt-5.x reasoning+tools must use the Responses API, not chat/completions")
				assert.True(t, gjson.GetBytes(body, "stream").Bool(),
					"Responses request MUST set stream:true — a stream:false regression reintroduces the #331 header-timeout hang")
				assert.Equal(t, "high", gjson.GetBytes(body, "reasoning.effort").String())
			},
		},
		{
			// Non-streaming client still streams UPSTREAM (#331) and gets a
			// reconstructed one-shot Anthropic body.
			name:            "responses/toolcall_nonstream_client",
			provider:        providers.ProviderOpenAI,
			model:           "gpt-5.5",
			newClient:       openAIClient,
			inbound:         `{"model":"gpt-5.5","stream":false,` + reasoningToolTurn + `}`,
			stream:          false,
			upstreamFixture: "responses/toolcall.upstream.sse",
			wantUpstream: func(t *testing.T, _ string, body []byte, _ http.Header) {
				assert.True(t, gjson.GetBytes(body, "stream").Bool(),
					"Responses upstream MUST stream even for a non-streaming client (#331)")
			},
		},
		{
			// Guards router #328: gpt-5.x has a measured "medium" dead-zone, so a
			// budget that resolves to medium must be promoted to high.
			name:            "responses/effort_medium_promotes_high",
			provider:        providers.ProviderOpenAI,
			model:           "gpt-5.5",
			newClient:       openAIClient,
			inbound:         `{"model":"gpt-5.5","stream":true,"max_tokens":2048,"thinking":{"type":"enabled","budget_tokens":8192},"tools":` + weatherTool + `,"messages":[{"role":"user","content":"Weather in NYC?"}]}`,
			stream:          true,
			upstreamFixture: "responses/toolcall.upstream.sse",
			wantUpstream: func(t *testing.T, _ string, body []byte, _ http.Header) {
				assert.Equal(t, "high", gjson.GetBytes(body, "reasoning.effort").String(),
					"a medium-effort budget must promote to high for gpt-5.x (router #328 dead-zone)")
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { runConformanceCase(t, c) })
	}
}
