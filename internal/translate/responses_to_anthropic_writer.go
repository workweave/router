package translate

import (
	"bufio"
	"bytes"
	"net/http"
	"strings"

	"workweave/router/internal/observability"
	"workweave/router/internal/observability/otel"
	"workweave/router/internal/sse"

	"github.com/tidwall/gjson"
)

// ResponsesToAnthropicWriter adapts a STREAMING OpenAI Responses upstream
// (`POST /v1/responses` with `stream:true`) into an Anthropic Messages response
// for the client. For a streaming client it parses the Responses SSE event
// stream and emits Anthropic SSE incrementally (reasoning → thinking, output
// text → text, function_call → tool_use); for a non-streaming client it buffers
// the stream and renders a one-shot Anthropic JSON body from the terminal
// `response.completed` event via ResponsesToAnthropicResponse. It exposes the
// Prelude/Write/Finalize/Summary surface the proxy's OpenAI dispatch expects,
// mirroring AnthropicSSETranslator so the dispatch closure is identical.
type ResponsesToAnthropicWriter struct {
	inner        http.ResponseWriter
	flusher      http.Flusher
	bw           *bufio.Writer
	requestModel string
	usageSink    otel.UsageSink

	routingMarker        string
	estimatedInputTokens int
	requestHadTools      bool

	buf            bytes.Buffer
	statusCode     int
	streaming      bool
	headersEmitted bool
	started        bool
	// closed guards against a second close after Finalize emits the trailer.
	closed bool

	// blockIdx is the next Anthropic content-block index to assign.
	blockIdx int
	// itemBlocks maps an OpenAI Responses output_index to the Anthropic content
	// block index opened for it. Responses emits output items sequentially, but
	// keying by output_index keeps each item's deltas correlated to its block
	// regardless of ordering.
	itemBlocks map[int]int
	// itemKind records the Anthropic block kind per output_index so the matching
	// output_item.done can close it correctly.
	itemKind map[int]string
	// toolArgs accumulates function_call_arguments deltas per output_index. The
	// concatenated payload is validated and emitted as a single input_json_delta
	// at item close — mirroring AnthropicSSETranslator, a payload that fails to
	// parse becomes `{}` rather than killing the client's strict tool-args
	// parser mid-turn.
	toolArgs map[int]*strings.Builder
	// suppressed holds output_indexes of function_call items dropped for carrying
	// no name. A nameless tool_use makes the client invoke tool "" in a loop, so
	// (mirroring AnthropicSSETranslator) we never open a block for it and drop its
	// later arg deltas / done event.
	suppressed map[int]struct{}

	// Captured from the terminal response.completed/.failed/.incomplete event.
	finalStopReason string
	hasUsage        bool
	usageInput      int
	usageOutput     int
	usageCacheRead  int

	// Summary fields.
	toolUseCount      int
	emittedStopReason string
}

// NewResponsesToAnthropicWriter wraps w to translate a streaming Responses
// upstream into Anthropic for the client.
func NewResponsesToAnthropicWriter(w http.ResponseWriter, requestModel string, sink otel.UsageSink) *ResponsesToAnthropicWriter {
	flusher, _ := w.(http.Flusher)
	return &ResponsesToAnthropicWriter{
		inner:        w,
		flusher:      flusher,
		bw:           bufio.NewWriterSize(w, 8192),
		requestModel: requestModel,
		usageSink:    sink,
		statusCode:   http.StatusOK,
		itemBlocks:   make(map[int]int),
		itemKind:     make(map[int]string),
		toolArgs:     make(map[int]*strings.Builder),
		suppressed:   make(map[int]struct{}),
	}
}

func (t *ResponsesToAnthropicWriter) WithRoutingMarker(marker string) *ResponsesToAnthropicWriter {
	t.routingMarker = marker
	return t
}

func (t *ResponsesToAnthropicWriter) WithEstimatedInputTokens(n int) *ResponsesToAnthropicWriter {
	if n > 0 {
		t.estimatedInputTokens = n
	}
	return t
}

func (t *ResponsesToAnthropicWriter) WithRequestHadTools(hadTools bool) *ResponsesToAnthropicWriter {
	t.requestHadTools = hadTools
	return t
}

func (t *ResponsesToAnthropicWriter) Header() http.Header { return t.inner.Header() }

// WriteHeader captures the upstream status. The streaming decision is the
// CLIENT's, committed in Prelude, so we never flip to SSE here.
func (t *ResponsesToAnthropicWriter) WriteHeader(code int) {
	if t.headersEmitted {
		return
	}
	t.statusCode = code
}

// Write receives upstream Responses bytes. When the client is streaming, it
// parses complete SSE events and emits Anthropic frames on the fly; otherwise
// it buffers the raw stream for Finalize.
func (t *ResponsesToAnthropicWriter) Write(data []byte) (int, error) {
	n := len(data)
	t.buf.Write(data)
	if !t.streaming {
		return n, nil
	}
	return n, t.processResponsesSSEBuffer()
}

func (t *ResponsesToAnthropicWriter) Flush() {
	if t.streaming && t.flusher != nil {
		t.flusher.Flush()
	}
}

// Prelude commits SSE headers + message_start (+ routing marker block) eagerly
// when the client requested streaming, so the client sees the message envelope
// while the upstream is still reasoning.
func (t *ResponsesToAnthropicWriter) Prelude(streaming bool) error {
	if !streaming || t.started {
		return nil
	}
	t.inner.Header().Set("Content-Type", "text/event-stream")
	t.inner.Header().Del("Content-Length")
	t.inner.Header().Del("Content-Encoding")
	t.streaming = true
	t.statusCode = http.StatusOK
	t.inner.WriteHeader(http.StatusOK)
	t.headersEmitted = true
	t.started = true
	if err := t.emitMessageStart(); err != nil {
		return err
	}
	return t.emitRoutingMarkerIfConfigured()
}

// Finalize emits the streaming trailer (message_delta + message_stop) for a
// streaming client, or renders the buffered stream as one-shot Anthropic JSON
// for a non-streaming client.
func (t *ResponsesToAnthropicWriter) Finalize() error {
	if t.streaming {
		if t.closed {
			return nil
		}
		return t.finishStream()
	}
	return t.finalizeBuffered()
}

func (t *ResponsesToAnthropicWriter) Summary() ResponseSummary {
	return ResponseSummary{
		StopReason:    t.emittedStopReason,
		ToolUseBlocks: t.toolUseCount,
		OutputTokens:  t.usageOutput,
	}
}

// --- streaming path ---

func (t *ResponsesToAnthropicWriter) processResponsesSSEBuffer() error {
	for {
		event, n := sse.SplitNext(t.buf.Bytes())
		if n == 0 {
			return nil
		}
		err := t.translateResponsesEvent(event)
		t.buf.Next(n)
		if err != nil {
			return err
		}
	}
}

func (t *ResponsesToAnthropicWriter) translateResponsesEvent(raw []byte) error {
	if t.closed {
		return nil
	}
	_, data := sse.ParseEvent(raw)
	if len(data) == 0 {
		return nil
	}
	// Match on the in-payload `type` (the `event:` line duplicates it but is
	// sometimes dropped by intermediaries). Unknown response.* events are
	// ignored; the ones below cover reasoning, text, tool calls, the terminal
	// envelope, and stream-level failures.
	switch gjson.GetBytes(data, "type").String() {
	case "response.output_item.added":
		return t.handleOutputItemAdded(data)
	case "response.output_text.delta":
		return t.handleTextDelta(data)
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		return t.handleReasoningDelta(data)
	case "response.function_call_arguments.delta":
		t.bufferToolArgs(data, "delta", true)
		return nil
	case "response.function_call_arguments.done":
		// The terminal arg event carries the complete `arguments` string. Adopt
		// it as authoritative so a tool call whose deltas were absent or lost
		// still emits the real parameters rather than `{}`.
		t.bufferToolArgs(data, "arguments", false)
		return nil
	case "response.output_item.done":
		return t.handleOutputItemDone(data)
	case "error":
		// Stream-level failure over HTTP 200 — surface it as an Anthropic error
		// event instead of closing the turn as if it succeeded.
		return t.emitStreamErrorEvent(gjson.GetBytes(data, "code").String(), gjson.GetBytes(data, "message").String())
	case "response.failed":
		// response.failed is always an upstream failure — surface it as an error
		// even when no error object rode along (responsesError fills a generic
		// message), matching the buffered path's status:"failed" rejection.
		e := gjson.GetBytes(data, "response.error")
		return t.emitStreamErrorEvent(e.Get("code").String(), e.Get("message").String())
	case "response.completed", "response.incomplete":
		t.captureFinalResponse(data)
		return nil
	}
	return nil
}

func (t *ResponsesToAnthropicWriter) handleOutputItemAdded(data []byte) error {
	oi := int(gjson.GetBytes(data, "output_index").Int())
	item := gjson.GetBytes(data, "item")
	if item.Get("type").String() != "function_call" {
		// message / reasoning blocks open lazily on their first delta so a
		// reasoning item that surfaces no summary text never opens an empty
		// thinking block.
		return nil
	}
	name := item.Get("name").String()
	if name == "" {
		// A nameless tool call would make the client invoke tool "" and loop.
		// Drop it: skip the block, drop later arg deltas + the done event. The
		// turn ends on its real stop_reason (reconciledStopReason demotes a
		// terminal tool_use claim with no surviving block to end_turn).
		t.suppressed[oi] = struct{}{}
		observability.Get().Error(
			"ResponsesToAnthropic dropping nameless function_call",
			"request_model", t.requestModel,
			"call_id", item.Get("call_id").String(),
		)
		return nil
	}
	idx := t.blockIdx
	t.itemBlocks[oi] = idx
	t.itemKind[oi] = "tool_use"
	t.toolUseCount++
	t.blockIdx++
	// Anthropic tool_use.id maps from call_id (not the fc_ item id).
	return t.emitContentBlockStartTool(idx, item.Get("call_id").String(), name)
}

func (t *ResponsesToAnthropicWriter) handleTextDelta(data []byte) error {
	idx, err := t.openBlock(int(gjson.GetBytes(data, "output_index").Int()), "text")
	if err != nil {
		return err
	}
	return t.emitContentBlockDeltaText(idx, gjson.GetBytes(data, "delta").String())
}

func (t *ResponsesToAnthropicWriter) handleReasoningDelta(data []byte) error {
	idx, err := t.openBlock(int(gjson.GetBytes(data, "output_index").Int()), "thinking")
	if err != nil {
		return err
	}
	return t.emitContentBlockDeltaThinking(idx, gjson.GetBytes(data, "delta").String())
}

// bufferToolArgs accumulates a tool call's arguments for an output_index.
// appendMode=true appends a streamed fragment from `field`; appendMode=false
// replaces the buffer with an authoritative complete value from `field`.
func (t *ResponsesToAnthropicWriter) bufferToolArgs(data []byte, field string, appendMode bool) {
	s := gjson.GetBytes(data, field).String()
	if s == "" && appendMode {
		return
	}
	oi := int(gjson.GetBytes(data, "output_index").Int())
	if _, dropped := t.suppressed[oi]; dropped {
		return
	}
	buf, ok := t.toolArgs[oi]
	if !ok {
		buf = &strings.Builder{}
		t.toolArgs[oi] = buf
	}
	if !appendMode {
		buf.Reset()
	}
	buf.WriteString(s)
}

func (t *ResponsesToAnthropicWriter) handleOutputItemDone(data []byte) error {
	oi := int(gjson.GetBytes(data, "output_index").Int())
	if _, dropped := t.suppressed[oi]; dropped {
		delete(t.suppressed, oi)
		delete(t.toolArgs, oi)
		return nil
	}
	item := gjson.GetBytes(data, "item")
	idx, ok := t.itemBlocks[oi]
	if !ok {
		// No delta opened a block for this item. Some upstreams send an item's
		// full content only on output_item.done (or output_item.added was lost);
		// without this a delta-less item would yield an empty assistant turn on
		// the streaming path (the non-streaming path already rebuilds from
		// response.completed). Synthesize the block from the terminal item.
		return t.emitDoneOnlyItem(oi, item)
	}
	if t.itemKind[oi] == "tool_use" {
		// item.arguments on the terminal event is the authoritative complete
		// args; use it when the streamed deltas were absent or malformed.
		if err := t.emitValidatedToolArgsDelta(oi, idx, item.Get("arguments").String()); err != nil {
			return err
		}
	}
	delete(t.itemBlocks, oi)
	delete(t.itemKind, oi)
	return t.emitContentBlockStop(idx)
}

// emitDoneOnlyItem synthesizes a content block from a completed output item that
// opened no block via streamed deltas — some upstreams deliver an item's full
// content only on output_item.done, or output_item.added was lost. Emitted as
// one start/delta/stop so a delta-less item still produces content.
func (t *ResponsesToAnthropicWriter) emitDoneOnlyItem(oi int, item gjson.Result) error {
	switch item.Get("type").String() {
	case "function_call":
		name := item.Get("name").String()
		if name == "" {
			delete(t.toolArgs, oi)
			return nil // nameless call → drop, as in handleOutputItemAdded
		}
		idx := t.blockIdx
		t.blockIdx++
		t.toolUseCount++
		if err := t.emitContentBlockStartTool(idx, item.Get("call_id").String(), name); err != nil {
			return err
		}
		if err := t.emitValidatedToolArgsDelta(oi, idx, item.Get("arguments").String()); err != nil {
			return err
		}
		return t.emitContentBlockStop(idx)
	case "message":
		var text strings.Builder
		item.Get("content").ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").String() == "output_text" {
				text.WriteString(part.Get("text").String())
			}
			return true
		})
		if text.Len() == 0 {
			return nil
		}
		idx := t.blockIdx
		t.blockIdx++
		if err := t.emitContentBlockStartText(idx); err != nil {
			return err
		}
		if err := t.emitContentBlockDeltaText(idx, text.String()); err != nil {
			return err
		}
		return t.emitContentBlockStop(idx)
	case "reasoning":
		text := joinReasoningSummary(item.Get("summary"))
		if text == "" {
			return nil
		}
		idx := t.blockIdx
		t.blockIdx++
		if err := t.emitContentBlockStartThinking(idx); err != nil {
			return err
		}
		if err := t.emitContentBlockDeltaThinking(idx, text); err != nil {
			return err
		}
		return t.emitContentBlockStop(idx)
	}
	return nil
}

// openBlock returns the Anthropic block index for output_index oi, opening a
// new block of the given kind (text/thinking) on first use.
func (t *ResponsesToAnthropicWriter) openBlock(oi int, kind string) (int, error) {
	if idx, ok := t.itemBlocks[oi]; ok {
		return idx, nil
	}
	idx := t.blockIdx
	t.itemBlocks[oi] = idx
	t.itemKind[oi] = kind
	t.blockIdx++
	if kind == "thinking" {
		return idx, t.emitContentBlockStartThinking(idx)
	}
	return idx, t.emitContentBlockStartText(idx)
}

// captureFinalResponse records usage + stop reason from a terminal event's
// nested `response` object.
func (t *ResponsesToAnthropicWriter) captureFinalResponse(data []byte) {
	resp := gjson.GetBytes(data, "response")
	if !resp.Exists() {
		return
	}
	hasToolCall := false
	resp.Get("output").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "function_call" {
			hasToolCall = true
			return false
		}
		return true
	})
	switch {
	case hasToolCall:
		t.finalStopReason = "tool_use"
	case resp.Get("incomplete_details.reason").String() == "max_output_tokens" || resp.Get("status").String() == "incomplete":
		t.finalStopReason = "max_tokens"
	default:
		t.finalStopReason = "end_turn"
	}
	usage := resp.Get("usage")
	if usage.Exists() {
		t.hasUsage = true
		t.usageInput = int(usage.Get("input_tokens").Int())
		t.usageOutput = int(usage.Get("output_tokens").Int())
		t.usageCacheRead = int(usage.Get("input_tokens_details.cached_tokens").Int())
		if t.usageSink != nil {
			t.usageSink.RecordUsage(t.usageInput, t.usageOutput)
			t.usageSink.RecordCacheUsage(0, t.usageCacheRead)
		}
	}
}

func (t *ResponsesToAnthropicWriter) finishStream() error {
	// Close any still-open blocks. Reaching here with one open means the stream
	// ended before its output_item.done (truncation); flush any buffered tool
	// args first so a partial tool call still delivers its input_json_delta.
	for oi, idx := range t.itemBlocks {
		if t.itemKind[oi] == "tool_use" {
			if err := t.emitValidatedToolArgsDelta(oi, idx, ""); err != nil {
				return err
			}
		}
		delete(t.itemBlocks, oi)
		delete(t.itemKind, oi)
		if err := t.emitContentBlockStop(idx); err != nil {
			return err
		}
	}
	t.emittedStopReason = t.reconciledStopReason()
	if err := t.emitMessageDelta(t.emittedStopReason); err != nil {
		return err
	}
	if err := t.emitMessageStop(); err != nil {
		return err
	}
	t.closed = true
	return nil
}

// reconciledStopReason enforces the Anthropic invariant that a turn with
// tool_use blocks reports stop_reason "tool_use" and one without never does —
// independent of the terminal Responses payload, which can be absent (truncated
// stream) or disagree with what was actually streamed. Mirrors the promotion /
// demotion in AnthropicSSETranslator.emitMessageDelta.
func (t *ResponsesToAnthropicWriter) reconciledStopReason() string {
	switch {
	case t.toolUseCount > 0:
		return "tool_use"
	case t.finalStopReason == "" || t.finalStopReason == "tool_use":
		// No terminal event, or it claimed tool_use with no block surviving.
		return "end_turn"
	default:
		return t.finalStopReason
	}
}

// --- non-streaming path ---

// finalizeBuffered renders the buffered Responses stream as a one-shot Anthropic
// JSON body for a non-streaming client. It extracts the final `response` object
// from the terminal SSE event and reuses ResponsesToAnthropicResponse.
func (t *ResponsesToAnthropicWriter) finalizeBuffered() error {
	if t.statusCode >= 400 {
		return t.finalizeError()
	}
	finalResp := extractFinalResponseObject(t.buf.Bytes())
	if finalResp == nil {
		observability.Get().Error("ResponsesToAnthropic: no terminal response event in stream")
		return t.finalizeError()
	}
	// A failed terminal response is an error, not an (empty) assistant turn —
	// surface it the way the streaming path does, rather than building success
	// JSON from a failed payload. `incomplete` (max_output_tokens) is a valid
	// truncated response and is left to ResponsesToAnthropicResponse.
	respErr := gjson.GetBytes(finalResp, "error")
	if gjson.GetBytes(finalResp, "status").String() == "failed" || (respErr.Exists() && respErr.Type != gjson.Null) {
		observability.Get().Error("ResponsesToAnthropic: upstream response failed", "request_model", t.requestModel)
		return t.finalizeError()
	}
	anthropic, err := ResponsesToAnthropicResponse(finalResp, t.requestModel)
	if err != nil {
		observability.Get().Error("ResponsesToAnthropic: translate failed", "err", err)
		return t.finalizeError()
	}
	root := gjson.ParseBytes(anthropic)
	t.recordBufferedUsage(root.Get("usage"))
	t.emittedStopReason = root.Get("stop_reason").String()
	t.usageOutput = int(root.Get("usage.output_tokens").Int())
	root.Get("content").ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").String() == "tool_use" {
			t.toolUseCount++
		}
		return true
	})

	t.inner.Header().Set("Content-Type", "application/json")
	t.inner.Header().Del("Content-Length")
	t.inner.WriteHeader(http.StatusOK)
	_, err = t.inner.Write(anthropic)
	return err
}

func (t *ResponsesToAnthropicWriter) recordBufferedUsage(usage gjson.Result) {
	if t.usageSink == nil || !usage.Exists() {
		return
	}
	t.usageSink.RecordUsage(int(usage.Get("input_tokens").Int()), int(usage.Get("output_tokens").Int()))
	t.usageSink.RecordCacheUsage(0, int(usage.Get("cache_read_input_tokens").Int()))
}

// finalizeError renders a one-shot Anthropic error body. Only reached on the
// non-streaming path; streaming errors are rendered by the dispatch's
// emitAnthropicSSEErrorEvent before Finalize and closed out by finishStream.
func (t *ResponsesToAnthropicWriter) finalizeError() error {
	errBody := t.anthropicErrorFromBuffer()
	if !t.headersEmitted {
		t.inner.Header().Set("Content-Type", "application/json")
		t.inner.Header().Del("Content-Length")
		code := t.statusCode
		if code < 400 {
			code = http.StatusBadGateway
		}
		t.inner.WriteHeader(code)
	}
	_, err := t.inner.Write(errBody)
	return err
}

// anthropicErrorFromBuffer builds an Anthropic error envelope from whatever the
// upstream left in buf. With stream:true the buffer is raw SSE, not a JSON error
// body, so feeding it straight to ResponsesToAnthropicError yields an empty
// message; instead scan the stream for a terminal `error` event or a failed
// response's error, falling back to a clear generic message.
func (t *ResponsesToAnthropicWriter) anthropicErrorFromBuffer() []byte {
	b := t.buf.Bytes()
	if gjson.ValidBytes(b) && gjson.GetBytes(b, "error").Exists() {
		return ResponsesToAnthropicError(b)
	}
	rest := b
	for {
		event, n := sse.SplitNext(rest)
		if n == 0 {
			break
		}
		rest = rest[n:]
		_, data := sse.ParseEvent(event)
		if len(data) == 0 {
			continue
		}
		switch gjson.GetBytes(data, "type").String() {
		case "error":
			return responsesError(gjson.GetBytes(data, "code").String(), gjson.GetBytes(data, "message").String())
		case "response.failed", "response.incomplete":
			if e := gjson.GetBytes(data, "response.error"); e.Exists() {
				return responsesError(e.Get("code").String(), e.Get("message").String())
			}
		}
	}
	return responsesError("api_error", "upstream Responses stream ended without a terminal response event")
}

// responsesError builds an Anthropic error envelope from a Responses-style
// type/message pair, routed through ResponsesToAnthropicError so the wire shape
// stays single-sourced.
func responsesError(errType, msg string) []byte {
	if errType == "" {
		errType = "api_error"
	}
	if msg == "" {
		msg = "upstream Responses request failed"
	}
	jw := newJSONWriter()
	jw.Obj()
	jw.Key("error")
	jw.Obj()
	jw.Key("type")
	jw.Str(errType)
	jw.Key("message")
	jw.Str(msg)
	jw.EndObj()
	jw.EndObj()
	return ResponsesToAnthropicError(jw.Bytes())
}

// extractFinalResponseObject scans a buffered Responses SSE stream for the last
// terminal event (response.completed/.incomplete/.failed) and returns its
// nested `response` object as raw JSON, or nil if none is present.
func extractFinalResponseObject(sseBytes []byte) []byte {
	var out []byte
	rest := sseBytes
	for {
		event, n := sse.SplitNext(rest)
		if n == 0 {
			break
		}
		rest = rest[n:]
		_, data := sse.ParseEvent(event)
		if len(data) == 0 {
			continue
		}
		switch gjson.GetBytes(data, "type").String() {
		case "response.completed", "response.incomplete", "response.failed":
			if resp := gjson.GetBytes(data, "response"); resp.Exists() {
				out = []byte(resp.Raw)
			}
		}
	}
	return out
}

// --- Anthropic SSE frame emitters (wire shapes mirror AnthropicSSETranslator) ---

func (t *ResponsesToAnthropicWriter) emitMessageStart() error {
	model := t.requestModel
	t.bw.WriteString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":")
	sse.WriteJSONString(t.bw, "msg_responses")
	t.bw.WriteString(",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":")
	sse.WriteJSONString(t.bw, model)
	t.bw.WriteString(",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":")
	sse.WriteJSONInt(t.bw, int64(t.estimatedInputTokens))
	t.bw.WriteString(",\"output_tokens\":0}}}\n\n")
	return t.flushEvent()
}

func (t *ResponsesToAnthropicWriter) emitRoutingMarkerIfConfigured() error {
	if t.routingMarker == "" {
		return nil
	}
	idx := t.blockIdx
	if err := t.emitContentBlockStartText(idx); err != nil {
		return err
	}
	if err := t.emitContentBlockDeltaText(idx, t.routingMarker); err != nil {
		return err
	}
	if err := t.emitContentBlockStop(idx); err != nil {
		return err
	}
	t.blockIdx++
	return nil
}

func (t *ResponsesToAnthropicWriter) emitContentBlockStartText(index int) error {
	t.bw.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString(",\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	return t.flushEvent()
}

func (t *ResponsesToAnthropicWriter) emitContentBlockStartThinking(index int) error {
	t.bw.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString(",\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n")
	return t.flushEvent()
}

func (t *ResponsesToAnthropicWriter) emitContentBlockStartTool(index int, id, name string) error {
	t.bw.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString(",\"content_block\":{\"type\":\"tool_use\",\"id\":")
	sse.WriteJSONString(t.bw, id)
	t.bw.WriteString(",\"name\":")
	sse.WriteJSONString(t.bw, name)
	t.bw.WriteString(",\"input\":{}}}\n\n")
	return t.flushEvent()
}

func (t *ResponsesToAnthropicWriter) emitContentBlockDeltaText(index int, text string) error {
	if text == "" {
		return nil
	}
	t.bw.WriteString("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString(",\"delta\":{\"type\":\"text_delta\",\"text\":")
	sse.WriteJSONString(t.bw, text)
	t.bw.WriteString("}}\n\n")
	return t.flushEvent()
}

func (t *ResponsesToAnthropicWriter) emitContentBlockDeltaThinking(index int, text string) error {
	if text == "" {
		return nil
	}
	t.bw.WriteString("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString(",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":")
	sse.WriteJSONString(t.bw, text)
	t.bw.WriteString("}}\n\n")
	return t.flushEvent()
}

// emitValidatedToolArgsDelta emits a single input_json_delta for a tool block,
// carrying the buffered arguments. When those are empty or fail to parse it
// falls back to fallback (the authoritative `arguments` from the terminal
// item), and only as a last resort `{}` — so a malformed concatenation or a
// lost delta stream can't break the client's tool-args parser.
func (t *ResponsesToAnthropicWriter) emitValidatedToolArgsDelta(oi, index int, fallback string) error {
	buf, ok := t.toolArgs[oi]
	delete(t.toolArgs, oi)
	buffered := ""
	if ok {
		buffered = buf.String()
	}
	args := "{}"
	switch {
	case buffered != "" && gjson.Valid(buffered):
		args = buffered
	case fallback != "" && gjson.Valid(fallback):
		args = fallback
	case buffered != "" || fallback != "":
		bad := buffered
		if bad == "" {
			bad = fallback
		}
		const previewMax = 200
		preview := bad
		if len(preview) > previewMax {
			preview = preview[:previewMax]
		}
		observability.Get().Error(
			"ResponsesToAnthropic tool_use args failed JSON validation — substituting empty args",
			"block_index", index,
			"request_model", t.requestModel,
			"buffered_len", len(buffered),
			"fallback_len", len(fallback),
			"args_preview", preview,
		)
	}
	t.bw.WriteString("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString(",\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":")
	sse.WriteJSONString(t.bw, args)
	t.bw.WriteString("}}\n\n")
	return t.flushEvent()
}

func (t *ResponsesToAnthropicWriter) emitContentBlockStop(index int) error {
	t.bw.WriteString("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString("}\n\n")
	return t.flushEvent()
}

func (t *ResponsesToAnthropicWriter) emitMessageDelta(stopReason string) error {
	t.bw.WriteString("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":")
	sse.WriteJSONString(t.bw, stopReason)
	t.bw.WriteString(",\"stop_sequence\":null},\"usage\":{")
	if t.hasUsage {
		// Anthropic's input_tokens is fresh-only; subtract cached reads so the
		// statusline formula doesn't double-count.
		freshInput := max(0, t.usageInput-t.usageCacheRead)
		t.bw.WriteString("\"input_tokens\":")
		sse.WriteJSONInt(t.bw, int64(freshInput))
		t.bw.WriteString(",\"output_tokens\":")
		sse.WriteJSONInt(t.bw, int64(t.usageOutput))
		if t.usageCacheRead > 0 {
			t.bw.WriteString(",\"cache_read_input_tokens\":")
			sse.WriteJSONInt(t.bw, int64(t.usageCacheRead))
		}
	} else {
		t.bw.WriteString("\"output_tokens\":0")
	}
	t.bw.WriteString("}}\n\n")
	return t.flushEvent()
}

func (t *ResponsesToAnthropicWriter) emitMessageStop() error {
	t.bw.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	return t.flushEvent()
}

// emitStreamErrorEvent writes an Anthropic `event: error` frame for a
// stream-level failure (an `error` or `response.failed` event over HTTP 200) and
// marks the stream closed so finishStream does not also emit a success trailer.
func (t *ResponsesToAnthropicWriter) emitStreamErrorEvent(errType, msg string) error {
	t.bw.WriteString("event: error\ndata: ")
	t.bw.Write(responsesError(errType, msg))
	t.bw.WriteString("\n\n")
	if err := t.flushEvent(); err != nil {
		return err
	}
	t.closed = true
	return nil
}

func (t *ResponsesToAnthropicWriter) flushEvent() error {
	if err := t.bw.Flush(); err != nil {
		return err
	}
	if t.flusher != nil {
		t.flusher.Flush()
	}
	return nil
}

var _ http.ResponseWriter = (*ResponsesToAnthropicWriter)(nil)
var _ http.Flusher = (*ResponsesToAnthropicWriter)(nil)
