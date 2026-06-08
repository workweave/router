package translate_test

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// responsesStreamFixture is a representative OpenAI Responses streaming SSE
// sequence: a reasoning summary, an output_text message, and one function_call
// whose arguments arrive split across two deltas, terminated by
// response.completed carrying usage + status.
const responsesStreamFixture = `event: response.created
data: {"type":"response.created","response":{"id":"resp_abc","status":"in_progress","model":"gpt-5.5","output":[]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[],"status":"in_progress"}}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"Checking the weather "}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"tool."}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"Checking the weather tool."}],"status":"completed"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","status":"in_progress","content":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"content_index":0,"delta":"Let me check"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"content_index":0,"delta":" the weather."}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Let me check the weather."}]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":2,"item":{"id":"fc_1","type":"function_call","call_id":"call_xyz","name":"get_weather","arguments":"","status":"in_progress"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":2,"delta":"{\"location\":"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":2,"delta":"\"NYC\"}"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":2,"item":{"id":"fc_1","type":"function_call","call_id":"call_xyz","name":"get_weather","arguments":"{\"location\":\"NYC\"}","status":"completed"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_abc","status":"completed","model":"gpt-5.5","incomplete_details":null,"output":[{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"Checking the weather tool."}]},{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Let me check the weather."}]},{"id":"fc_1","type":"function_call","call_id":"call_xyz","name":"get_weather","arguments":"{\"location\":\"NYC\"}"}],"usage":{"input_tokens":150,"input_tokens_details":{"cached_tokens":0},"output_tokens":45}}}

`

// A streaming client gets the Responses event stream translated to Anthropic SSE
// incrementally: thinking → text → tool_use blocks in order, then a
// message_delta carrying stop_reason=tool_use and the upstream usage.
func TestResponsesToAnthropicWriter_StreamingClient(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewResponsesToAnthropicWriter(rec, "gpt-5.5", nil)

	require.NoError(t, w.Prelude(true))
	_, err := w.Write([]byte(responsesStreamFixture))
	require.NoError(t, err)
	require.NoError(t, w.Finalize())

	body := rec.Body.String()

	// Frame presence.
	assert.Contains(t, body, "event: message_start")
	assert.Contains(t, body, `"content_block":{"type":"thinking"`)
	assert.Contains(t, body, "Checking the weather ")
	assert.Contains(t, body, "tool.")
	assert.Contains(t, body, `"content_block":{"type":"text"`)
	// Text arrives as separate live deltas, not one concatenated string.
	assert.Contains(t, body, `"text_delta","text":"Let me check"`)
	assert.Contains(t, body, `"text_delta","text":" the weather."`)
	assert.Contains(t, body, `"type":"tool_use","id":"call_xyz","name":"get_weather"`)
	assert.Contains(t, body, `"partial_json":"{\"location\":\"NYC\"}"`,
		"tool args concatenated from both deltas and emitted as one validated input_json_delta")
	assert.Contains(t, body, `"stop_reason":"tool_use"`)
	assert.Contains(t, body, `"output_tokens":45`)
	assert.Contains(t, body, `"input_tokens":150`)
	assert.Contains(t, body, "event: message_stop")

	// Block ordering: thinking (0) → text (1) → tool_use (2), all between
	// message_start and message_delta.
	order := []string{
		"event: message_start",
		`"content_block":{"type":"thinking"`,
		`"content_block":{"type":"text"`,
		`"type":"tool_use","id":"call_xyz"`,
		"event: message_delta",
		"event: message_stop",
	}
	prev := -1
	for _, marker := range order {
		at := strings.Index(body, marker)
		require.GreaterOrEqual(t, at, 0, "missing %q", marker)
		assert.Greater(t, at, prev, "out of order: %q", marker)
		prev = at
	}

	// Anthropic block indices must be dense and contiguous from 0.
	assert.Contains(t, body, `"content_block_start","index":0`)
	assert.Contains(t, body, `"content_block_start","index":1`)
	assert.Contains(t, body, `"content_block_start","index":2`)
}

// A non-streaming client gets a one-shot Anthropic JSON body reconstructed from
// the terminal response.completed event in the (still-streamed) upstream.
func TestResponsesToAnthropicWriter_NonStreamingClient(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewResponsesToAnthropicWriter(rec, "gpt-5.5", nil)

	// Prelude(false): client did not request streaming, so the writer buffers.
	require.NoError(t, w.Prelude(false))
	_, err := w.Write([]byte(responsesStreamFixture))
	require.NoError(t, err)
	require.NoError(t, w.Finalize())

	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.NotContains(t, rec.Body.String(), "event: ", "non-streaming client gets JSON, not SSE")

	var msg map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &msg))
	assert.Equal(t, "message", msg["type"])
	assert.Equal(t, "tool_use", msg["stop_reason"])
	content, _ := msg["content"].([]any)
	require.Len(t, content, 3)
	b2, _ := content[2].(map[string]any)
	assert.Equal(t, "tool_use", b2["type"])
	assert.Equal(t, "call_xyz", b2["id"])
	input, _ := b2["input"].(map[string]any)
	assert.Equal(t, "NYC", input["location"])
}

// A function_call that streams no argument deltas still delivers its real
// arguments: the translator falls back to the authoritative item.arguments on
// the terminal output_item.done rather than emitting {}.
func TestResponsesToAnthropicWriter_ToolArgsFromDoneEvent(t *testing.T) {
	const fixture = `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_a","name":"Read","arguments":"","status":"in_progress"}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_a","name":"Read","arguments":"{\"path\":\"x.go\"}","status":"completed"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"r","status":"completed","output":[{"id":"fc_1","type":"function_call","call_id":"call_a","name":"Read","arguments":"{\"path\":\"x.go\"}"}],"usage":{"input_tokens":10,"output_tokens":5}}}

`
	rec := httptest.NewRecorder()
	w := translate.NewResponsesToAnthropicWriter(rec, "gpt-5.5", nil)
	require.NoError(t, w.Prelude(true))
	_, err := w.Write([]byte(fixture))
	require.NoError(t, err)
	require.NoError(t, w.Finalize())

	body := rec.Body.String()
	assert.Contains(t, body, `"partial_json":"{\"path\":\"x.go\"}"`,
		"no arg deltas → fall back to item.arguments on output_item.done, not {}")
	assert.NotContains(t, body, `"partial_json":"{}"`)
	assert.Contains(t, body, `"stop_reason":"tool_use"`)
}

// A stream truncated before response.completed still reconciles to
// stop_reason=tool_use (a tool block was emitted) and flushes the partial
// tool args, rather than defaulting to end_turn with a dropped input_json_delta.
func TestResponsesToAnthropicWriter_TruncatedToolStream(t *testing.T) {
	const fixture = `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_a","name":"Bash","arguments":"","status":"in_progress"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"command\":"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"\"ls\"}"}

`
	rec := httptest.NewRecorder()
	w := translate.NewResponsesToAnthropicWriter(rec, "gpt-5.5", nil)
	require.NoError(t, w.Prelude(true))
	_, err := w.Write([]byte(fixture))
	require.NoError(t, err)
	require.NoError(t, w.Finalize()) // no response.completed arrived

	body := rec.Body.String()
	assert.Contains(t, body, `"partial_json":"{\"command\":\"ls\"}"`,
		"buffered tool args flushed on early close")
	assert.Contains(t, body, `"stop_reason":"tool_use"`,
		"a tool_use block was emitted → invariant forces tool_use even with no terminal event")
	assert.Contains(t, body, "event: message_stop")
}

// An upstream that delivers a reasoning summary and message text only on
// output_item.done (no *.delta events) still produces visible thinking + text
// blocks on the streaming path, rather than an empty assistant turn.
func TestResponsesToAnthropicWriter_DoneOnlyContent(t *testing.T) {
	const fixture = `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[],"status":"in_progress"}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"weighed the options"}],"status":"completed"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","status":"in_progress","content":[]}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"final answer"}]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"r","status":"completed","output":[{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"weighed the options"}]},{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"final answer"}]}],"usage":{"input_tokens":7,"output_tokens":3}}}

`
	rec := httptest.NewRecorder()
	w := translate.NewResponsesToAnthropicWriter(rec, "gpt-5.5", nil)
	require.NoError(t, w.Prelude(true))
	_, err := w.Write([]byte(fixture))
	require.NoError(t, err)
	require.NoError(t, w.Finalize())

	body := rec.Body.String()
	assert.Contains(t, body, `"content_block":{"type":"thinking"`)
	assert.Contains(t, body, `"thinking_delta","thinking":"weighed the options"`)
	assert.Contains(t, body, `"content_block":{"type":"text"`)
	assert.Contains(t, body, `"text_delta","text":"final answer"`)
	assert.Contains(t, body, `"stop_reason":"end_turn"`)
}

// A function_call with no name is dropped (never opened as a tool_use block),
// so the client can't be sent on an invoke-"" loop; the turn demotes to
// end_turn since no tool_use block survives.
func TestResponsesToAnthropicWriter_NamelessToolDropped(t *testing.T) {
	const fixture = `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_x","type":"function_call","call_id":"call_x","name":"","arguments":"","status":"in_progress"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_x","output_index":0,"delta":"{\"a\":1}"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_x","type":"function_call","call_id":"call_x","name":"","arguments":"{\"a\":1}","status":"completed"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"r","status":"completed","output":[{"id":"fc_x","type":"function_call","call_id":"call_x","name":"","arguments":"{\"a\":1}"}],"usage":{"input_tokens":3,"output_tokens":1}}}

`
	rec := httptest.NewRecorder()
	w := translate.NewResponsesToAnthropicWriter(rec, "gpt-5.5", nil)
	require.NoError(t, w.Prelude(true))
	_, err := w.Write([]byte(fixture))
	require.NoError(t, err)
	require.NoError(t, w.Finalize())

	body := rec.Body.String()
	assert.NotContains(t, body, `"type":"tool_use"`, "nameless function_call must not open a tool_use block")
	assert.NotContains(t, body, "input_json_delta")
	assert.Contains(t, body, `"stop_reason":"end_turn"`, "no surviving tool_use block → demote to end_turn")
	assert.Contains(t, body, "event: message_stop")
}

// On the non-streaming path, an upstream stream that ends without a terminal
// response event but carries an `error` event yields a real Anthropic error
// envelope — not an empty 502 from feeding raw SSE to the JSON error mapper.
func TestResponsesToAnthropicWriter_NonStreamingErrorFromStream(t *testing.T) {
	const fixture = `event: error
data: {"type":"error","code":"server_error","message":"upstream exploded"}

`
	rec := httptest.NewRecorder()
	w := translate.NewResponsesToAnthropicWriter(rec, "gpt-5.5", nil)
	require.NoError(t, w.Prelude(false)) // non-streaming client → buffered
	_, err := w.Write([]byte(fixture))
	require.NoError(t, err)
	require.NoError(t, w.Finalize())

	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.NotContains(t, rec.Body.String(), "event: ")
	var msg map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &msg))
	assert.Equal(t, "error", msg["type"])
	e, _ := msg["error"].(map[string]any)
	require.NotNil(t, e)
	assert.Contains(t, e["message"], "upstream exploded", "real upstream message surfaced, not empty")
}

// A streaming client sees a stream-level failure (response.failed over HTTP 200)
// as an Anthropic `event: error`, not a silent normal close.
func TestResponsesToAnthropicWriter_StreamingErrorEvent(t *testing.T) {
	const fixture = `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"in_progress","content":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"partial"}

event: response.failed
data: {"type":"response.failed","response":{"id":"r","status":"failed","error":{"code":"server_error","message":"model crashed mid-stream"}}}

`
	rec := httptest.NewRecorder()
	w := translate.NewResponsesToAnthropicWriter(rec, "gpt-5.5", nil)
	require.NoError(t, w.Prelude(true))
	_, err := w.Write([]byte(fixture))
	require.NoError(t, err)
	require.NoError(t, w.Finalize())

	body := rec.Body.String()
	assert.Contains(t, body, "event: error", "stream-level failure surfaces as an Anthropic error event")
	assert.Contains(t, body, "model crashed mid-stream")
	assert.NotContains(t, body, "event: message_stop", "no success trailer after an error close")
}

// response.failed with no error object is still surfaced as an error (generic
// message), not a normal success close.
func TestResponsesToAnthropicWriter_StreamingFailedNoErrorObject(t *testing.T) {
	const fixture = `event: response.failed
data: {"type":"response.failed","response":{"id":"r","status":"failed","output":[]}}

`
	rec := httptest.NewRecorder()
	w := translate.NewResponsesToAnthropicWriter(rec, "gpt-5.5", nil)
	require.NoError(t, w.Prelude(true))
	_, err := w.Write([]byte(fixture))
	require.NoError(t, err)
	require.NoError(t, w.Finalize())

	body := rec.Body.String()
	assert.Contains(t, body, "event: error")
	assert.NotContains(t, body, "event: message_stop")
}

// A function_call delivered only on output_item.done (no output_item.added)
// still opens a tool_use block with its real id/name/args.
func TestResponsesToAnthropicWriter_DoneOnlyToolCall(t *testing.T) {
	const fixture = `event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_z","name":"Grep","arguments":"{\"pattern\":\"foo\"}","status":"completed"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"r","status":"completed","output":[{"id":"fc_1","type":"function_call","call_id":"call_z","name":"Grep","arguments":"{\"pattern\":\"foo\"}"}],"usage":{"input_tokens":4,"output_tokens":2}}}

`
	rec := httptest.NewRecorder()
	w := translate.NewResponsesToAnthropicWriter(rec, "gpt-5.5", nil)
	require.NoError(t, w.Prelude(true))
	_, err := w.Write([]byte(fixture))
	require.NoError(t, err)
	require.NoError(t, w.Finalize())

	body := rec.Body.String()
	assert.Contains(t, body, `"type":"tool_use","id":"call_z","name":"Grep"`)
	assert.Contains(t, body, `"partial_json":"{\"pattern\":\"foo\"}"`)
	assert.Contains(t, body, `"stop_reason":"tool_use"`)
}

// A non-streaming client also gets an error envelope (not empty success JSON)
// when the buffered stream ends in response.failed — symmetric with the
// streaming path's event: error.
func TestResponsesToAnthropicWriter_NonStreamingFailedResponse(t *testing.T) {
	const fixture = `event: response.failed
data: {"type":"response.failed","response":{"id":"r","status":"failed","error":{"code":"server_error","message":"boom"},"output":[]}}

`
	rec := httptest.NewRecorder()
	w := translate.NewResponsesToAnthropicWriter(rec, "gpt-5.5", nil)
	require.NoError(t, w.Prelude(false))
	_, err := w.Write([]byte(fixture))
	require.NoError(t, err)
	require.NoError(t, w.Finalize())

	var msg map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &msg))
	assert.Equal(t, "error", msg["type"], "failed response → error envelope, not an empty assistant message")
	e, _ := msg["error"].(map[string]any)
	require.NotNil(t, e)
	assert.Contains(t, e["message"], "boom")
}

// A routing marker is emitted as content block 0; upstream content then starts
// at block 1.
func TestResponsesToAnthropicWriter_RoutingMarker(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewResponsesToAnthropicWriter(rec, "gpt-5.5", nil).WithRoutingMarker("[routed: gpt-5.5]")

	require.NoError(t, w.Prelude(true))
	_, err := w.Write([]byte(responsesStreamFixture))
	require.NoError(t, err)
	require.NoError(t, w.Finalize())

	body := rec.Body.String()
	assert.Contains(t, body, "[routed: gpt-5.5]")
	markerAt := strings.Index(body, "[routed: gpt-5.5]")
	thinkingAt := strings.Index(body, `"content_block":{"type":"thinking"`)
	require.GreaterOrEqual(t, markerAt, 0)
	require.GreaterOrEqual(t, thinkingAt, 0)
	assert.Less(t, markerAt, thinkingAt, "routing marker precedes upstream content")
	// Marker occupies block 0, so reasoning opens at block 1.
	assert.Contains(t, body, `"content_block_start","index":1`)
}
