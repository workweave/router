package proxy_test

// Anthropic same-format conformance cases. The Anthropic provider is a
// passthrough (no response translation), so these mainly guard request-side
// emit behavior — notably hoisting a role:system message out of the messages
// array, which otherwise 400s on a same-format bounce.

import (
	"net/http"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/providers/anthropic"

	"github.com/stretchr/testify/assert"
	"github.com/tidwall/gjson"
)

func anthropicClient(baseURL string) providers.Client {
	return anthropic.NewClient("test-key", baseURL)
}

func TestConformance_Anthropic(t *testing.T) {
	cases := []conformanceCase{
		{
			name:            "anthropic/passthrough_text",
			provider:        providers.ProviderAnthropic,
			model:           "claude-opus-4-8",
			newClient:       anthropicClient,
			inbound:         `{"model":"claude-opus-4-8","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"Say hi."}]}`,
			stream:          true,
			upstreamFixture: "anthropic/basic_text.upstream.sse",
			wantUpstream: func(t *testing.T, path string, _ []byte, _ http.Header) {
				assert.Equal(t, "/v1/messages", path)
			},
		},
		{
			// Guards router #332: a role:system message inside the messages array
			// must be hoisted to the top-level system field; left in place it 400s
			// on a same-format Anthropic bounce.
			name:            "anthropic/system_role_hoisted",
			provider:        providers.ProviderAnthropic,
			model:           "claude-opus-4-8",
			newClient:       anthropicClient,
			inbound:         `{"model":"claude-opus-4-8","stream":true,"max_tokens":1024,"messages":[{"role":"system","content":"Be terse."},{"role":"user","content":"hi"}]}`,
			stream:          true,
			upstreamFixture: "anthropic/basic_text.upstream.sse",
			wantUpstream: func(t *testing.T, _ string, body []byte, _ http.Header) {
				assert.Contains(t, gjson.GetBytes(body, "system").Raw, "Be terse", "role:system must be hoisted into the top-level system field")
				assert.NotEqual(t, "system", gjson.GetBytes(body, "messages.0.role").String(), "no system role may remain in the messages array")
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { runConformanceCase(t, c) })
	}
}
