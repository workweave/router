package proxy_test

// Gemini native (:generateContent / :streamGenerateContent) conformance cases.
// The router serves Gemini through the native REST surface (not the OpenAI-compat
// one) because only native preserves thoughtSignature for Gemini 3.x tool use.
// These pin the recent Gemini request-translation regressions: thinkingLevel vs
// legacy thinkingBudget, and thoughtSignature round-trip.

import (
	"net/http"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/providers/google"

	"github.com/stretchr/testify/assert"
	"github.com/tidwall/gjson"
)

func geminiClient(baseURL string) providers.Client {
	return google.NewNativeClient("test-key", baseURL)
}

func TestConformance_GeminiNative(t *testing.T) {
	cases := []conformanceCase{
		{
			name:            "gemini_native/basic_text",
			provider:        providers.ProviderGoogle,
			model:           "gemini-3.1-pro-preview",
			newClient:       geminiClient,
			inbound:         `{"model":"gemini-3.1-pro-preview","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"Say hi."}]}`,
			stream:          true,
			upstreamFixture: "gemini_native/basic_text.upstream.sse",
			wantUpstream: func(t *testing.T, path string, body []byte, _ http.Header) {
				assert.Contains(t, path, "gemini-3.1-pro-preview:streamGenerateContent", "streaming inbound must hit the native streaming surface")
				assert.Equal(t, "user", gjson.GetBytes(body, "contents.0.role").String())
			},
		},
		{
			name:            "gemini_native/toolcall",
			provider:        providers.ProviderGoogle,
			model:           "gemini-3.1-pro-preview",
			newClient:       geminiClient,
			inbound:         `{"model":"gemini-3.1-pro-preview","stream":true,"max_tokens":1024,"tools":` + weatherTool + `,"messages":[{"role":"user","content":"Weather in NYC?"}]}`,
			stream:          true,
			upstreamFixture: "gemini_native/toolcall.upstream.sse",
			wantUpstream: func(t *testing.T, _ string, body []byte, _ http.Header) {
				assert.Equal(t, "get_weather", gjson.GetBytes(body, "tools.0.functionDeclarations.0.name").String(), "Anthropic tools must map to Gemini functionDeclarations")
			},
		},
		{
			// Guards router #328/#121: Gemini 3.x must use thinkingLevel (string),
			// NOT the legacy numeric thinkingBudget — mixing both 400s.
			name:            "gemini_native/thinking_level_gemini3",
			provider:        providers.ProviderGoogle,
			model:           "gemini-3.1-pro-preview",
			newClient:       geminiClient,
			inbound:         `{"model":"gemini-3.1-pro-preview","stream":true,"max_tokens":1024,"thinking":{"type":"enabled","budget_tokens":24576},"messages":[{"role":"user","content":"Think hard."}]}`,
			stream:          true,
			upstreamFixture: "gemini_native/basic_text.upstream.sse",
			wantUpstream: func(t *testing.T, _ string, body []byte, _ http.Header) {
				tc := gjson.GetBytes(body, "generationConfig.thinkingConfig")
				assert.Equal(t, "high", tc.Get("thinkingLevel").String(), "gemini-3.x must send a string thinkingLevel")
				assert.False(t, tc.Get("thinkingBudget").Exists(), "gemini-3.x must NOT send the legacy numeric thinkingBudget")
			},
		},
		{
			// The 2.5 counterpart: numeric thinkingBudget, no thinkingLevel.
			name:            "gemini_native/thinking_budget_gemini25",
			provider:        providers.ProviderGoogle,
			model:           "gemini-2.5-pro",
			newClient:       geminiClient,
			inbound:         `{"model":"gemini-2.5-pro","stream":true,"max_tokens":1024,"thinking":{"type":"enabled","budget_tokens":24576},"messages":[{"role":"user","content":"Think hard."}]}`,
			stream:          true,
			upstreamFixture: "gemini_native/basic_text.upstream.sse",
			wantUpstream: func(t *testing.T, _ string, body []byte, _ http.Header) {
				tc := gjson.GetBytes(body, "generationConfig.thinkingConfig")
				assert.EqualValues(t, 24576, tc.Get("thinkingBudget").Int(), "gemini-2.5 keeps the numeric thinkingBudget")
				assert.False(t, tc.Get("thinkingLevel").Exists(), "gemini-2.5 must NOT send thinkingLevel")
			},
		},
		{
			// Guards the load-bearing thoughtSignature round-trip: a prior assistant
			// tool_use's signature must ride back onto the Gemini functionCall part,
			// or the next turn 400s on Gemini 3.x preview models.
			name:      "gemini_native/thought_signature_roundtrip",
			provider:  providers.ProviderGoogle,
			model:     "gemini-3.1-pro-preview",
			newClient: geminiClient,
			inbound: `{"model":"gemini-3.1-pro-preview","stream":true,"max_tokens":1024,"tools":` + weatherTool + `,"messages":[` +
				`{"role":"user","content":"Weather in NYC?"},` +
				`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"NYC"},"thought_signature":"SIG_ROUNDTRIP"}]},` +
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"sunny"}]}` +
				`]}`,
			stream:          true,
			upstreamFixture: "gemini_native/basic_text.upstream.sse",
			wantUpstream: func(t *testing.T, _ string, body []byte, _ http.Header) {
				assert.Contains(t, string(body), `"thoughtSignature":"SIG_ROUNDTRIP"`,
					"prior assistant tool_use signature must round-trip onto the Gemini functionCall part")
			},
		},
		{
			// Strict-decode prevention layer: tools present with no forced
			// tool_choice must emit functionCallingConfig.mode=VALIDATED on
			// Gemini 3.x — schema-constrained tool args at decode time without
			// forcing a function call.
			name:            "gemini_native/validated_mode",
			provider:        providers.ProviderGoogle,
			model:           "gemini-3.1-pro-preview",
			newClient:       geminiClient,
			inbound:         `{"model":"gemini-3.1-pro-preview","stream":true,"max_tokens":1024,"tools":` + weatherTool + `,"messages":[{"role":"user","content":"Weather in NYC?"}]}`,
			stream:          true,
			upstreamFixture: "gemini_native/toolcall.upstream.sse",
			wantUpstream: func(t *testing.T, _ string, body []byte, _ http.Header) {
				assert.Equal(t, "VALIDATED", gjson.GetBytes(body, "toolConfig.functionCallingConfig.mode").String(),
					"tools + unforced tool_choice on Gemini 3.x must request VALIDATED decoding")
			},
		},
		{
			// An explicit client tool_choice must never be clobbered by the
			// VALIDATED upgrade: forced single-tool stays ANY + allowlist.
			name:            "gemini_native/validated_mode_forced_tool_preserved",
			provider:        providers.ProviderGoogle,
			model:           "gemini-3.1-pro-preview",
			newClient:       geminiClient,
			inbound:         `{"model":"gemini-3.1-pro-preview","stream":true,"max_tokens":1024,"tools":` + weatherTool + `,"tool_choice":{"type":"tool","name":"get_weather"},"messages":[{"role":"user","content":"Weather in NYC?"}]}`,
			stream:          true,
			upstreamFixture: "gemini_native/toolcall.upstream.sse",
			wantUpstream: func(t *testing.T, _ string, body []byte, _ http.Header) {
				fcc := gjson.GetBytes(body, "toolConfig.functionCallingConfig")
				assert.Equal(t, "ANY", fcc.Get("mode").String(), "a forced tool keeps mode=ANY")
				assert.Equal(t, "get_weather", fcc.Get("allowedFunctionNames.0").String())
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { runConformanceCase(t, c) })
	}
}
