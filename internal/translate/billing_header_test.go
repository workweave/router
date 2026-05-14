package translate

import (
	"strings"
	"testing"

	"workweave/router/internal/router"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStripAnthropicBillingHeader(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "claude code v2.1 prelude",
			in:   "x-anthropic-billing-header: cc_version=2.1.141.bb8; cc_entrypoint=cli; cch=54d19;\nYou are Claude Code, Anthropic's official CLI for Claude.",
			want: "You are Claude Code, Anthropic's official CLI for Claude.",
		},
		{
			name: "no header passes through unchanged",
			in:   "You are a helpful assistant.",
			want: "You are a helpful assistant.",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
		{
			name: "standalone billing block with no trailing newline strips to empty",
			in:   "x-anthropic-billing-header: cc_version=2.1.141.bb8; cc_entrypoint=cli; cch=6c6ec;",
			want: "",
		},
		{
			name: "header anywhere but the start is kept",
			in:   "preamble\nx-anthropic-billing-header: cch=abc;\nbody",
			want: "preamble\nx-anthropic-billing-header: cch=abc;\nbody",
		},
		{
			name: "only the first line is consumed",
			in:   "x-anthropic-billing-header: cch=abc;\nsystem\nx-anthropic-billing-header: cch=def;\nstays",
			want: "system\nx-anthropic-billing-header: cch=def;\nstays",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stripAnthropicBillingHeader(c.in)
			assert.Equal(t, c.want, got)
			// Idempotency: repeated application must not change the result.
			assert.Equal(t, got, stripAnthropicBillingHeader(got))
		})
	}
}

// TestPrepareOpenAIStripsBillingHeaderFromSystemArray reproduces the exact
// inbound shape Claude Code sends — a `system` array whose first text block
// is just the billing header with NO trailing newline, and whose second
// block carries the real system prompt. The naive per-block regex without
// "\n?" would skip the strip; this test guards against that regression.
func TestPrepareOpenAIStripsBillingHeaderFromSystemArray(t *testing.T) {
	inbound := []byte(`{
		"model": "claude-opus-4-7",
		"max_tokens": 64,
		"system": [
			{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.141.bb8; cc_entrypoint=cli; cch=6c6ec;"},
			{"type": "text", "text": "You are Claude Code.", "cache_control": {"type": "ephemeral", "ttl": "1h"}}
		],
		"messages": [{"role": "user", "content": "hi"}]
	}`)
	env, err := ParseAnthropic(inbound)
	require.NoError(t, err)
	prep, err := env.PrepareOpenAI(nil, EmitOptions{
		TargetModel:  "deepseek/deepseek-v4-pro",
		Capabilities: router.Lookup("deepseek/deepseek-v4-pro"),
	})
	require.NoError(t, err)
	body := string(prep.Body)
	require.NotContains(t, body, "x-anthropic-billing-header", "billing header must be stripped; body=%s", body)
	require.True(t, strings.Contains(body, "You are Claude Code."), "real system prompt must survive")
}

// TestPrepareGeminiStripsBillingHeaderFromSystemArray is the Gemini-emit
// twin of the OpenAI test above — same inbound shape, same expectation.
func TestPrepareGeminiStripsBillingHeaderFromSystemArray(t *testing.T) {
	inbound := []byte(`{
		"model": "claude-opus-4-7",
		"max_tokens": 64,
		"system": [
			{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.141.bb8; cc_entrypoint=cli; cch=6c6ec;"},
			{"type": "text", "text": "You are Claude Code."}
		],
		"messages": [{"role": "user", "content": "hi"}]
	}`)
	env, err := ParseAnthropic(inbound)
	require.NoError(t, err)
	prep, err := env.PrepareGemini(nil, EmitOptions{
		TargetModel:  "gemini-2.5-flash",
		Capabilities: router.Lookup("gemini-2.5-flash"),
	})
	require.NoError(t, err)
	body := string(prep.Body)
	require.NotContains(t, body, "x-anthropic-billing-header", "billing header must be stripped; body=%s", body)
	require.True(t, strings.Contains(body, "You are Claude Code."), "real system prompt must survive")
}
