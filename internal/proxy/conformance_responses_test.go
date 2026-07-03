package proxy_test

// OpenAI Responses API conformance: reasoning gpt-5.x + tools routes here
// instead of chat/completions. Guards #331 (must stream upstream) and #328
// (medium-effort promotion).

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
			// Full Responses-SSE -> Anthropic translation plus the load-bearing stream:true guard.
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
		{
			// Strictifiable schemas must go out strict:true, additionalProperties:false,
			// all-required, optionals as null unions.
			name:            "responses/strict_tools",
			provider:        providers.ProviderOpenAI,
			model:           "gpt-5.5",
			newClient:       openAIClient,
			inbound:         `{"model":"gpt-5.5","stream":true,"max_tokens":2048,"thinking":{"type":"enabled","budget_tokens":24576},"tools":` + readTool + `,"messages":[{"role":"user","content":"Read a.go"}]}`,
			stream:          true,
			upstreamFixture: "responses/toolcall.upstream.sse",
			wantUpstream: func(t *testing.T, _ string, body []byte, _ http.Header) {
				tool := gjson.GetBytes(body, "tools.0")
				assert.True(t, tool.Get("strict").Bool(), "strictifiable schema must opt into strict mode")
				params := tool.Get("parameters")
				assert.False(t, params.Get("additionalProperties").Bool(), "strict requires additionalProperties:false")
				required := []string{}
				params.Get("required").ForEach(func(_, r gjson.Result) bool {
					required = append(required, r.String())
					return true
				})
				assert.ElementsMatch(t, []string{"file_path", "pages", "limit"}, required,
					"strict requires every property listed in required")
				assert.Equal(t, `["string","null"]`, params.Get("properties.pages.type").Raw,
					"originally-optional params must become null unions, not stay omittable")
			},
		},
		{
			// toolcheck must repair a truncated function_call arguments payload
			// (missing closing brace) before it reaches the client.
			name:            "responses/invalid_toolcall",
			provider:        providers.ProviderOpenAI,
			model:           "gpt-5.5",
			newClient:       openAIClient,
			inbound:         `{"model":"gpt-5.5","stream":true,"max_tokens":2048,"thinking":{"type":"enabled","budget_tokens":24576},"tools":` + readTool + `,"messages":[{"role":"user","content":"Read a.go"}]}`,
			stream:          true,
			upstreamFixture: "responses/invalid_toolcall.upstream.sse",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { runConformanceCase(t, c) })
	}
}
