package proxy_test

// OpenAI /v1/chat/completions conformance cases (the format every OpenAI-compat
// upstream shares: OpenAI, OpenRouter, Fireworks, DeepInfra, Bedrock). Each
// pins a known translation behavior or past regression.

import (
	"net/http"
	"strings"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/providers/openaicompat"

	"github.com/stretchr/testify/assert"
	"github.com/tidwall/gjson"
)

func openRouterClient(baseURL string) providers.Client {
	return openaicompat.NewClient("test-key", baseURL)
}

const weatherTool = `[{"name":"get_weather","description":"Get the weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]`

func TestConformance_OpenAIChat(t *testing.T) {
	cases := []conformanceCase{
		{
			name:            "openai_chat/basic_text",
			provider:        providers.ProviderOpenRouter,
			model:           "deepseek/deepseek-v4-pro",
			newClient:       openRouterClient,
			inbound:         `{"model":"deepseek/deepseek-v4-pro","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"Say hi."}]}`,
			stream:          true,
			upstreamFixture: "openai_chat/basic_text.upstream.sse",
			wantUpstream: func(t *testing.T, path string, body []byte, _ http.Header) {
				assert.True(t, strings.HasSuffix(path, "/chat/completions"), "path=%s", path)
				assert.Equal(t, "deepseek/deepseek-v4-pro", gjson.GetBytes(body, "model").String())
				assert.True(t, gjson.GetBytes(body, "stream").Bool(), "inbound stream must propagate upstream")
				assert.Equal(t, "user", gjson.GetBytes(body, "messages.0.role").String())
			},
		},
		{
			name:            "openai_chat/basic_text_nonstream",
			provider:        providers.ProviderOpenRouter,
			model:           "deepseek/deepseek-v4-pro",
			newClient:       openRouterClient,
			inbound:         `{"model":"deepseek/deepseek-v4-pro","stream":false,"max_tokens":1024,"messages":[{"role":"user","content":"Say hi."}]}`,
			stream:          false,
			upstreamFixture: "openai_chat/basic_text.upstream.json",
			wantUpstream: func(t *testing.T, _ string, body []byte, _ http.Header) {
				assert.False(t, gjson.GetBytes(body, "stream").Bool(), "non-streaming inbound must not request an upstream stream")
			},
		},
		{
			name:            "openai_chat/toolcall",
			provider:        providers.ProviderOpenRouter,
			model:           "deepseek/deepseek-v4-pro",
			newClient:       openRouterClient,
			inbound:         `{"model":"deepseek/deepseek-v4-pro","stream":true,"max_tokens":1024,"tools":` + weatherTool + `,"messages":[{"role":"user","content":"Weather in NYC?"}]}`,
			stream:          true,
			upstreamFixture: "openai_chat/toolcall.upstream.sse",
			wantUpstream: func(t *testing.T, _ string, body []byte, _ http.Header) {
				assert.Equal(t, "get_weather", gjson.GetBytes(body, "tools.0.function.name").String(), "Anthropic tool must translate to OpenAI function shape")
			},
		},
		{
			// Guards router #293: an upstream that closes finish_reason="tool_calls"
			// but emits no usable (named) tool_use block must demote stop_reason to
			// end_turn, not strand the agent waiting for a tool call that never comes.
			name:            "openai_chat/degenerate_toolcall_demotes",
			provider:        providers.ProviderOpenRouter,
			model:           "deepseek/deepseek-v4-pro",
			newClient:       openRouterClient,
			inbound:         `{"model":"deepseek/deepseek-v4-pro","stream":true,"max_tokens":1024,"tools":` + weatherTool + `,"messages":[{"role":"user","content":"Run the tool."}]}`,
			stream:          true,
			upstreamFixture: "openai_chat/degenerate_toolcall.upstream.sse",
		},
		{
			// Guards the OpenRouter-only request gates: deepseek/* must carry the
			// provider-pin hint and reasoning:{enabled:false} so OpenRouter pins a
			// prefix-caching host and doesn't burn max_tokens on hidden thinking.
			name:            "openai_chat/openrouter_gates",
			provider:        providers.ProviderOpenRouter,
			model:           "deepseek/deepseek-v4-pro",
			newClient:       openRouterClient,
			inbound:         `{"model":"deepseek/deepseek-v4-pro","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"Say hi."}]}`,
			stream:          true,
			upstreamFixture: "openai_chat/basic_text.upstream.sse",
			wantUpstream: func(t *testing.T, _ string, body []byte, _ http.Header) {
				assert.Equal(t, "deepseek", gjson.GetBytes(body, "provider.order.0").String(), "deepseek/* must pin the deepseek upstream on OpenRouter")
				assert.False(t, gjson.GetBytes(body, "reasoning.enabled").Bool(), "reasoning must be disabled to avoid burning max_tokens on hidden thinking")
			},
		},
		{
			name:            "openai_chat/system_prompt",
			provider:        providers.ProviderOpenRouter,
			model:           "deepseek/deepseek-v4-pro",
			newClient:       openRouterClient,
			inbound:         `{"model":"deepseek/deepseek-v4-pro","stream":true,"max_tokens":1024,"system":"You are a helpful assistant.","messages":[{"role":"user","content":"Say hi."}]}`,
			stream:          true,
			upstreamFixture: "openai_chat/basic_text.upstream.sse",
			wantUpstream: func(t *testing.T, _ string, body []byte, _ http.Header) {
				assert.Equal(t, "system", gjson.GetBytes(body, "messages.0.role").String(), "top-level Anthropic system must become a leading system message")
				assert.Contains(t, gjson.GetBytes(body, "messages.0.content").String(), "helpful assistant")
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { runConformanceCase(t, c) })
	}
}
