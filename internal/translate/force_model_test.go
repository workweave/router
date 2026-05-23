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
			name:         "command with same-line trailing prompt",
			input:        "/force-model gpt-5 help me debug this",
			wantModel:    "gpt-5",
			wantFound:    true,
			wantStripped: "help me debug this",
		},
		{
			// Security guard: a /force-model anywhere other than the leading
			// non-empty line is ignored. Pasted content (snippets, transcripts,
			// log dumps) frequently contains lines starting with "/" and must
			// not silently rewrite session routing.
			name:      "command after leading text is ignored",
			input:     "Please help me.\n/force-model gemini-2.5-pro",
			wantFound: false,
		},
		{
			name:         "leading blank lines before command",
			input:        "\n\n/force-model claude-opus-4-7\nthen help",
			wantModel:    "claude-opus-4-7",
			wantFound:    true,
			wantStripped: "then help",
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
		{
			// Claude Code injects <system-reminder> blocks ahead of the user's
			// typed text. The command must still be recognized; injected blocks
			// must be preserved in the stripped output.
			name:         "leading system-reminder before command",
			input:        "<system-reminder>be helpful</system-reminder>\n/force-model gpt-5",
			wantModel:    "gpt-5",
			wantFound:    true,
			wantStripped: "<system-reminder>be helpful</system-reminder>",
		},
		{
			name:         "multiple leading injected tag blocks",
			input:        "<system-reminder>foo</system-reminder>\n<command-name>x</command-name>\n/force-model claude-opus-4-7\nthen help",
			wantModel:    "claude-opus-4-7",
			wantFound:    true,
			wantStripped: "<system-reminder>foo</system-reminder>\n<command-name>x</command-name>\nthen help",
		},
		{
			name:         "multiline system-reminder body",
			input:        "<system-reminder>\nline one\nline two\n</system-reminder>\n/force-model gemini-2.5-pro",
			wantModel:    "gemini-2.5-pro",
			wantFound:    true,
			wantStripped: "<system-reminder>\nline one\nline two\n</system-reminder>",
		},
		{
			// Security guard preserved: an unclosed tag does not satisfy the
			// prefix matcher, so a stray /force-model after it is still ignored.
			name:      "unclosed tag does not unlock leading-line guard",
			input:     "<system-reminder>unclosed\n/force-model gpt-5",
			wantFound: false,
		},
		{
			// Tags with attributes are not part of Claude Code's injection set
			// and must not unlock the guard — they may originate from pasted
			// HTML/XML content.
			name:      "tag with attributes does not unlock leading-line guard",
			input:     "<div class=\"x\">hi</div>\n/force-model gpt-5",
			wantFound: false,
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

func TestExtractForceModelCommand_OpenAIFormat(t *testing.T) {
	body := mustMarshalJSON(t, map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{"role": "user", "content": "/force-model gpt-5\nhelp me."},
		},
	})
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)

	res, found := env.ExtractForceModelCommand()
	require.True(t, found)
	assert.Equal(t, "gpt-5", res.Model)
	assert.False(t, res.Clear)
	assert.Equal(t, "help me.", lastOpenAIUserMessageText(t, env))
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

func lastOpenAIUserMessageText(t *testing.T, env *translate.RequestEnvelope) string {
	t.Helper()
	prep, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "gpt-4o"})
	require.NoError(t, err)

	var body map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &body))
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
