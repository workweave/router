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
		{
			// Guards the 2026-06-09 re-route 400: a session running with
			// output_config.effort="xhigh" (valid for the requested opus-4-7+)
			// re-routed onto claude-sonnet-4-6 must have the effort clamped to
			// "max" — sonnet's menu has no xhigh and the resulting 400 is
			// non-retryable, killing the session.
			name:            "anthropic/xhigh_effort_clamped_on_reroute",
			provider:        providers.ProviderAnthropic,
			model:           "claude-sonnet-4-6",
			newClient:       anthropicClient,
			inbound:         `{"model":"claude-opus-4-7","stream":true,"max_tokens":1024,"thinking":{"type":"adaptive"},"output_config":{"effort":"xhigh"},"messages":[{"role":"user","content":"hi"}]}`,
			stream:          true,
			upstreamFixture: "anthropic/basic_text.upstream.sse",
			wantUpstream: func(t *testing.T, _ string, body []byte, _ http.Header) {
				assert.Equal(t, "claude-sonnet-4-6", gjson.GetBytes(body, "model").String())
				assert.Equal(t, "max", gjson.GetBytes(body, "output_config.effort").String(), "xhigh must clamp to max for models without CapXhighEffort")
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { runConformanceCase(t, c) })
	}
}
