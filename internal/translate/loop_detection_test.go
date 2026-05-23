package translate_test

import (
	"encoding/json"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssistantToolCallSignatures_Anthropic(t *testing.T) {
	body := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "user", "content": "do stuff"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "1", "name": "ls", "input": map[string]any{"path": "/tmp"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "1", "content": "a"},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "2", "name": "ls", "input": map[string]any{"path": "/tmp"}},
				map[string]any{"type": "text", "text": "ignore me"},
				map[string]any{"type": "tool_use", "id": "3", "name": "read", "input": map[string]any{"path": "/etc/hosts"}},
			}},
		},
		"max_tokens": 256,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	sigs := env.AssistantToolCallSignatures()
	require.Len(t, sigs, 3)
	assert.Equal(t, "ls", sigs[0].Name)
	assert.Equal(t, "ls", sigs[1].Name)
	assert.Equal(t, "read", sigs[2].Name)
	assert.Equal(t, sigs[0].InputHash, sigs[1].InputHash, "identical args must produce identical hash")
	assert.NotEqual(t, sigs[1].InputHash, sigs[2].InputHash, "different tool args must produce different hash")
}

func TestAssistantToolCallSignatures_Anthropic_KeyOrderInvariant(t *testing.T) {
	bodyA := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "1", "name": "x", "input": map[string]any{"a": 1, "b": 2}},
			}},
		},
		"max_tokens": 256,
	})
	// Build the alternate form with the same keys in opposite order. We
	// can't rely on json.Marshal ordering, so write the raw JSON directly.
	bodyB := []byte(`{"model":"claude-sonnet-4-6","max_tokens":256,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"1","name":"x","input":{"b":2,"a":1}}]}]}`)

	envA, err := translate.ParseAnthropic(bodyA)
	require.NoError(t, err)
	envB, err := translate.ParseAnthropic(bodyB)
	require.NoError(t, err)

	sigsA := envA.AssistantToolCallSignatures()
	sigsB := envB.AssistantToolCallSignatures()
	require.Len(t, sigsA, 1)
	require.Len(t, sigsB, 1)
	assert.Equal(t, sigsA[0].InputHash, sigsB[0].InputHash, "canonical hashing must be key-order-invariant")
}

func TestAssistantToolCallSignatures_OpenAI(t *testing.T) {
	body := mustMarshalJSON(t, map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{"role": "user", "content": "do stuff"},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{
					"id":   "call_1",
					"type": "function",
					"function": map[string]any{
						"name":      "ls",
						"arguments": `{"path":"/tmp"}`,
					},
				},
				map[string]any{
					"id":   "call_2",
					"type": "function",
					"function": map[string]any{
						"name":      "ls",
						"arguments": `{"path": "/tmp"}`, // whitespace differs
					},
				},
			}},
		},
	})
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)

	sigs := env.AssistantToolCallSignatures()
	require.Len(t, sigs, 2)
	assert.Equal(t, sigs[0].InputHash, sigs[1].InputHash, "whitespace-only differences must not affect the hash")
}

func TestAssistantToolCallSignatures_EmptyAndNonAssistant(t *testing.T) {
	body := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "x", "content": "noise"},
			}},
		},
		"max_tokens": 256,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	assert.Nil(t, env.AssistantToolCallSignatures())
}

// mustMarshalJSON is shared with force_model_test.go; redeclared here would be
// a compile error so the existing one is reused.
var _ = json.Marshal
