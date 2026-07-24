//go:build smoke

package smoke

import (
	"net/http"
	"testing"
)

// TestStreaming asserts the SSE lifecycle on a multi-block tool-use turn:
// balanced content_block_start/stop pairs and a single terminal message_stop.
func TestStreaming(t *testing.T) {
	body := newRequest("smoke-stream-lifecycle").tokens(256).streaming().
		text("Use the Bash tool to list files in the current directory. Call the tool; do not answer in prose.").
		build(t)
	r := call(t, body)
	if r.status != http.StatusOK {
		t.Fatalf("stream: want 200, got %d; body: %s", r.status, truncate(r.body, 400))
	}
	assertStreamWellFormed(t, r)

	if r.message == nil {
		t.Fatalf("no reconstructed message from stream")
	}
	if r.message.StopReason == "" {
		t.Errorf("want a stop_reason in message_delta, got none")
	}
}

// assertStreamWellFormed checks Anthropic SSE invariants: message_start first,
// exactly one message_stop last, balanced content_block_start/stop pairs, and at
// least one message_delta. A truncated or re-ordered stream trips one of these.
func assertStreamWellFormed(t *testing.T, r response) {
	t.Helper()
	ev := r.streamEvents
	if len(ev) == 0 {
		t.Fatalf("no SSE events parsed; body: %s", truncate(r.body, 400))
	}
	if ev[0] != "message_start" {
		t.Errorf("want first event message_start, got %q", ev[0])
	}
	if last := ev[len(ev)-1]; last != "message_stop" {
		t.Errorf("want last event message_stop, got %q", last)
	}

	var starts, stops, msgStops, msgDeltas int
	for _, e := range ev {
		switch e {
		case "content_block_start":
			starts++
		case "content_block_stop":
			stops++
		case "message_stop":
			msgStops++
		case "message_delta":
			msgDeltas++
		}
	}
	if starts != stops {
		t.Errorf("unbalanced content blocks: %d start vs %d stop", starts, stops)
	}
	if starts == 0 {
		t.Errorf("want at least one content_block_start")
	}
	if msgStops != 1 {
		t.Errorf("want exactly one message_stop, got %d", msgStops)
	}
	if msgDeltas == 0 {
		t.Errorf("want at least one message_delta (carries stop_reason)")
	}
}
