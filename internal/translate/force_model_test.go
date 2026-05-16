package translate_test

import (
	"encoding/json"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseForceModelCommand_ForceModel(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantModel    string
		wantFound    bool
		wantStripped string
	}{
		{
			name:         "command only",
			input:        "/force-model deepseek/deepseek-v4-pro",
			wantModel:    "deepseek/deepseek-v4-pro",
			wantFound:    true,
			wantStripped: "",
		},
		{
			name:         "command with trailing text",
			input:        "/force-model claude-opus-4-7\nPlease help me with this.",
			wantModel:    "claude-opus-4-7",
			wantFound:    true,
			wantStripped: "Please help me with this.",
		},
		{
			name:         "command with leading text",
			input:        "Please help me.\n/force-model gemini-2.5-pro",
			wantModel:    "gemini-2.5-pro",
			wantFound:    true,
			wantStripped: "Please help me.",
		},
		{
			name:      "no command",
			input:     "Can you help me debug this code?",
			wantFound: false,
		},
		{
			name:      "force-model without model name is ignored",
			input:     "/force-model ",
			wantFound: false,
		},
		{
			name:         "leading and trailing whitespace on command line",
			input:        "  /force-model   qwen/qwen3-235b-a22b-2507  ",
			wantModel:    "qwen/qwen3-235b-a22b-2507",
			wantFound:    true,
			wantStripped: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := translate.ParseAnthropic(buildAnthropicBody(t, tt.input))
			require.NoError(t, err)

			res, found := env.ExtractForceModelCommand()
			assert.Equal(t, tt.wantFound, found)
			if !tt.wantFound {
				return
			}
			assert.Equal(t, tt.wantModel, res.Model)
			assert.False(t, res.Clear)

			// Verify the command was stripped from env body.
			stripped := lastUserMessageText(t, env)
			assert.Equal(t, tt.wantStripped, stripped)
		})
	}
}

func TestParseForceModelCommand_UnforceModel(t *testing.T) {
	env, err := translate.ParseAnthropic(buildAnthropicBody(t, "/unforce-model"))
	require.NoError(t, err)

	res, found := env.ExtractForceModelCommand()
	require.True(t, found)
	assert.True(t, res.Clear)
	assert.Empty(t, res.Model)
}

func TestExtractForceModelCommand_ArrayContent(t *testing.T) {
	body := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "/force-model gpt-5\nHelp me."},
				},
			},
		},
		"max_tokens": 1024,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	res, found := env.ExtractForceModelCommand()
	require.True(t, found)
	assert.Equal(t, "gpt-5", res.Model)

	stripped := lastUserMessageArrayText(t, env)
	assert.Equal(t, "Help me.", stripped)
}

func TestExtractForceModelCommand_NoUserMessage(t *testing.T) {
	body := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "assistant", "content": "Hello!"},
		},
		"max_tokens": 1024,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	_, found := env.ExtractForceModelCommand()
	assert.False(t, found)
}

func TestExtractForceModelCommand_GeminiFormatIgnored(t *testing.T) {
	body := mustMarshalJSON(t, map[string]any{
		"contents": []any{
			map[string]any{"role": "user", "parts": []any{map[string]any{"text": "/force-model gpt-5"}}},
		},
	})
	env, err := translate.ParseGemini(body)
	require.NoError(t, err)

	_, found := env.ExtractForceModelCommand()
	assert.False(t, found, "Gemini format should not be scanned for force-model commands")
}

// buildAnthropicBody creates a minimal Anthropic Messages request with text as
// the sole user message content.
func buildAnthropicBody(t *testing.T, text string) []byte {
	t.Helper()
	return mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "user", "content": text},
		},
		"max_tokens": 1024,
	})
}

func lastUserMessageText(t *testing.T, env *translate.RequestEnvelope) string {
	t.Helper()
	var body map[string]any
	raw, _ := env.PrepareAnthropic(nil, translate.EmitOptions{TargetModel: "claude-sonnet-4-6"})
	require.NoError(t, json.Unmarshal(raw.Body, &body))
	msgs, _ := body["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		msg, _ := msgs[i].(map[string]any)
		if msg["role"] == "user" {
			content, _ := msg["content"].(string)
			return content
		}
	}
	return ""
}

func lastUserMessageArrayText(t *testing.T, env *translate.RequestEnvelope) string {
	t.Helper()
	var body map[string]any
	raw, _ := env.PrepareAnthropic(nil, translate.EmitOptions{TargetModel: "claude-sonnet-4-6"})
	require.NoError(t, json.Unmarshal(raw.Body, &body))
	msgs, _ := body["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		msg, _ := msgs[i].(map[string]any)
		if msg["role"] != "user" {
			continue
		}
		blocks, _ := msg["content"].([]any)
		for _, b := range blocks {
			block, _ := b.(map[string]any)
			if block["type"] == "text" {
				return block["text"].(string)
			}
		}
	}
	return ""
}

func mustMarshalJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
