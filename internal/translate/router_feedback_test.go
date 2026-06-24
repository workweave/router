package translate_test

import (
	"encoding/json"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRouterFeedbackCommand(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantRating   string
		wantFeedback string
		wantFound    bool
		wantStripped string
	}{
		{
			name:         "command with feedback text",
			input:        "/router-feedback got stuck on Haiku for too long",
			wantFeedback: "got stuck on Haiku for too long",
			wantFound:    true,
			wantStripped: "",
		},
		{
			name:       "rf plus shortcut is a thumbs up with no note",
			input:      "/rf+",
			wantRating: "up",
			wantFound:  true,
		},
		{
			name:       "rf minus shortcut is a thumbs down with no note",
			input:      "/rf-",
			wantRating: "down",
			wantFound:  true,
		},
		{
			name:         "rf minus shortcut carries a trailing note",
			input:        "/rf- stuck on haiku for too long",
			wantRating:   "down",
			wantFeedback: "stuck on haiku for too long",
			wantFound:    true,
		},
		{
			name:         "rf with leading thumbs emoji promotes to rating",
			input:        "/rf 👍 nailed the model choice",
			wantRating:   "up",
			wantFeedback: "nailed the model choice",
			wantFound:    true,
		},
		{
			name:         "rf with leading word down promotes to rating",
			input:        "/rf down wrong tier for this",
			wantRating:   "down",
			wantFeedback: "wrong tier for this",
			wantFound:    true,
		},
		{
			name:       "rf space plus promotes to thumbs up",
			input:      "/rf +",
			wantRating: "up",
			wantFound:  true,
		},
		{
			name:       "rf space minus promotes to thumbs down",
			input:      "/rf -",
			wantRating: "down",
			wantFound:  true,
		},
		{
			name:         "rf space minus carries a trailing note",
			input:        "/rf - too slow",
			wantRating:   "down",
			wantFeedback: "too slow",
			wantFound:    true,
		},
		{
			name:         "note without a verdict keeps empty rating",
			input:        "/rf the diff was incomplete",
			wantFeedback: "the diff was incomplete",
			wantFound:    true,
		},
		{
			name:         "multiline feedback is captured whole",
			input:        "/router-feedback the model kept looping\nit re-read the same file 40 times",
			wantFeedback: "the model kept looping\nit re-read the same file 40 times",
			wantFound:    true,
			wantStripped: "",
		},
		{
			name:         "bare command matches with empty feedback",
			input:        "/router-feedback",
			wantFeedback: "",
			wantFound:    true,
			wantStripped: "",
		},
		{
			// /rf is the router-side alias for clients without local
			// slash-command expansion (pi, opencode, raw API callers).
			name:         "rf alias with feedback text",
			input:        "/rf kept thrashing between models",
			wantFeedback: "kept thrashing between models",
			wantFound:    true,
			wantStripped: "",
		},
		{
			name:         "bare rf alias matches with empty feedback",
			input:        "/rf",
			wantFeedback: "",
			wantFound:    true,
			wantStripped: "",
		},
		{
			// /rfoo must not match the alias prefix.
			name:      "rf alias without space boundary is ignored",
			input:     "/rfoo bar",
			wantFound: false,
		},
		{
			// Security guard shared with /force-model: pasted content containing
			// the command mid-message must not be intercepted.
			name:      "command after leading text is ignored",
			input:     "Please help me.\n/router-feedback bad routing",
			wantFound: false,
		},
		{
			name:      "no command",
			input:     "Can you help me debug this code?",
			wantFound: false,
		},
		{
			// /router-feedbackgarbage must not match the prefix.
			name:      "prefix without space or exact match is ignored",
			input:     "/router-feedbackish text",
			wantFound: false,
		},
		{
			name:         "leading system-reminder before command",
			input:        "<system-reminder>be helpful</system-reminder>\n/router-feedback too slow on opus",
			wantFeedback: "too slow on opus",
			wantFound:    true,
			wantStripped: "<system-reminder>be helpful</system-reminder>",
		},
		{
			name:      "unclosed tag does not unlock leading-line guard",
			input:     "<system-reminder>unclosed\n/router-feedback bad",
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := translate.ParseAnthropic(buildAnthropicBody(t, tt.input))
			require.NoError(t, err)

			res, found := env.ExtractRouterFeedbackCommand()
			assert.Equal(t, tt.wantFound, found)
			if !tt.wantFound {
				return
			}
			assert.Equal(t, tt.wantRating, res.Rating)
			assert.Equal(t, tt.wantFeedback, res.Feedback)

			stripped := lastUserMessageText(t, env)
			assert.Equal(t, tt.wantStripped, stripped)
		})
	}
}

func TestExtractRouterFeedbackCommand_OpenAIFormat(t *testing.T) {
	body := mustMarshalJSON(t, map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{"role": "user", "content": "/router-feedback wrong model for this task"},
		},
	})
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)

	res, found := env.ExtractRouterFeedbackCommand()
	require.True(t, found)
	assert.Equal(t, "wrong model for this task", res.Feedback)
	assert.Equal(t, "", lastOpenAIUserMessageText(t, env))
}

func TestExtractRouterFeedbackCommand_ArrayContentMultipleTextBlocks(t *testing.T) {
	// Claude Code splits slash-command turns into an injected-tags block plus
	// the typed directive; the parser must scan every text block.
	body := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "<command-message>router-feedback</command-message>\n<command-name>/router-feedback</command-name>\n<command-args>stuck on haiku</command-args>"},
					map[string]any{"type": "text", "text": "/router-feedback stuck on haiku"},
				},
			},
		},
		"max_tokens": 1024,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	res, found := env.ExtractRouterFeedbackCommand()
	require.True(t, found, "directive in a non-first text block must still be recognized")
	assert.Equal(t, "stuck on haiku", res.Feedback)

	raw, _ := env.PrepareAnthropic(nil, translate.EmitOptions{TargetModel: "claude-sonnet-4-6"})
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw.Body, &got))
	msgs, _ := got["messages"].([]any)
	last, _ := msgs[len(msgs)-1].(map[string]any)
	blocks, _ := last["content"].([]any)
	require.Len(t, blocks, 2)
	second, _ := blocks[1].(map[string]any)
	assert.Equal(t, "", second["text"], "the directive-bearing text block must be stripped")
}

func TestExtractRouterFeedbackCommand_GeminiFormatIgnored(t *testing.T) {
	body := mustMarshalJSON(t, map[string]any{
		"contents": []any{
			map[string]any{"role": "user", "parts": []any{map[string]any{"text": "/router-feedback bad"}}},
		},
	})
	env, err := translate.ParseGemini(body)
	require.NoError(t, err)

	_, found := env.ExtractRouterFeedbackCommand()
	assert.False(t, found, "Gemini format should not be scanned for router-feedback commands")
}
