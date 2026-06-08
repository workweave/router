package translate_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// buildAnthropicSSE builds a single Anthropic-format SSE event string.
func buildAnthropicSSE(eventType, data string) string {
	return "event: " + eventType + "\ndata: " + data + "\n\n"
}

func TestAnthropicRoutingMarkerWriter_StreamingInjectsMarker(t *testing.T) {
	rec := httptest.NewRecorder()
	markerText := "✦ **Weave Router** → claude-opus-4 · best pick for this turn"
	w := translate.NewAnthropicRoutingMarkerWriter(rec, "claude-opus-4", markerText)

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	// Upstream Anthropic SSE: a single tool_use block at index 1.
	// (A real upstream stream; index 0 would be a text block but we use
	// index 1 to verify the shift clearly.)
	upstreamData :=
		// message_start — should be suppressed (prelude already sent one).
		buildAnthropicSSE("message_start", `{"type":"message_start","message":{"id":"msg_upstream","type":"message","role":"assistant","content":[],"model":"claude-opus-4","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}`) +
			// content_block_start at index 1 (tool_use) → shifted to index 2.
			buildAnthropicSSE("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_abc123","name":"bash","input":{}}}`) +
			// content_block_delta at index 1 → shifted to index 2.
			buildAnthropicSSE("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\": \"ls\"}"}}`) +
			// content_block_stop at index 1 → shifted to index 2.
			buildAnthropicSSE("content_block_stop", `{"type":"content_block_stop","index":1}`) +
			// message_delta — pass-through.
			buildAnthropicSSE("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":20}}`) +
			// message_stop — pass-through.
			buildAnthropicSSE("message_stop", `{"type":"message_stop"}`)

	_, err := w.Write([]byte(upstreamData))
	require.NoError(t, err)

	body := rec.Body.String()
	events := splitSSEEvents(body)

	// Prelude: message_start + content_block_start(0) + content_block_delta(0) + content_block_stop(0) = 4
	// Upstream: 6 events total, but message_start is dropped → 5 pass through
	// Total: 4 + 5 = 9
	require.Len(t, events, 9, "expected prelude (4) + upstream (5 after dropping message_start)")

	// --- Prelude events at the beginning ---

	// Event 0: prelude message_start
	assert.Contains(t, events[0], "event: message_start")
	assert.Contains(t, events[0], `"type":"message_start"`)
	msgData := extractDataField(events[0])
	assert.Equal(t, "message_start", gjson.Get(msgData, "type").String())
	assert.Equal(t, "claude-opus-4", gjson.Get(msgData, "message.model").String())
	assert.True(t, gjson.Get(msgData, "message.id").String() != "", "prelude message_start should have a non-empty id")

	// Event 1: prelude content_block_start at index 0 (text)
	startData := extractDataField(events[1])
	assert.Equal(t, "content_block_start", gjson.Get(startData, "type").String())
	assert.EqualValues(t, 0, gjson.Get(startData, "index").Int())
	assert.Equal(t, "text", gjson.Get(startData, "content_block.type").String())

	// Event 2: prelude content_block_delta at index 0 (text_delta with marker)
	deltaData := extractDataField(events[2])
	assert.Equal(t, "content_block_delta", gjson.Get(deltaData, "type").String())
	assert.EqualValues(t, 0, gjson.Get(deltaData, "index").Int())
	assert.Equal(t, "text_delta", gjson.Get(deltaData, "delta.type").String())
	assert.Contains(t, gjson.Get(deltaData, "delta.text").String(), "Weave Router")
	assert.Contains(t, gjson.Get(deltaData, "delta.text").String(), "claude-opus-4")

	// Event 3: prelude content_block_stop at index 0
	stopData := extractDataField(events[3])
	assert.Equal(t, "content_block_stop", gjson.Get(stopData, "type").String())
	assert.EqualValues(t, 0, gjson.Get(stopData, "index").Int())

	// --- Upstream events (shifted) ---

	// Event 4: upstream content_block_start originally at index 1 → now index 2
	toolStartData := extractDataField(events[4])
	assert.Equal(t, "content_block_start", gjson.Get(toolStartData, "type").String())
	assert.EqualValues(t, 2, gjson.Get(toolStartData, "index").Int(), "tool_use index should shift from 1 to 2")
	assert.Equal(t, "tool_use", gjson.Get(toolStartData, "content_block.type").String())

	// Event 5: upstream content_block_delta originally at index 1 → now index 2
	inputDeltaData := extractDataField(events[5])
	assert.Equal(t, "content_block_delta", gjson.Get(inputDeltaData, "type").String())
	assert.EqualValues(t, 2, gjson.Get(inputDeltaData, "index").Int(), "input_json_delta index should shift from 1 to 2")
	assert.Equal(t, "input_json_delta", gjson.Get(inputDeltaData, "delta.type").String())

	// Event 6: upstream content_block_stop originally at index 1 → now index 2
	toolStopData := extractDataField(events[6])
	assert.Equal(t, "content_block_stop", gjson.Get(toolStopData, "type").String())
	assert.EqualValues(t, 2, gjson.Get(toolStopData, "index").Int(), "content_block_stop index should shift from 1 to 2")

	// Event 7: message_delta — pass-through untouched
	msgDeltaData := extractDataField(events[7])
	assert.Equal(t, "message_delta", gjson.Get(msgDeltaData, "type").String())
	assert.Equal(t, "tool_use", gjson.Get(msgDeltaData, "delta.stop_reason").String())

	// Event 8: message_stop — pass-through untouched
	msgStopData := extractDataField(events[8])
	assert.Equal(t, "message_stop", gjson.Get(msgStopData, "type").String())

	// Assert that upstream message_start does NOT appear
	for _, e := range events {
		data := extractDataField(e)
		if gjson.Get(data, "type").String() == "message_start" {
			// The prelude's message_start is the only one allowed.
			// Upstream message_start has message.id = "msg_upstream"; prelude has a different id.
			id := gjson.Get(data, "message.id").String()
			assert.NotEqual(t, "msg_upstream", id, "upstream message_start must be suppressed")
		}
	}
}

func TestAnthropicRoutingMarkerWriter_ThinkingFidelity(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewAnthropicRoutingMarkerWriter(rec, "claude-opus-4", "✦ marker")

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	// Upstream response with a thinking block at index 0 and text at index 1.
	// After marker injection, thinking shifts from index 0 → 1, text from 1 → 2.
	upstreamData :=
		buildAnthropicSSE("message_start", `{"type":"message_start","message":{"id":"msg_upstream","type":"message","role":"assistant","content":[],"model":"claude-opus-4","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":0}}}`) +
			// content_block_start at index 0 (thinking)
			buildAnthropicSSE("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`) +
			// content_block_delta at index 0 (thinking_delta)
			buildAnthropicSSE("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me think about this step by step..."}}`) +
			// signature_delta at index 0
			buildAnthropicSSE("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"EpQCGx8SBAgEIAcY1QYo8QUqCQIFBSAHGAEgADoUChIJ"}}`) +
			// content_block_stop at index 0
			buildAnthropicSSE("content_block_stop", `{"type":"content_block_stop","index":0}`) +
			// content_block_start at index 1 (text)
			buildAnthropicSSE("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`) +
			// content_block_delta at index 1 (text_delta)
			buildAnthropicSSE("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"The answer is 42."}}`) +
			// content_block_stop at index 1
			buildAnthropicSSE("content_block_stop", `{"type":"content_block_stop","index":1}`) +
			// message_delta
			buildAnthropicSSE("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":50}}`) +
			// message_stop
			buildAnthropicSSE("message_stop", `{"type":"message_stop"}`)

	_, err := w.Write([]byte(upstreamData))
	require.NoError(t, err)

	body := rec.Body.String()
	events := splitSSEEvents(body)

	// Prelude: 4 events + 10 upstream events (1 message_start dropped → 9) = 13
	require.Len(t, events, 13, "expected prelude (4) + upstream (9 after dropping message_start)")

	// Verify that upstream thinking content_block_start at original index 0 is now at index 1
	// Event 4 = upstream content_block_start (thinking) → shifted from index 0 to index 1
	thinkingStart := extractDataField(events[4])
	assert.Equal(t, "content_block_start", gjson.Get(thinkingStart, "type").String())
	assert.EqualValues(t, 1, gjson.Get(thinkingStart, "index").Int(), "thinking content_block_start should shift from 0 to 1")
	assert.Equal(t, "thinking", gjson.Get(thinkingStart, "content_block.type").String())

	// Event 5 = upstream content_block_delta (thinking_delta) → shifted from index 0 to index 1
	thinkingDelta := extractDataField(events[5])
	assert.Equal(t, "content_block_delta", gjson.Get(thinkingDelta, "type").String())
	assert.EqualValues(t, 1, gjson.Get(thinkingDelta, "index").Int(), "thinking_delta index should shift from 0 to 1")
	assert.Equal(t, "thinking_delta", gjson.Get(thinkingDelta, "delta.type").String())
	assert.Contains(t, gjson.Get(thinkingDelta, "delta.thinking").String(), "step by step")

	// Event 6 = signature_delta → shifted from index 0 to index 1
	sigDelta := extractDataField(events[6])
	assert.Equal(t, "content_block_delta", gjson.Get(sigDelta, "type").String())
	assert.EqualValues(t, 1, gjson.Get(sigDelta, "index").Int(), "signature_delta index should shift from 0 to 1")
	assert.Equal(t, "signature_delta", gjson.Get(sigDelta, "delta.type").String())
	assert.NotEmpty(t, gjson.Get(sigDelta, "delta.signature").String(), "signature should pass through intact")

	// Event 7 = upstream content_block_stop at original index 0 → shifted to index 1
	thinkingStop := extractDataField(events[7])
	assert.Equal(t, "content_block_stop", gjson.Get(thinkingStop, "type").String())
	assert.EqualValues(t, 1, gjson.Get(thinkingStop, "index").Int(), "thinking content_block_stop should shift from 0 to 1")

	// Event 8 = upstream content_block_start (text) at original index 1 → shifted to index 2
	textStart := extractDataField(events[8])
	assert.Equal(t, "content_block_start", gjson.Get(textStart, "type").String())
	assert.EqualValues(t, 2, gjson.Get(textStart, "index").Int(), "text content_block_start should shift from 1 to 2")

	// Event 9 = upstream content_block_delta (text_delta) at original index 1 → shifted to index 2
	textDelta := extractDataField(events[9])
	assert.Equal(t, "content_block_delta", gjson.Get(textDelta, "type").String())
	assert.EqualValues(t, 2, gjson.Get(textDelta, "index").Int(), "text_delta index should shift from 1 to 2")
	assert.Equal(t, "text_delta", gjson.Get(textDelta, "delta.type").String())
	assert.Contains(t, gjson.Get(textDelta, "delta.text").String(), "42")
}

func TestAnthropicRoutingMarkerWriter_EmptyMarkerNoInjection(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewAnthropicRoutingMarkerWriter(rec, "claude-opus-4", "")

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	upstreamData := buildAnthropicSSE("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-opus-4","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}`)
	_, err := w.Write([]byte(upstreamData))
	require.NoError(t, err)

	// Empty marker: fully transparent passthrough. Byte-identical.
	assert.Equal(t, upstreamData, rec.Body.String(), "empty marker must produce byte-identical passthrough")
}

func TestAnthropicRoutingMarkerWriter_NonStreamingPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewAnthropicRoutingMarkerWriter(rec, "claude-opus-4", "✦ marker")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	body := `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"Hello"}],"model":"claude-opus-4"}`
	_, err := w.Write([]byte(body))
	require.NoError(t, err)

	assert.Equal(t, body, rec.Body.String(), "non-streaming responses should pass through unmodified")
}

func TestAnthropicRoutingMarkerWriter_ErrorResponsePassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewAnthropicRoutingMarkerWriter(rec, "claude-opus-4", "✦ marker")

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusTooManyRequests)

	errBody := `{"error":{"message":"rate limited"}}`
	_, err := w.Write([]byte(errBody))
	require.NoError(t, err)

	assert.Equal(t, errBody, rec.Body.String(), "error responses should pass through unmodified")
}

func TestAnthropicRoutingMarkerWriter_UpstreamMessageStartSuppressed(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewAnthropicRoutingMarkerWriter(rec, "claude-opus-4", "✦ marker")

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	// Upstream sends message_start followed by message_stop.
	upstreamData :=
		buildAnthropicSSE("message_start", `{"type":"message_start","message":{"id":"msg_upstream","type":"message","role":"assistant","content":[],"model":"claude-opus-4","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}`) +
			buildAnthropicSSE("message_stop", `{"type":"message_stop"}`)

	_, err := w.Write([]byte(upstreamData))
	require.NoError(t, err)

	body := rec.Body.String()
	events := splitSSEEvents(body)

	// Prelude (4) + message_stop (1) = 5 events. Upstream message_start must be suppressed.
	require.Len(t, events, 5, "expected prelude (4) + message_stop (1) — upstream message_start must be suppressed")

	// First event: prelude message_start (from marker writer)
	firstData := extractDataField(events[0])
	assert.Equal(t, "message_start", gjson.Get(firstData, "type").String())

	// The last event should be the upstream message_stop (unchanged).
	lastData := extractDataField(events[4])
	assert.Equal(t, "message_stop", gjson.Get(lastData, "type").String())

	// Count how many message_start events exist in the output.
	msgStartCount := 0
	for _, e := range events {
		data := extractDataField(e)
		if gjson.Get(data, "type").String() == "message_start" {
			msgStartCount++
		}
	}
	assert.Equal(t, 1, msgStartCount, "exactly one message_start must reach the client (prelude only)")
}

func TestAnthropicRoutingMarkerWriter_MarkerEmittedOnce(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewAnthropicRoutingMarkerWriter(rec, "claude-opus-4", "✦ marker")

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	// Two separate Write calls; marker must only be emitted on the first.
	chunk1 := buildAnthropicSSE("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	chunk2 := buildAnthropicSSE("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`)

	_, err := w.Write([]byte(chunk1))
	require.NoError(t, err)

	_, err = w.Write([]byte(chunk2))
	require.NoError(t, err)

	body := rec.Body.String()
	events := splitSSEEvents(body)

	// Prelude (4) + chunk1 (1) + chunk2 (1) = 6
	require.Len(t, events, 6, "expected prelude (4) + upstream (2)")

	// Verify marker (text_delta with marker content) appears exactly once.
	markerCount := 0
	for _, e := range events {
		data := extractDataField(e)
		if gjson.Get(data, "type").String() == "content_block_delta" {
			deltaType := gjson.Get(data, "delta.type").String()
			text := gjson.Get(data, "delta.text").String()
			if deltaType == "text_delta" && strings.Contains(text, "marker") {
				markerCount++
			}
		}
	}
	assert.Equal(t, 1, markerCount, "marker text_delta must be emitted exactly once")
}

func TestAnthropicRoutingMarkerWriter_PreludeFiresBeforeUpstream(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewAnthropicRoutingMarkerWriter(rec, "claude-opus-4", "✦ Weave Router → claude-opus-4")

	require.NoError(t, w.Prelude(true))

	// Prelude must have flushed HTTP 200 + all 4 prelude events before any upstream byte.
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	events := splitSSEEvents(rec.Body.String())
	require.Len(t, events, 4, "prelude should emit 4 events: message_start + content_block_start/delta/stop")

	// Verify prelude contains the marker text.
	deltaData := extractDataField(events[2])
	assert.Contains(t, gjson.Get(deltaData, "delta.text").String(), "Weave Router")

	// A subsequent upstream chunk must NOT trigger a duplicate marker.
	upstream := buildAnthropicSSE("message_stop", `{"type":"message_stop"}`)
	_, err := w.Write([]byte(upstream))
	require.NoError(t, err)

	events = splitSSEEvents(rec.Body.String())
	require.Len(t, events, 5, "prelude (4) + message_stop (1) — no duplicate marker")
}

func TestAnthropicRoutingMarkerWriter_PreludeNoOpWhenNonStreaming(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewAnthropicRoutingMarkerWriter(rec, "claude-opus-4", "marker")

	require.NoError(t, w.Prelude(false))
	assert.Empty(t, rec.Body.String(), "non-streaming Prelude must write nothing")
}

func TestAnthropicRoutingMarkerWriter_PreludeEmptyMarker(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewAnthropicRoutingMarkerWriter(rec, "claude-opus-4", "")

	require.NoError(t, w.Prelude(true))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))

	// The empty marker prelude should write a comment only.
	body := rec.Body.String()
	assert.Contains(t, body, ": routing complete")
}

// extractDataField extracts the JSON data payload from an Anthropic-format SSE
// event line (the value after "data: ").
func extractDataField(event string) string {
	lines := strings.Split(event, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data: ") {
			return strings.TrimPrefix(line, "data: ")
		}
	}
	return ""
}

// TestAnthropicRoutingMarkerWriter_EventSplitAcrossWrites verifies that SSE
// events split across multiple Write calls are buffered and emitted correctly
// (regression test for the persistent buffer fix).
func TestAnthropicRoutingMarkerWriter_EventSplitAcrossWrites(t *testing.T) {
	rec := httptest.NewRecorder()
	markerText := "✦ **Weave Router** → claude-opus-4"
	w := translate.NewAnthropicRoutingMarkerWriter(rec, "claude-opus-4", markerText)

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	// Simulate the upstream splitting an event across two Write calls.
	// This is a common case when the upstream is buffering at TCP window boundaries.
	upstreamEvent := `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"The answer is 42."}}`

	// First Write: partial event line (everything up to JSON close + newline).
	part1 := "event: content_block_delta\ndata: " + upstreamEvent[:len(upstreamEvent)-2]
	_, err := w.Write([]byte(part1))
	require.NoError(t, err)
	require.Len(t, splitSSEEvents(rec.Body.String()), 4, "partial event should stay buffered after the first write")

	// Second Write: rest of the event (close of JSON + trailing blank line).
	part2 := upstreamEvent[len(upstreamEvent)-2:] + "\n\n"
	_, err = w.Write([]byte(part2))
	require.NoError(t, err)

	body := rec.Body.String()
	events := splitSSEEvents(body)

	// Expect: marker prelude (4 events) + shifted content_block_delta at index 1.
	require.Len(t, events, 5, "should have prelude + one reassembled content_block_delta")

	// Find the rewritten content_block_delta.
	var foundShiftedDelta bool
	for _, event := range events {
		if strings.Contains(event, "content_block_delta") {
			data := extractDataField(event)
			idx := gjson.Get(data, "index").Int()
			text := gjson.Get(data, "delta.text").String()
			if idx == 1 && text == "The answer is 42." {
				foundShiftedDelta = true
				break
			}
		}
	}
	assert.True(t, foundShiftedDelta, "split event must be reassembled with shifted index and preserved text")
}
