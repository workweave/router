package translate_test

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"workweave/router/internal/translate"
	"workweave/router/internal/translate/toolcheck"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func firstSignatureDelta(t *testing.T, body string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || gjson.Get(data, "delta.type").String() != "signature_delta" {
			continue
		}
		sig := gjson.Get(data, "delta.signature").String()
		require.NotEmpty(t, sig)
		return sig
	}
	t.Fatal("missing signature_delta")
	return ""
}

// responsesStreamFixture is a representative OpenAI Responses streaming SSE
// sequence: a reasoning summary, an output_text message, and one function_call
// whose arguments arrive split across two deltas, terminated by
// response.completed carrying usage + status.
const responsesStreamFixture = `event: response.created
data: {"type":"response.created","response":{"id":"resp_abc","status":"in_progress","model":"gpt-5.5","output":[]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"enc_stream","summary":[],"status":"in_progress"}}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"Checking the weather "}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"tool."}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"enc_stream","summary":[{"type":"summary_text","text":"Checking the weather tool."}],"status":"completed"}}

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
data: {"type":"response.completed","response":{"id":"resp_abc","status":"completed","model":"gpt-5.5","incomplete_details":null,"output":[{"id":"rs_1","type":"reasoning","encrypted_content":"enc_stream","summary":[{"type":"summary_text","text":"Checking the weather tool."}]},{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Let me check the weather."}]},{"id":"fc_1","type":"function_call","call_id":"call_xyz","name":"get_weather","arguments":"{\"location\":\"NYC\"}"}],"usage":{"input_tokens":150,"input_tokens_details":{"cached_tokens":0},"output_tokens":45}}}

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
	sig := firstSignatureDelta(t, body)
	sigEnv := decodeOpenAIReasoningTestSignature(t, sig)
	assert.Equal(t, "rs_1", sigEnv["id"])
	assert.Equal(t, "enc_stream", sigEnv["enc"])
	assert.Contains(t, body, `"content_block":{"type":"text"`)
	// Text arrives as separate live deltas, not one concatenated string.
	assert.Contains(t, body, `"text_delta","text":"Let me check"`)
	assert.Contains(t, body, `"text_delta","text":" the weather."`)
	// The reasoning item's signature is carried on the tool_use id so it survives
	// the Claude Code round-trip; the id is the call_id plus an opaque suffix.
	assert.Contains(t, body, `"type":"tool_use","id":"call_xyz__openai_reasoning__`)
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
		`"type":"signature_delta"`,
		`"content_block":{"type":"text"`,
		`"type":"tool_use","id":"call_xyz__openai_reasoning__`,
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
	toolID, _ := b2["id"].(string)
	assert.True(t, strings.HasPrefix(toolID, "call_xyz"), "tool id keeps call_id prefix, got %q", toolID)
	assert.Contains(t, toolID, "__openai_reasoning__", "tool id carries the reasoning signature for replay")
	input, _ := b2["input"].(map[string]any)
	assert.Equal(t, "NYC", input["location"])
}

func TestResponsesToAnthropicWriter_NonStreamingSummaryCarriesUsageCounts(t *testing.T) {
	const fixture = `event: response.completed
data: {"type":"response.completed","response":{"id":"r","status":"completed","model":"gpt-5.5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":1200,"input_tokens_details":{"cached_tokens":800},"output_tokens":340}}}

`
	rec := httptest.NewRecorder()
	w := translate.NewResponsesToAnthropicWriter(rec, "gpt-5.5", nil)
	require.NoError(t, w.Prelude(false))
	_, err := w.Write([]byte(fixture))
	require.NoError(t, err)
	require.NoError(t, w.Finalize())

	got := w.Summary()
	assert.Equal(t, 1200, got.InputTokens)
	assert.Equal(t, 340, got.OutputTokens)
	assert.Equal(t, 800, got.CacheReadTokens)
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

// readToolValidator compiles a Claude-Code-like Read tool schema: file_path
// required, pages optional.
func readToolValidator(t *testing.T) *toolcheck.Validator {
	t.Helper()
	v := toolcheck.Compile([]byte(`[{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"pages":{"type":"string"}},"required":["file_path"]}}]`))
	require.NotNil(t, v)
	return v
}

// writeToolValidator compiles a schema set that does NOT contain Read, for the
// unknown-tool passthrough cases.
func writeToolValidator(t *testing.T) *toolcheck.Validator {
	t.Helper()
	v := toolcheck.Compile([]byte(`[{"name":"Write","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}]`))
	require.NotNil(t, v)
	return v
}

// gpt-5.x reasoning models emit optional string params (e.g. Read.pages) as ""
// rather than omitting them, which fails the client's tool validation and loops
// the model. With the tool's schema validator installed, the writer strips the
// empty optional arg so the client receives a valid call; the required file_path
// is preserved.
func TestResponsesToAnthropicWriter_StripsEmptyOptionalArg(t *testing.T) {
	const fixture = `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_a","name":"Read","arguments":"","status":"in_progress"}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_a","name":"Read","arguments":"{\"file_path\":\"x.go\",\"pages\":\"\"}","status":"completed"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"r","status":"completed","output":[{"id":"fc_1","type":"function_call","call_id":"call_a","name":"Read","arguments":"{\"file_path\":\"x.go\",\"pages\":\"\"}"}],"usage":{"input_tokens":10,"output_tokens":5}}}

`
	rec := httptest.NewRecorder()
	w := translate.NewResponsesToAnthropicWriter(rec, "gpt-5.5", nil).
		WithToolValidator(readToolValidator(t))
	require.NoError(t, w.Prelude(true))
	_, err := w.Write([]byte(fixture))
	require.NoError(t, err)
	require.NoError(t, w.Finalize())

	body := rec.Body.String()
	assert.Contains(t, body, `"partial_json":"{\"file_path\":\"x.go\"}"`,
		"empty optional pages must be stripped so the client tool doesn't error")
	assert.NotContains(t, body, `pages`,
		"the empty optional arg must not reach the client at all")
}

func TestResponsesToAnthropicWriter_NonStreamingStripsEmptyOptionalArg(t *testing.T) {
	const fixture = `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_a","name":"Read","arguments":"","status":"in_progress"}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_a","name":"Read","arguments":"{\"file_path\":\"x.go\",\"pages\":\"\"}","status":"completed"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"r","status":"completed","output":[{"id":"fc_1","type":"function_call","call_id":"call_a","name":"Read","arguments":"{\"file_path\":\"x.go\",\"pages\":\"\"}"}],"usage":{"input_tokens":10,"output_tokens":5}}}

`
	rec := httptest.NewRecorder()
	w := translate.NewResponsesToAnthropicWriter(rec, "gpt-5.5", nil).
		WithToolValidator(readToolValidator(t))
	require.NoError(t, w.Prelude(false))
	_, err := w.Write([]byte(fixture))
	require.NoError(t, err)
	require.NoError(t, w.Finalize())

	var msg map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &msg))
	content, _ := msg["content"].([]any)
	require.Len(t, content, 1)
	tool, _ := content[0].(map[string]any)
	input, _ := tool["input"].(map[string]any)
	assert.Equal(t, "x.go", input["file_path"])
	assert.NotContains(t, input, "pages",
		"non-streaming conversion must apply the same empty optional strip")
}

func TestResponsesToAnthropicWriter_NonStreamingNoStripForUnknownToolSchema(t *testing.T) {
	const fixture = `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_a","name":"Read","arguments":"","status":"in_progress"}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_a","name":"Read","arguments":"{\"file_path\":\"x.go\",\"pages\":\"\"}","status":"completed"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"r","status":"completed","output":[{"id":"fc_1","type":"function_call","call_id":"call_a","name":"Read","arguments":"{\"file_path\":\"x.go\",\"pages\":\"\"}"}],"usage":{"input_tokens":10,"output_tokens":5}}}

`
	rec := httptest.NewRecorder()
	w := translate.NewResponsesToAnthropicWriter(rec, "gpt-5.5", nil).
		WithToolValidator(writeToolValidator(t))
	require.NoError(t, w.Prelude(false))
	_, err := w.Write([]byte(fixture))
	require.NoError(t, err)
	require.NoError(t, w.Finalize())

	var msg map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &msg))
	content, _ := msg["content"].([]any)
	require.Len(t, content, 1)
	tool, _ := content[0].(map[string]any)
	input, _ := tool["input"].(map[string]any)
	assert.Equal(t, "", input["pages"],
		"schema for a different tool must not authorize stripping this tool's args")
}

// Without a required-param map (no tools / non-Anthropic inbound) the writer
// passes args through verbatim — the strip is opt-in and never guesses.
func TestResponsesToAnthropicWriter_NoStripWithoutSchema(t *testing.T) {
	const fixture = `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_a","name":"Read","arguments":"","status":"in_progress"}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_a","name":"Read","arguments":"{\"file_path\":\"x.go\",\"pages\":\"\"}","status":"completed"}}

`
	rec := httptest.NewRecorder()
	w := translate.NewResponsesToAnthropicWriter(rec, "gpt-5.5", nil)
	require.NoError(t, w.Prelude(true))
	_, err := w.Write([]byte(fixture))
	require.NoError(t, err)
	require.NoError(t, w.Finalize())

	assert.Contains(t, rec.Body.String(), `\"pages\":\"\"`,
		"without a schema the writer must not strip anything")
}

func TestResponsesToAnthropicWriter_NoStripForUnknownToolSchema(t *testing.T) {
	const fixture = `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_a","name":"Read","arguments":"","status":"in_progress"}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_a","name":"Read","arguments":"{\"file_path\":\"x.go\",\"pages\":\"\"}","status":"completed"}}

`
	rec := httptest.NewRecorder()
	w := translate.NewResponsesToAnthropicWriter(rec, "gpt-5.5", nil).
		WithToolValidator(writeToolValidator(t))
	require.NoError(t, w.Prelude(true))
	_, err := w.Write([]byte(fixture))
	require.NoError(t, err)
	require.NoError(t, w.Finalize())

	assert.Contains(t, rec.Body.String(), `\"pages\":\"\"`,
		"schema for a different tool must not authorize stripping this tool's args")
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
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"enc_stream","summary":[],"status":"in_progress"}}

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
