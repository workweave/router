package proxy

import (
	"encoding/json"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetectToolCallLoop_TripsAtMaxRepeats(t *testing.T) {
	// 5 identical (ls, /tmp) tool calls in a row → trip on the 5th.
	body := buildBodyWithToolCalls(t, []toolCall{
		{name: "ls", input: map[string]any{"path": "/tmp"}},
		{name: "ls", input: map[string]any{"path": "/tmp"}},
		{name: "ls", input: map[string]any{"path": "/tmp"}},
		{name: "ls", input: map[string]any{"path": "/tmp"}},
		{name: "ls", input: map[string]any{"path": "/tmp"}},
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	loop, sig, count := detectToolCallLoop(env)
	assert.True(t, loop)
	assert.Equal(t, "ls", sig.Name)
	assert.GreaterOrEqual(t, count, loopDetectionMaxRepeats)
}

func TestDetectToolCallLoop_NoLoopBelowThreshold(t *testing.T) {
	body := buildBodyWithToolCalls(t, []toolCall{
		{name: "ls", input: map[string]any{"path": "/tmp"}},
		{name: "ls", input: map[string]any{"path": "/tmp"}},
		{name: "ls", input: map[string]any{"path": "/tmp"}},
		{name: "ls", input: map[string]any{"path": "/tmp"}},
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	loop, _, _ := detectToolCallLoop(env)
	assert.False(t, loop, "4 identical calls must not trip the detector (threshold is 5)")
}

func TestDetectToolCallLoop_DifferentArgsDoNotTrip(t *testing.T) {
	body := buildBodyWithToolCalls(t, []toolCall{
		{name: "ls", input: map[string]any{"path": "/a"}},
		{name: "ls", input: map[string]any{"path": "/b"}},
		{name: "ls", input: map[string]any{"path": "/c"}},
		{name: "ls", input: map[string]any{"path": "/d"}},
		{name: "ls", input: map[string]any{"path": "/e"}},
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	loop, _, _ := detectToolCallLoop(env)
	assert.False(t, loop, "same tool name but distinct args must not trip the detector")
}

func TestDetectToolCallLoop_WindowedOldEntriesDropOut(t *testing.T) {
	// Window is 10. Put 4 (ls,/tmp) entries spaced far apart (separated by
	// many distinct calls). The window should be small enough that the
	// (ls,/tmp) count drops below threshold by the time we sample it.
	calls := []toolCall{
		{name: "ls", input: map[string]any{"path": "/tmp"}},
		{name: "ls", input: map[string]any{"path": "/tmp"}},
	}
	for i := range 15 {
		calls = append(calls, toolCall{name: "read", input: map[string]any{"path": "/etc", "n": i}})
	}
	body := buildBodyWithToolCalls(t, calls)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	loop, _, _ := detectToolCallLoop(env)
	assert.False(t, loop, "stale repeats outside the window must not trip the detector")
}

func TestDetectToolCallLoop_AlternatingPairStillTripsOnRepeats(t *testing.T) {
	// An A/B alternating loop (Hermes-style qwen3 failure mode) still trips
	// because each leg accrues count independently.
	calls := []toolCall{
		{name: "ls", input: map[string]any{"path": "/tmp"}},
		{name: "read", input: map[string]any{"path": "/etc/hosts"}},
		{name: "ls", input: map[string]any{"path": "/tmp"}},
		{name: "read", input: map[string]any{"path": "/etc/hosts"}},
		{name: "ls", input: map[string]any{"path": "/tmp"}},
		{name: "read", input: map[string]any{"path": "/etc/hosts"}},
		{name: "ls", input: map[string]any{"path": "/tmp"}},
		{name: "read", input: map[string]any{"path": "/etc/hosts"}},
		{name: "ls", input: map[string]any{"path": "/tmp"}},
	}
	body := buildBodyWithToolCalls(t, calls)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	loop, sig, count := detectToolCallLoop(env)
	assert.True(t, loop)
	assert.Equal(t, "ls", sig.Name)
	assert.GreaterOrEqual(t, count, loopDetectionMaxRepeats)
}

// --- helpers ---

type toolCall struct {
	name  string
	input map[string]any
}

func buildBodyWithToolCalls(t *testing.T, calls []toolCall) []byte {
	t.Helper()
	msgs := []any{
		map[string]any{"role": "user", "content": "do the thing"},
	}
	for i, c := range calls {
		id := "toolu_" + itoa(i)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": id, "name": c.name, "input": c.input},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": id, "content": "result"},
			}},
		)
	}
	body, err := json.Marshal(map[string]any{
		"model":      "claude-sonnet-4-6",
		"max_tokens": 256,
		"messages":   msgs,
	})
	require.NoError(t, err)
	return body
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string('0'+byte(n%10)) + digits
		n /= 10
	}
	return digits
}
