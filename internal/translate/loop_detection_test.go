package translate_test

import (
	"encoding/json"
	"fmt"
	"strings"
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

func TestAssistantToolCallSignatures_IncludesRouterNudgeEntries(t *testing.T) {
	// Router-synthesized recovery nudges are now included in loop detection.
	// When a model repeatedly calls the same nudge (same name + args), it will
	// trip the loop detector after 5+ consecutive calls, breaking the stuck loop.
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
	require.Len(t, sigs, 3, "router nudge tool_use entries are now counted; 2 nudges + 1 Read")
	assert.Equal(t, "Bash", sigs[0].Name)
	assert.Equal(t, "Bash", sigs[1].Name)
	assert.Equal(t, "Read", sigs[2].Name)
	assert.Equal(t, sigs[0].InputHash, sigs[1].InputHash, "identical nudge args produce identical hash")
}

// TestAssistantToolCallArgsPreview_AlignsWithSignatures_Anthropic reproduces
// proxy.detectToolCallLoop's exact usage: window the trailing N entries of
// AssistantToolCallSignatures(), find the offending index within that window,
// then fetch AssistantToolCallArgsPreview(start, ...) — start expressed as an
// index into the SAME sequence AssistantToolCallSignatures walked — and check
// the preview entries line up name-for-name with sigs[start:]. The body
// interleaves empty-input tool_use blocks (filtered out of the signature
// sequence) among the real ones. Before the fix, ArgsPreview only skipped
// nameless blocks (not empty-input ones), so it walked a longer, differently
// filtered sequence: an offset valid against sigs pointed at the wrong
// entries in the preview.
func TestAssistantToolCallArgsPreview_AlignsWithSignatures_Anthropic(t *testing.T) {
	const windowSize = 10
	const maxRepeats = 5

	var content []any
	id := 0
	nextID := func() string {
		id++
		return fmt.Sprintf("%d", id)
	}
	// 3 "Setup" calls with distinct args, then 5 identical "Loop" calls — each
	// immediately followed by an empty-input tool_use, which Signatures
	// filters out but the pre-fix ArgsPreview did not.
	for i := 0; i < 3; i++ {
		content = append(content,
			map[string]any{"type": "tool_use", "id": nextID(), "name": "Setup", "input": map[string]any{"step": i}},
			map[string]any{"type": "tool_use", "id": nextID(), "name": "Read", "input": map[string]any{}},
		)
	}
	for i := 0; i < 5; i++ {
		content = append(content,
			map[string]any{"type": "tool_use", "id": nextID(), "name": "Loop", "input": map[string]any{"n": 1}},
			map[string]any{"type": "tool_use", "id": nextID(), "name": "Read", "input": map[string]any{}},
		)
	}
	body := mustMarshalJSON(t, map[string]any{
		"model":      "claude-sonnet-4-6",
		"messages":   []any{map[string]any{"role": "assistant", "content": content}},
		"max_tokens": 256,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	sigs := env.AssistantToolCallSignatures()
	require.Len(t, sigs, 8, "the 8 empty-input Read blocks must be filtered out of the signature sequence")

	// Mirror proxy.detectToolCallLoop's windowing exactly.
	start := 0
	if len(sigs) > windowSize {
		start = len(sigs) - windowSize
	}
	require.Equal(t, 0, start, "8 sigs fits within the 10-wide window, so detectToolCallLoop windows from the start")
	window := sigs[start:]
	counts := make(map[string]int, len(window))
	offendingIdx := -1
	for i, s := range window {
		key := s.Name + "\x00" + s.InputHash
		counts[key]++
		if counts[key] >= maxRepeats {
			offendingIdx = i
			break
		}
	}
	require.NotEqual(t, -1, offendingIdx, "the 5 identical Loop calls must trip the repeat threshold")
	require.Equal(t, "Loop", window[offendingIdx].Name)

	// detectToolCallLoop passes `start` (not offendingIdx) as the offset — it
	// wants the whole window dumped, not just the tail from the trip point.
	preview := env.AssistantToolCallArgsPreview(start, 200)
	require.Len(t, preview, len(sigs)-start,
		"preview window must contain exactly as many entries as the aligned signature window")
	for i, s := range sigs[start:] {
		require.True(t, strings.HasPrefix(preview[i], s.Name+":"),
			"preview[%d] = %q must describe the same tool call as sigs[%d] = %q (index alignment)", i, preview[i], start+i, s.Name)
	}
	// Pin down the exact entries the loop-detector log line cares about: 3
	// distinct Setup previews followed by 5 identical Loop previews, in
	// order — proving the empty-input Read blocks did not shift ArgsPreview's
	// output relative to Signatures'.
	require.Equal(t, []string{
		`Setup:{"step":0}`,
		`Setup:{"step":1}`,
		`Setup:{"step":2}`,
		`Loop:{"n":1}`,
		`Loop:{"n":1}`,
		`Loop:{"n":1}`,
		`Loop:{"n":1}`,
		`Loop:{"n":1}`,
	}, preview)
}

// mustMarshalJSON is shared with force_model_test.go; redeclared here would be
// a compile error so the existing one is reused.
var _ = json.Marshal

func TestAssistantToolCallSignatures_UserTextResetsLoop(t *testing.T) {
	// If the user explicitly sends a text message, it breaks the loop detector
	// by resetting the tool call history.
	body := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "user", "content": "do stuff"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "1", "name": "ls", "input": map[string]any{"path": "/tmp"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "1", "content": "a"},
				map[string]any{"type": "text", "text": "continue please"}, // This resets the loop
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "2", "name": "read", "input": map[string]any{"path": "/etc/hosts"}},
			}},
		},
		"max_tokens": 256,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	sigs := env.AssistantToolCallSignatures()
	require.Len(t, sigs, 1) // Only "read" is counted, because the "ls" was before the user text
	assert.Equal(t, "read", sigs[0].Name)
}

func TestAssistantToolCallSignatures_InjectedTextDoesNotResetLoop(t *testing.T) {
	// A normal tool round carries Claude Code's injected <system-reminder>
	// text block alongside the tool_result. That is NOT a user intervention and
	// must NOT reset the window — otherwise the loop detector could never reach
	// its repeat threshold. Build a 6-deep identical-call loop with an injected
	// reminder on every tool_result turn and assert all 6 sigs survive.
	msgs := []any{
		map[string]any{"role": "user", "content": "do stuff"},
	}
	for i := 0; i < 6; i++ {
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "t", "name": "ls", "input": map[string]any{"path": "/tmp"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t", "content": "a"},
				map[string]any{"type": "text", "text": "<system-reminder>be helpful</system-reminder>"},
			}},
		)
	}
	body := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6", "messages": msgs, "max_tokens": 256,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	sigs := env.AssistantToolCallSignatures()
	require.Len(t, sigs, 6, "injected reminder blocks must not reset the loop window")
}

func TestAssistantToolCallSignatures_UserTextResetsLoop_OpenAI(t *testing.T) {
	// OpenAI-format parallel of the Anthropic reset test: a genuine user text
	// message clears tool_calls accumulated before it.
	body := mustMarshalJSON(t, map[string]any{
		"model": "gpt-5.5",
		"messages": []any{
			map[string]any{"role": "user", "content": "do stuff"},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "1", "type": "function", "function": map[string]any{"name": "ls", "arguments": `{"path":"/tmp"}`}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "1", "content": "a"},
			map[string]any{"role": "user", "content": "continue please"}, // resets
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "2", "type": "function", "function": map[string]any{"name": "read", "arguments": `{"path":"/etc/hosts"}`}},
			}},
		},
	})
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)

	sigs := env.AssistantToolCallSignatures()
	require.Len(t, sigs, 1)
	assert.Equal(t, "read", sigs[0].Name)
}
