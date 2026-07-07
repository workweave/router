package translate_test

import (
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrailingAssistantTexts_ExtractsNarrationInOrder(t *testing.T) {
	body := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "user", "content": "do stuff"},
			// text + tool_use in one message: only the text is narration.
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "first"},
				map[string]any{"type": "tool_use", "id": "1", "name": "ls", "input": map[string]any{}},
			}},
			// tool_result is not a real user turn, so the run continues through it.
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "1", "content": "ok"},
			}},
			// multiple text blocks concatenate; thinking is ignored.
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "hidden reasoning"},
				map[string]any{"type": "text", "text": "second"},
				map[string]any{"type": "text", "text": "third"},
			}},
			// tool-only assistant message contributes nothing.
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "2", "name": "read", "input": map[string]any{}},
			}},
		},
		"max_tokens": 256,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	require.Equal(t, []string{"first", "second\nthird"}, env.TrailingAssistantTexts())
}

func TestTrailingAssistantTexts_ResetsAtLastRealUserTurn(t *testing.T) {
	// Narration before a real user message belongs to a prior task: after a
	// break clears the pin and the user sends a follow-up, the stale loop
	// narration must NOT count against the fresh model (Cursor Bugbot #665).
	body := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "assistant", "content": "stale narration from the looping model"},
			map[string]any{"role": "assistant", "content": "more stale narration from the loop"},
			map[string]any{"role": "user", "content": "please try again"}, // real user turn = boundary
			map[string]any{"role": "assistant", "content": "fresh model, first real step"},
		},
		"max_tokens": 256,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	require.Equal(t, []string{"fresh model, first real step"}, env.TrailingAssistantTexts(),
		"only narration since the last real user turn is in scope")
}

func TestTrailingAssistantTexts_PlainStringContent(t *testing.T) {
	body := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "assistant", "content": "a plain string reply"},
			map[string]any{"role": "assistant", "content": ""},
		},
		"max_tokens": 256,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	assert.Equal(t, []string{"a plain string reply"}, env.TrailingAssistantTexts(), "empty content is skipped")
}

func TestTrailingAssistantTexts_NonAnthropicReturnsNil(t *testing.T) {
	body := mustMarshalJSON(t, map[string]any{
		"model": "gpt-5.5",
		"messages": []any{
			map[string]any{"role": "assistant", "content": "hi"},
		},
	})
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)

	assert.Nil(t, env.TrailingAssistantTexts())
}
