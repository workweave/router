package translate_test

import (
	"net/http/httptest"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the OUTPUT-progress classification the output-stall watchdog
// depends on (see httputil.DefaultResponsesOutputStallTimeout). The translator
// is the only layer that can tell an output-bearing Responses frame from a
// reasoning/keepalive frame, so it owns the mark: reasoning deltas and reasoning
// items must NOT count as progress (else the 2026-06-16 reasoning-only stall
// would keep resetting the watchdog forever), while text, tool-call args, and
// the terminal envelope must.

// SSE events lifted from responsesStreamFixture, one per const so a test can
// feed them individually and assert the mark fires (or not) per event.
const (
	evReasoningItemAdded = `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"enc","summary":[],"status":"in_progress"}}

`
	evReasoningDelta = `event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"thinking"}

`
	evReasoningItemDone = `event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"enc","summary":[{"type":"summary_text","text":"thinking"}],"status":"completed"}}

`
	evMessageItemAdded = `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","status":"in_progress","content":[]}}

`
	evTextDelta = `event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"content_index":0,"delta":"hi"}

`
	evToolArgsDelta = `event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":2,"delta":"{\"q\":1}"}

`
	evCompleted = `event: response.completed
data: {"type":"response.completed","response":{"id":"resp_abc","status":"completed","model":"gpt-5.5","incomplete_details":null,"output":[],"usage":{"input_tokens":1,"output_tokens":1}}}

`
)

func newStreamingWriter(t *testing.T) (*translate.ResponsesToAnthropicWriter, *int) {
	t.Helper()
	w := translate.NewResponsesToAnthropicWriter(httptest.NewRecorder(), "gpt-5.5", nil)
	require.NoError(t, w.Prelude(true))
	count := 0
	require.True(t, w.ArmOutputProgress(func() { count++ }),
		"ArmOutputProgress must report armed for a streaming client")
	return w, &count
}

func TestResponsesOutputProgress_ReasoningDoesNotCount(t *testing.T) {
	w, count := newStreamingWriter(t)

	// A reasoning item + its deltas + its done event: the entire thinking phase
	// must leave the output-progress mark untouched.
	_, err := w.Write([]byte(evReasoningItemAdded + evReasoningDelta + evReasoningDelta + evReasoningItemDone))
	require.NoError(t, err)
	assert.Zero(t, *count, "reasoning frames must not register as output progress")
}

func TestResponsesOutputProgress_OutputEventsCount(t *testing.T) {
	tests := []struct {
		name  string
		event string
	}{
		{"message item added", evMessageItemAdded},
		{"text delta", evTextDelta},
		{"tool args delta", evToolArgsDelta},
		{"terminal completed", evCompleted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, count := newStreamingWriter(t)
			_, err := w.Write([]byte(tc.event))
			require.NoError(t, err)
			assert.Positive(t, *count, "%s must register as output progress", tc.name)
		})
	}
}

func TestResponsesOutputProgress_NotArmedWhenNotStreaming(t *testing.T) {
	// The buffered (non-streaming) path parses events only at Finalize, so it has
	// nothing to mark mid-stream; arming there would guarantee a false trip.
	w := translate.NewResponsesToAnthropicWriter(httptest.NewRecorder(), "gpt-5.5", nil)
	require.NoError(t, w.Prelude(false))
	assert.False(t, w.ArmOutputProgress(func() {}),
		"ArmOutputProgress must report not-armed for a non-streaming client")
}
