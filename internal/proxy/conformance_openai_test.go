package proxy_test

// OpenAI /v1/chat/completions conformance cases, shared by every OpenAI-compat
// upstream. Each pins a known translation behavior or past regression.

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

// readTool mirrors the Claude Code Read tool shape: required param, optional
// string, integer, closed schema — for toolcheck validate/repair cases.
const readTool = `[{"name":"Read","description":"Read a file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"pages":{"type":"string"},"limit":{"type":"integer"}},"required":["file_path"],"additionalProperties":false}}]`

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
			// Guards #293: finish_reason="tool_calls" with no usable tool_use
			// block must demote stop_reason to end_turn, not strand the agent.
			name:            "openai_chat/degenerate_toolcall_demotes",
			provider:        providers.ProviderOpenRouter,
			model:           "deepseek/deepseek-v4-pro",
			newClient:       openRouterClient,
			inbound:         `{"model":"deepseek/deepseek-v4-pro","stream":true,"max_tokens":1024,"tools":` + weatherTool + `,"messages":[{"role":"user","content":"Run the tool."}]}`,
			stream:          true,
			upstreamFixture: "openai_chat/degenerate_toolcall.upstream.sse",
		},
		{
			// deepseek/* must carry the provider-pin hint and
			// reasoning:{enabled:false} to pin a caching host and avoid
			// burning max_tokens on hidden thinking.
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
		{
			// Unrepairable args degrade to `{}` (never raw malformed bytes)
			// so the tool_use block stays dispatchable.
			name:            "openai_chat/invalid_toolcall_args",
			provider:        providers.ProviderOpenRouter,
			model:           "deepseek/deepseek-v4-pro",
			newClient:       openRouterClient,
			inbound:         `{"model":"deepseek/deepseek-v4-pro","stream":true,"max_tokens":1024,"tools":` + readTool + `,"messages":[{"role":"user","content":"Read a.go"}]}`,
			stream:          true,
			upstreamFixture: "openai_chat/invalid_toolcall_args.upstream.sse",
		},
		{
			// Normalize+repair: empty-string optional dropped (#339),
			// hallucinated param dropped, "5"-for-integer coerced.
			name:            "openai_chat/toolcall_repaired_args",
			provider:        providers.ProviderOpenRouter,
			model:           "deepseek/deepseek-v4-pro",
			newClient:       openRouterClient,
			inbound:         `{"model":"deepseek/deepseek-v4-pro","stream":true,"max_tokens":1024,"tools":` + readTool + `,"messages":[{"role":"user","content":"Read a.go"}]}`,
			stream:          true,
			upstreamFixture: "openai_chat/toolcall_repaired_args.upstream.sse",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { runConformanceCase(t, c) })
	}
}
