package proxy

import (
	"encoding/json"
	"testing"

	"workweave/router/internal/translate"
)

// A substantive narration line (>= textRepetitionMinLen) that a stuck model
// restates verbatim each turn while still issuing fresh tool calls.
const loopNarration = "I'll read the test file and then implement the fix."

func TestDetectTextRepetition_FiresOnRepeatedNarrationDespiteFreshToolCalls(t *testing.T) {
	// The loop shape that defeats every other detector: a NEW tool call every
	// turn (tool-call count grows, no-progress fingerprint advances) but the
	// same narration repeated. Three restatements trips the break.
	var turns []string
	for i := 0; i < 3; i++ {
		turns = append(turns,
			"text:"+loopNarration,
			`call:Read:{"file_path":"/src/thing.go"}`, // a tool call every turn: the detector must fire on the text, not tool-call stagnation
			"result:ok",
		)
	}
	env := parseSpiralEnv(t, turns)

	looped, count, sample := detectTextRepetition(env)
	if !looped {
		t.Fatalf("expected repetition loop to be detected; count=%d", count)
	}
	if count != 3 {
		t.Fatalf("expected recurrence count 3, got %d", count)
	}
	if sample == "" {
		t.Fatal("expected a non-empty text sample hash for logs")
	}
}

func TestDetectTextRepetition_IgnoresDistinctNarration(t *testing.T) {
	turns := []string{
		"text:First I will inspect the failing assertion in the parser suite.",
		`call:Read:{"file_path":"/src/a.go"}`, "result:ok",
		"text:Now I understand the bug; the offset is computed before the guard.",
		`call:Edit:{"file_path":"/src/a.go"}`, "result:ok",
		"text:Verifying the fix by re-running only the previously failing case.",
		`call:Bash:{"command":"go test ./..."}`, "result:ok",
	}
	env := parseSpiralEnv(t, turns)

	if looped, count, _ := detectTextRepetition(env); looped {
		t.Fatalf("healthy distinct narration must not trip the break; count=%d", count)
	}
}

func TestDetectTextRepetition_BelowThresholdDoesNotFire(t *testing.T) {
	// Two identical restatements is a retry, not a loop.
	turns := []string{
		"text:" + loopNarration, `call:Read:{"file_path":"/src/x.go"}`, "result:ok",
		"text:" + loopNarration, `call:Read:{"file_path":"/src/y.go"}`, "result:ok",
	}
	env := parseSpiralEnv(t, turns)

	if looped, count, _ := detectTextRepetition(env); looped {
		t.Fatalf("two restatements must not trip the break; count=%d", count)
	}
}

func TestDetectTextRepetition_FiresThroughSystemReminderToolResults(t *testing.T) {
	// Production shape (Redwood): Claude Code appends a <system-reminder> text
	// block on the same user turn as the tool_result every iteration. Those
	// turns must NOT count as human boundaries, or the backward scan collects
	// nothing on exactly the turns this detector guards. The loop must fire.
	var msgs []map[string]any
	for i := 0; i < 4; i++ {
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": loopNarration},
				{"type": "tool_use", "id": "t", "name": "Read", "input": map[string]any{"file_path": "/src/x.go"}},
			}},
			map[string]any{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "t", "content": "ok"},
				{"type": "text", "text": "<system-reminder>The task tools haven't been used recently.</system-reminder>"},
			}},
		)
	}
	body, err := json.Marshal(map[string]any{"model": "claude-sonnet-4-6", "messages": msgs, "max_tokens": 256})
	if err != nil {
		t.Fatal(err)
	}
	env, err := translate.ParseAnthropic(body)
	if err != nil {
		t.Fatal(err)
	}

	looped, count, _ := detectTextRepetition(env)
	if !looped {
		t.Fatalf("loop through reminder-bearing tool_result turns must fire; count=%d", count)
	}
}

func TestDetectTextRepetition_RealUserTurnResetsWindow(t *testing.T) {
	// After a break clears the pin, the stale loop narration is still in the
	// body. Once the user sends a real follow-up, that narration is before the
	// turn boundary and must not re-trip the break before the fresh model runs
	// (Cursor Bugbot #665). Three restatements, then a real user turn.
	turns := []string{
		"text:" + loopNarration, `call:Read:{"file_path":"/src/x.go"}`, "result:ok",
		"text:" + loopNarration, `call:Read:{"file_path":"/src/y.go"}`, "result:ok",
		"text:" + loopNarration, `call:Read:{"file_path":"/src/z.go"}`, "result:ok",
		"user:please try a different approach",
	}
	env := parseSpiralEnv(t, turns)

	if looped, count, _ := detectTextRepetition(env); looped {
		t.Fatalf("a real user turn must reset the repetition window; count=%d", count)
	}
}

func TestDetectTextRepetition_IgnoresShortLines(t *testing.T) {
	// "Done." style acknowledgements repeat benignly and must not count.
	turns := []string{
		"text:Done.", `call:Read:{"file_path":"/src/x.go"}`, "result:ok",
		"text:Done.", `call:Read:{"file_path":"/src/y.go"}`, "result:ok",
		"text:Done.", `call:Read:{"file_path":"/src/z.go"}`, "result:ok",
	}
	env := parseSpiralEnv(t, turns)

	if looped, count, _ := detectTextRepetition(env); looped {
		t.Fatalf("short repeated lines must not trip the break; count=%d", count)
	}
}

func TestDetectTextRepetition_NormalizesCaseAndWhitespace(t *testing.T) {
	// Cosmetic drift (case, re-wrapping) between otherwise-identical narration
	// must still be counted as the same line.
	turns := []string{
		"text:" + loopNarration,
		"text:i'll read the test file    and then implement the fix.",
		"text:I'LL READ THE TEST FILE AND THEN IMPLEMENT THE FIX.",
	}
	env := parseSpiralEnv(t, turns)

	looped, count, _ := detectTextRepetition(env)
	if !looped || count != 3 {
		t.Fatalf("case/whitespace-variant restatements must all count; looped=%v count=%d", looped, count)
	}
}
