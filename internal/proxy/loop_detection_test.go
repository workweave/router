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

// ---- cyclic (wide re-read) loop detector ----

func cyclicReads(nFiles, total int) []toolCall {
	calls := make([]toolCall, 0, total)
	for i := 0; i < total; i++ {
		calls = append(calls, toolCall{name: "Read", input: map[string]any{"file_path": "/app/f" + itoa(i%nFiles) + ".go"}})
	}
	return calls
}

func TestDetectCyclicToolCallLoop_TripsOnLowDiversityCycle(t *testing.T) {
	// 30 Reads cycling over 5 files (each 6x) → distinct ratio 5/30 ≈ 0.17 < 0.4.
	env, err := translate.ParseAnthropic(buildBodyWithToolCalls(t, cyclicReads(5, 30)))
	require.NoError(t, err)
	looped, top, count, ratio, total := detectCyclicToolCallLoop(env)
	assert.True(t, looped, "low-diversity re-read cycle must trip")
	assert.Equal(t, "Read", top.Name)
	assert.GreaterOrEqual(t, count, 2)
	assert.Less(t, ratio, cyclicLoopMaxDistinctRatio)
	assert.Equal(t, cyclicLoopWindowSize, total)
}

func TestDetectCyclicToolCallLoop_BroadDistinctReadsDoNotTrip(t *testing.T) {
	// A healthy Explore reads MANY DISTINCT files → high diversity → no trip.
	env, err := translate.ParseAnthropic(buildBodyWithToolCalls(t, cyclicReads(30, 30)))
	require.NoError(t, err)
	looped, _, _, ratio, _ := detectCyclicToolCallLoop(env)
	assert.False(t, looped, "broad distinct exploration must not trip (the #271 guard)")
	assert.GreaterOrEqual(t, ratio, cyclicLoopMaxDistinctRatio)
}

func TestDetectCyclicToolCallLoop_EditInWindowIsProgress(t *testing.T) {
	// Same low-diversity cycle but with a real Edit in the window → progress, no trip.
	calls := cyclicReads(5, 29)
	calls = append(calls, toolCall{name: "Edit", input: map[string]any{"file_path": "/app/f0.go", "old_string": "a", "new_string": "b"}})
	env, err := translate.ParseAnthropic(buildBodyWithToolCalls(t, calls))
	require.NoError(t, err)
	looped, _, _, _, _ := detectCyclicToolCallLoop(env)
	assert.False(t, looped, "an edit in the window means the agent is progressing, not stuck")
}

func TestDetectCyclicToolCallLoop_BelowMinCallsDoesNotTrip(t *testing.T) {
	// Fewer than cyclicLoopMinCalls tool calls → too early to call it a loop.
	env, err := translate.ParseAnthropic(buildBodyWithToolCalls(t, cyclicReads(3, 20)))
	require.NoError(t, err)
	looped, _, _, _, _ := detectCyclicToolCallLoop(env)
	assert.False(t, looped, "below the min-calls floor must not trip")
}
