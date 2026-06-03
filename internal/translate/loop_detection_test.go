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

func TestAssistantToolCallSignatures_SkipsEmptyNameEntries(t *testing.T) {
	// Some upstreams emit tool_use blocks without a `name` field (mid-stream
	// snapshots, cross-format translation artifacts). Counting them collapses
	// every nameless entry to the same map key and false-positive trips the
	// loop detector after 5 distinct real tool calls. They must be ignored.
	body := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "1", "name": "Read", "input": map[string]any{"path": "a"}},
				map[string]any{"type": "tool_use", "id": "2", "input": map[string]any{}},
				map[string]any{"type": "tool_use", "id": "3", "name": "", "input": map[string]any{}},
				map[string]any{"type": "tool_use", "id": "4", "name": "Bash", "input": map[string]any{"command": "ls"}},
			}},
		},
		"max_tokens": 256,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	sigs := env.AssistantToolCallSignatures()
	require.Len(t, sigs, 2, "nameless tool_use entries must be filtered")
	assert.Equal(t, "Read", sigs[0].Name)
	assert.Equal(t, "Bash", sigs[1].Name)
}

func TestAssistantToolCallSignatures_SkipsEmptyInputEntries(t *testing.T) {
	// Cross-format translation of stream-incomplete tool calls (deepinfra /
	// openaicompat upstream → Anthropic inbound) emits `input:{}` to satisfy
	// the schema. Claude Code echoes those back in the assistant history.
	// Without filtering, 5 of them all hash to the same empty-canonical key
	// and false-positive trip the loop detector even when the surrounding
	// real Read calls each have distinct file paths. This is the bug
	// observed against xiaomi/mimo-v2.5 on deepinfra in dev.
	body := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "1", "name": "Read", "input": map[string]any{"file_path": "/a"}},
				map[string]any{"type": "tool_use", "id": "2", "name": "Read", "input": map[string]any{}},
				map[string]any{"type": "tool_use", "id": "3", "name": "Read", "input": map[string]any{"file_path": "/b"}},
				map[string]any{"type": "tool_use", "id": "4", "name": "Read", "input": map[string]any{}},
				map[string]any{"type": "tool_use", "id": "5", "name": "Read", "input": map[string]any{"file_path": "/c"}},
				map[string]any{"type": "tool_use", "id": "6", "name": "Read", "input": map[string]any{}},
				map[string]any{"type": "tool_use", "id": "7", "name": "Read", "input": map[string]any{}},
			}},
		},
		"max_tokens": 256,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	sigs := env.AssistantToolCallSignatures()
	require.Len(t, sigs, 3, "empty-input tool_use entries must be filtered")
	// The three real Reads have distinct file_paths, so their hashes differ.
	assert.NotEqual(t, sigs[0].InputHash, sigs[1].InputHash)
	assert.NotEqual(t, sigs[1].InputHash, sigs[2].InputHash)
}

func TestAssistantToolCallSignatures_OpenAI_SkipsEmptyNameEntries(t *testing.T) {
	body := mustMarshalJSON(t, map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "1", "type": "function", "function": map[string]any{"name": "ls", "arguments": `{"path":"/a"}`}},
				map[string]any{"id": "2", "type": "function", "function": map[string]any{"arguments": `{"path":"/b"}`}},             // missing name
				map[string]any{"id": "3", "type": "function", "function": map[string]any{"name": "", "arguments": `{"path":"/c"}`}}, // empty name
				map[string]any{"id": "4", "type": "function", "function": map[string]any{"name": "ls", "arguments": "{}"}},          // empty args
				map[string]any{"id": "5", "type": "function", "function": map[string]any{"name": "cat", "arguments": `{"file":"/d"}`}},
			}},
		},
	})
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)

	sigs := env.AssistantToolCallSignatures()
	require.Len(t, sigs, 2, "missing-name, empty-name, and empty-args entries all filtered")
	assert.Equal(t, "ls", sigs[0].Name)
	assert.Equal(t, "cat", sigs[1].Name)
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

func TestAssistantToolCallSignatures_SkipsRouterNudgeEntries(t *testing.T) {
	// Router-synthesized recovery nudges have a constant command string so their
	// InputHash never changes. If a model emits consecutive text-only turns the
	// router injects a nudge each time; once 5 accumulate in the history both
	// detectToolCallLoop and toolProgressMarker see 5 identical Bash sigs and
	// fire — killing the session the nudge was trying to rescue. Nudges must be
	// invisible to the detectors.
	body := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_router_nudge_msg1", "name": "Bash", "input": map[string]any{
					"command":     "echo '[router] previous turn produced no tool_use; please use Edit/Write/Read/Bash/Grep — do not respond with prose or <think> tags only.'",
					"description": "router recovery nudge: previous turn had no tool_use",
				}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_router_nudge_msg1", "content": ""},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_router_nudge_msg2", "name": "Bash", "input": map[string]any{
					"command":     "echo '[router] previous turn produced no tool_use; please use Edit/Write/Read/Bash/Grep — do not respond with prose or <think> tags only.'",
					"description": "router recovery nudge: previous turn had no tool_use",
				}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_router_nudge_msg2", "content": ""},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_real_1", "name": "Read", "input": map[string]any{"file_path": "/foo.go"}},
			}},
		},
		"max_tokens": 256,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	sigs := env.AssistantToolCallSignatures()
	require.Len(t, sigs, 1, "router nudge tool_use entries must be filtered; only the real Read should appear")
	assert.Equal(t, "Read", sigs[0].Name)
}

// mustMarshalJSON is shared with force_model_test.go; redeclared here would be
// a compile error so the existing one is reused.
var _ = json.Marshal
