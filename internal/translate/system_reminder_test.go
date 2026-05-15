package translate_test

import (
	"encoding/json"
	"strings"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// emittedMessages returns the `messages` array from an emitted OpenAI body.
func emittedMessages(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(body, &doc))
	raw, _ := doc["messages"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		msg, _ := m.(map[string]any)
		if msg != nil {
			out = append(out, msg)
		}
	}
	return out
}

// systemContent returns the concatenated text content of the first system
// message, or "" when none is present.
func systemContent(msgs []map[string]any) string {
	for _, msg := range msgs {
		if role, _ := msg["role"].(string); role != "system" {
			continue
		}
		switch c := msg["content"].(type) {
		case string:
			return c
		case []any:
			var parts []string
			for _, b := range c {
				block, _ := b.(map[string]any)
				if block == nil {
					continue
				}
				if text, _ := block["text"].(string); text != "" {
					parts = append(parts, text)
				}
			}
			return strings.Join(parts, "\n")
		}
	}
	return ""
}

const reminderSnippet = "byte-for-byte"

func TestSystemReminder_AnthropicSource_AppendsForDeepSeekWithTools(t *testing.T) {
	src := []byte(`{
		"model":"claude-opus-4-7",
		"system":"You are a helpful coding assistant.",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"Edit","description":"edit","input_schema":{"type":"object"}}],
		"max_tokens":256
	}`)
	env, err := translate.ParseAnthropic(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "deepseek/deepseek-v4-pro"})
	require.NoError(t, err)

	got := systemContent(emittedMessages(t, out.Body))
	assert.Contains(t, got, "You are a helpful coding assistant.", "original system text must be preserved")
	assert.Contains(t, got, reminderSnippet, "deepseek/* with tools must get the reminder")
}

func TestSystemReminder_AnthropicSource_SkipsWithoutTools(t *testing.T) {
	src := []byte(`{
		"model":"claude-opus-4-7",
		"system":"You are a helpful coding assistant.",
		"messages":[{"role":"user","content":"hi"}],
		"max_tokens":256
	}`)
	env, err := translate.ParseAnthropic(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "deepseek/deepseek-v4-pro"})
	require.NoError(t, err)

	assert.NotContains(t, systemContent(emittedMessages(t, out.Body)), reminderSnippet,
		"requests without tools must not get the reminder")
}

func TestSystemReminder_AnthropicSource_NoSystemMessagePrepended(t *testing.T) {
	src := []byte(`{
		"model":"claude-opus-4-7",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"Edit","description":"edit","input_schema":{"type":"object"}}],
		"max_tokens":256
	}`)
	env, err := translate.ParseAnthropic(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "deepseek/deepseek-v4-pro"})
	require.NoError(t, err)

	msgs := emittedMessages(t, out.Body)
	require.NotEmpty(t, msgs)
	assert.Equal(t, "system", msgs[0]["role"], "reminder must be prepended as a new system message")
	assert.Contains(t, msgs[0]["content"], reminderSnippet)
}

func TestSystemReminder_AnthropicSource_NoReminderForNonDeepSeek(t *testing.T) {
	cases := []string{"gpt-5", "moonshotai/kimi-k2.5", "qwen/qwen3-max", "google/gemini-2.5-pro"}
	for _, model := range cases {
		t.Run(model, func(t *testing.T) {
			src := []byte(`{
				"model":"claude-opus-4-7",
				"system":"sys",
				"messages":[{"role":"user","content":"hi"}],
				"tools":[{"name":"Edit","description":"edit","input_schema":{"type":"object"}}],
				"max_tokens":256
			}`)
			env, err := translate.ParseAnthropic(src)
			require.NoError(t, err)

			out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: model})
			require.NoError(t, err)

			assert.NotContains(t, systemContent(emittedMessages(t, out.Body)), reminderSnippet,
				"non-deepseek targets must not receive the reminder")
		})
	}
}

func TestSystemReminder_OpenAISource_AppendsForDeepSeekWithTools(t *testing.T) {
	src := []byte(`{
		"model":"x",
		"messages":[
			{"role":"system","content":"be concise"},
			{"role":"user","content":"hi"}
		],
		"tools":[{"type":"function","function":{"name":"Edit","parameters":{"type":"object"}}}],
		"max_tokens":256
	}`)
	env, err := translate.ParseOpenAI(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "deepseek/deepseek-v4-pro"})
	require.NoError(t, err)

	got := systemContent(emittedMessages(t, out.Body))
	assert.Contains(t, got, "be concise", "original system content preserved")
	assert.Contains(t, got, reminderSnippet, "OpenAI→OpenAI path must inject reminder for deepseek/* with tools")
}

func TestSystemReminder_OpenAISource_SkipsWithoutTools(t *testing.T) {
	src := []byte(`{
		"model":"x",
		"messages":[
			{"role":"system","content":"be concise"},
			{"role":"user","content":"hi"}
		],
		"max_tokens":256
	}`)
	env, err := translate.ParseOpenAI(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "deepseek/deepseek-v4-pro"})
	require.NoError(t, err)

	assert.NotContains(t, systemContent(emittedMessages(t, out.Body)), reminderSnippet)
}
