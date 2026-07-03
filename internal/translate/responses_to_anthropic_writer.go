package translate

import (
	"bufio"
	"bytes"
	"net/http"
	"strings"

	"workweave/router/internal/observability"
	"workweave/router/internal/providers"
	"workweave/router/internal/sse"
	"workweave/router/internal/translate/toolcheck"

	"github.com/tidwall/gjson"
)

var _ providers.OutputProgressArmer = (*ResponsesToAnthropicWriter)(nil)

// ResponsesToAnthropicWriter adapts a streaming OpenAI Responses upstream
// (`POST /v1/responses` with `stream:true`) into an Anthropic Messages
// response. Streaming clients get incremental Anthropic SSE (reasoning →
// thinking, output text → text, function_call → tool_use); non-streaming
// clients get a one-shot JSON body built at Finalize from the terminal
// `response.completed` event. Exposes the same Prelude/Write/Finalize/Summary
// surface as AnthropicSSETranslator so the dispatch closure is identical.
type ResponsesToAnthropicWriter struct {
	inner        http.ResponseWriter
	flusher      http.Flusher
	bw           *bufio.Writer
	requestModel string
	usageSink    UsageSink
	// messageID: generated fresh per response since Prelude fires message_start
	// before upstream produces a `resp_...` id (no passthrough possible). Must
	// be unique — clients like ccusage dedupe usage records by message id, so a
	// constant id would collapse a session's turns into one undercounted record.
	messageID string

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

	// onOutputProgress, set via ArmOutputProgress, fires on output-bearing events
	// only (never reasoning/keepalives) to feed the watchdog that aborts a
	// stream staying byte-alive with zero output (DefaultResponsesOutputStallTimeout).
	onOutputProgress func()

	// blockIdx is the next Anthropic content-block index to assign.
	blockIdx int
	// itemBlocks maps an output_index to its Anthropic content block index, so
	// deltas stay correlated to the right block regardless of event ordering.
	itemBlocks map[int]int
	// itemKind records the Anthropic block kind per output_index so the matching
	// output_item.done can close it correctly.
	itemKind map[int]string
	// toolArgs accumulates function_call_arguments deltas per output_index,
	// emitted as one input_json_delta at item close; unparseable becomes `{}`
	// (mirrors AnthropicSSETranslator) rather than breaking the client's parser.
	toolArgs                  map[int]*strings.Builder
	reasoningSignatures       map[int]string
	pendingReasoningSignature string
	// suppressed holds output_indexes of nameless function_call items — a nameless
	// tool_use makes the client invoke tool "" in a loop, so we drop the block and
	// its later arg deltas/done event (mirrors AnthropicSSETranslator).
	suppressed map[int]struct{}

	// toolName records the function_call name per output_index so its arguments
	// can be validated against that tool's schema at emit time.
	toolName map[int]string
	// toolValidator validates/repairs tool args against the request's schemas
	// (see toolcheck); nil means syntax-check-only (no tools in the request).
	toolValidator *toolcheck.Validator
	// toolCallIssues collects every validation/repair finding so the proxy
	// can emit per-block router.tool_call_invalid telemetry from Summary.
	toolCallIssues []toolcheck.Issue

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
func NewResponsesToAnthropicWriter(w http.ResponseWriter, requestModel string, sink UsageSink) *ResponsesToAnthropicWriter {
	flusher, _ := w.(http.Flusher)
	return &ResponsesToAnthropicWriter{
		inner:               w,
		flusher:             flusher,
		bw:                  bufio.NewWriterSize(w, 8192),
		requestModel:        requestModel,
		usageSink:           sink,
		messageID:           "msg_responses_" + randomHex(8),
		statusCode:          http.StatusOK,
		itemBlocks:          make(map[int]int),
		itemKind:            make(map[int]string),
		toolArgs:            make(map[int]*strings.Builder),
		reasoningSignatures: make(map[int]string),
		suppressed:          make(map[int]struct{}),
		toolName:            make(map[int]string),
	}
}

// WithToolValidator installs the request's compiled tool-schema validator so
// emitted tool args are validated and repaired before reaching the client.
func (t *ResponsesToAnthropicWriter) WithToolValidator(v *toolcheck.Validator) *ResponsesToAnthropicWriter {
	t.toolValidator = v
	return t
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

// ArmOutputProgress installs mark, called on output-bearing events (never
// reasoning deltas/keepalives) so the watchdog tracks time-since-last-output
// rather than time-since-last-byte. Returns false for non-streaming clients,
// whose buffered path only parses events at Finalize and would false-trip.
// Call after Prelude, which sets the streaming flag.
func (t *ResponsesToAnthropicWriter) ArmOutputProgress(mark func()) (armed bool) {
	if !t.streaming {
		return false
	}
	t.onOutputProgress = mark
	return true
}

func (t *ResponsesToAnthropicWriter) markOutputProgress() {
	if t.onOutputProgress != nil {
		t.onOutputProgress()
	}
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

// Write receives upstream Responses bytes: streams parse and translate SSE
// events on the fly; non-streaming buffers the raw stream for Finalize.
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

// Prelude eagerly commits SSE headers + message_start (+ routing marker) for
// streaming clients, so the envelope arrives while upstream is still reasoning.
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
		StopReason:      t.emittedStopReason,
		ToolUseBlocks:   t.toolUseCount,
		ToolCallIssues:  t.toolCallIssues,
		OutputTokens:    t.usageOutput,
		InputTokens:     t.usageInput,
		CacheReadTokens: t.usageCacheRead,
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
	// Match on the in-payload `type`, not `event:` — intermediaries sometimes
	// drop the latter. markOutputProgress is deliberately skipped for reasoning
	// deltas/items and unknown frames, so a reasoning-only/keepalive-only stream
	// can still trip the watchdog; output_item.added/done gate on item.type
	// since those fire for reasoning items too.
	switch gjson.GetBytes(data, "type").String() {
	case "response.output_item.added":
		if gjson.GetBytes(data, "item.type").String() != "reasoning" {
			t.markOutputProgress()
		}
		return t.handleOutputItemAdded(data)
	case "response.output_text.delta":
		t.markOutputProgress()
		return t.handleTextDelta(data)
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		return t.handleReasoningDelta(data)
	case "response.function_call_arguments.delta":
		t.markOutputProgress()
		t.bufferToolArgs(data, "delta", true)
		return nil
	case "response.function_call_arguments.done":
		// Terminal event carries the complete arguments; adopt it so a call whose
		// deltas were absent/lost still emits real params instead of `{}`.
		t.markOutputProgress()
		t.bufferToolArgs(data, "arguments", false)
		return nil
	case "response.output_item.done":
		if gjson.GetBytes(data, "item.type").String() != "reasoning" {
			t.markOutputProgress()
		}
		return t.handleOutputItemDone(data)
	case "error":
		// Stream-level failure over HTTP 200 — surface it as an Anthropic error
		// event instead of closing the turn as if it succeeded.
		return t.emitStreamErrorEvent(gjson.GetBytes(data, "code").String(), gjson.GetBytes(data, "message").String())
	case "response.failed":
		// Always an upstream failure; responsesError fills a generic message if
		// no error object rode along, matching the buffered path's rejection.
		resp := gjson.GetBytes(data, "response")
		errType, msg := responsesFailureFromResponse(resp)
		return t.emitStreamErrorEvent(errType, msg)
	case "response.completed", "response.incomplete":
		// Terminal envelope counts as progress; a post-trip cancel is moot anyway.
		t.markOutputProgress()
		t.captureFinalResponse(data)
		return nil
	}
	return nil
}

func (t *ResponsesToAnthropicWriter) handleOutputItemAdded(data []byte) error {
	oi := int(gjson.GetBytes(data, "output_index").Int())
	item := gjson.GetBytes(data, "item")
	if item.Get("type").String() != "function_call" {
		if item.Get("type").String() == "reasoning" {
			t.captureReasoningSignature(oi, item)
		}
		return nil
	}
	name := item.Get("name").String()
	if name == "" {
		// Nameless call would make the client invoke tool "" and loop; drop it.
		// reconciledStopReason demotes a terminal tool_use claim with no
		// surviving block to end_turn.
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
	t.toolName[oi] = name
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

// bufferToolArgs accumulates a tool call's arguments. appendMode=true appends
// a streamed fragment; false replaces the buffer with a complete value.
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
	if item.Get("type").String() == "reasoning" {
		t.captureReasoningSignature(oi, item)
	}
	idx, ok := t.itemBlocks[oi]
	if !ok {
		// No delta opened a block: some upstreams send full content only on
		// output_item.done (or output_item.added was lost). Synthesize the block
		// from the terminal item instead of emitting an empty assistant turn.
		return t.emitDoneOnlyItem(oi, item)
	}
	switch t.itemKind[oi] {
	case "tool_use":
		// item.arguments is authoritative; used when streamed deltas were
		// absent or malformed.
		if err := t.emitValidatedToolArgsDelta(oi, idx, item.Get("arguments").String()); err != nil {
			return err
		}
	case "thinking":
		if err := t.emitReasoningSignatureDelta(oi, idx); err != nil {
			return err
		}
	}
	delete(t.itemBlocks, oi)
	delete(t.itemKind, oi)
	return t.emitContentBlockStop(idx)
}

// emitDoneOnlyItem synthesizes a start/delta/stop block for an item that never
// opened one via streamed deltas (full content arrived only on done, or added
// was lost), so a delta-less item still produces content.
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
		t.toolName[oi] = name
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
		t.captureReasoningSignature(oi, item)
		text := joinReasoningSummary(item.Get("summary"))
		if text == "" && t.reasoningSignatures[oi] == "" {
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
		if err := t.emitReasoningSignatureDelta(oi, idx); err != nil {
			return err
		}
		return t.emitContentBlockStop(idx)
	}
	return nil
}

func (t *ResponsesToAnthropicWriter) captureReasoningSignature(oi int, item gjson.Result) {
	if sig := encodeOpenAIReasoningSignature(item.Get("id").String(), item.Get("encrypted_content").String()); sig != "" {
		t.reasoningSignatures[oi] = sig
	}
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
	outputIndex := 0
	resp.Get("output").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "reasoning" {
			t.captureReasoningSignature(outputIndex, item)
		}
		if item.Get("type").String() == "function_call" {
			hasToolCall = true
			return false
		}
		outputIndex++
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
	// A still-open block here means the stream ended before output_item.done
	// (truncation); flush buffered tool args so it still delivers input_json_delta.
	for oi, idx := range t.itemBlocks {
		switch t.itemKind[oi] {
		case "tool_use":
			if err := t.emitValidatedToolArgsDelta(oi, idx, ""); err != nil {
				return err
			}
		case "thinking":
			if err := t.emitReasoningSignatureDelta(oi, idx); err != nil {
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

// reconciledStopReason enforces that a turn with tool_use blocks reports
// stop_reason "tool_use" and one without never does, independent of the
// terminal Responses payload (which can be absent or disagree with what
// actually streamed). Mirrors AnthropicSSETranslator.emitMessageDelta.
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

// finalizeBuffered renders the buffered stream as a one-shot Anthropic JSON
// body, extracting the final `response` object and reusing ResponsesToAnthropicResponse.
func (t *ResponsesToAnthropicWriter) finalizeBuffered() error {
	if t.statusCode >= 400 {
		return t.finalizeError()
	}
	finalResp := extractFinalResponseObject(t.buf.Bytes())
	if finalResp == nil {
		observability.Get().Error("ResponsesToAnthropic: no terminal response event in stream")
		return t.finalizeError()
	}
	// A failed terminal response is an error, not an empty assistant turn.
	// `incomplete` (max_output_tokens) is a valid truncated response, left to
	// ResponsesToAnthropicResponse.
	respErr := gjson.GetBytes(finalResp, "error")
	if gjson.GetBytes(finalResp, "status").String() == "failed" || (respErr.Exists() && respErr.Type != gjson.Null) {
		errType, errMsg := responsesFailureFromResponse(gjson.ParseBytes(finalResp))
		observability.Get().Error("ResponsesToAnthropic: upstream response failed",
			"request_model", t.requestModel,
			"upstream_status", gjson.GetBytes(finalResp, "status").String(),
			"upstream_error_type", errType,
			"upstream_error_message", errMsg,
		)
		return t.finalizeError()
	}
	anthropic, issues, err := responsesToAnthropicResponse(finalResp, t.requestModel, t.toolValidator)
	t.toolCallIssues = append(t.toolCallIssues, issues...)
	if err != nil {
		observability.Get().Error("ResponsesToAnthropic: translate failed", "err", err)
		return t.finalizeError()
	}
	root := gjson.ParseBytes(anthropic)
	t.recordBufferedUsage(root.Get("usage"))
	t.emittedStopReason = root.Get("stop_reason").String()
	t.captureBufferedUsage(root.Get("usage"))
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

func (t *ResponsesToAnthropicWriter) captureBufferedUsage(usage gjson.Result) {
	if !usage.Exists() {
		return
	}
	t.hasUsage = true
	t.usageInput = int(usage.Get("input_tokens").Int())
	t.usageOutput = int(usage.Get("output_tokens").Int())
	t.usageCacheRead = int(usage.Get("cache_read_input_tokens").Int())
}

func (t *ResponsesToAnthropicWriter) recordBufferedUsage(usage gjson.Result) {
	if t.usageSink == nil || !usage.Exists() {
		return
	}
	t.usageSink.RecordUsage(int(usage.Get("input_tokens").Int()), int(usage.Get("output_tokens").Int()))
	t.usageSink.RecordCacheUsage(0, int(usage.Get("cache_read_input_tokens").Int()))
}

// finalizeError renders a one-shot Anthropic error body. Streaming errors are
// instead rendered by the dispatch's emitAnthropicSSEErrorEvent before Finalize.
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

// anthropicErrorFromBuffer builds an error envelope from buf. With stream:true
// buf is raw SSE, not a JSON error body, so this scans for a terminal `error`
// event or failed response's error instead of feeding it directly to
// ResponsesToAnthropicError (which would yield an empty message).
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
		case "response.failed":
			resp := gjson.GetBytes(data, "response")
			errType, msg := responsesFailureFromResponse(resp)
			return responsesError(errType, msg)
		case "response.incomplete":
			// Only an error if it carries an error object; a plain
			// max_output_tokens incomplete is valid and must not stop the scan.
			resp := gjson.GetBytes(data, "response")
			if e := resp.Get("error"); e.Exists() && e.Type != gjson.Null {
				errType, msg := responsesFailureFromResponse(resp)
				return responsesError(errType, msg)
			}
		}
	}
	return responsesError("api_error", "upstream Responses stream ended without a terminal response event")
}

// responsesFailureFromResponse extracts an error type/message from a
// response.failed terminal event's `response` object. A parsed error type is
// kept even if the message is empty and a fallback is used.
func responsesFailureFromResponse(resp gjson.Result) (errType, msg string) {
	if !resp.Exists() {
		return "", ""
	}
	if e := resp.Get("error"); e.Exists() && e.Type != gjson.Null {
		errType = e.Get("code").String()
		if errType == "" {
			errType = e.Get("type").String()
		}
		if msg = e.Get("message").String(); msg != "" {
			return errType, msg
		}
	}
	if reason := resp.Get("incomplete_details.reason").String(); reason != "" {
		return errType, "upstream Responses request incomplete: " + reason
	}
	if resp.Get("status").String() == "failed" {
		return errType, "upstream Responses request failed (status: failed)"
	}
	return errType, ""
}

// responsesError builds an Anthropic error envelope from a Responses-style
// type/message pair via ResponsesToAnthropicError, keeping the wire shape single-sourced.
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

// extractFinalResponseObject scans a buffered SSE stream for the last terminal
// event and returns its nested `response` object as raw JSON, or nil.
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
	sse.WriteJSONString(t.bw, t.messageID)
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

func (t *ResponsesToAnthropicWriter) toolIDWithReasoningSignature(id string) string {
	if t.pendingReasoningSignature == "" {
		return id
	}
	sig := t.pendingReasoningSignature
	t.pendingReasoningSignature = ""
	return embedOpenAIReasoningSignatureInID(id, sig)
}

func (t *ResponsesToAnthropicWriter) emitContentBlockStartTool(index int, id, name string) error {
	id = t.toolIDWithReasoningSignature(id)
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

func (t *ResponsesToAnthropicWriter) emitReasoningSignatureDelta(oi, index int) error {
	sig := t.reasoningSignatures[oi]
	delete(t.reasoningSignatures, oi)
	if sig == "" {
		return nil
	}
	t.pendingReasoningSignature = sig
	t.bw.WriteString("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString(",\"delta\":{\"type\":\"signature_delta\",\"signature\":")
	sse.WriteJSONString(t.bw, sig)
	t.bw.WriteString("}}\n\n")
	return t.flushEvent()
}

// emitValidatedToolArgsDelta emits one input_json_delta for a tool block after
// toolcheck validation/repair. Falls back to the terminal item's authoritative
// `arguments` if the buffered concatenation is empty/unparseable, so a lost
// delta stream can't break the client's parser; unrepairable args degrade to
// `{}` and unrepairable schema mismatches are forwarded as-is so the client's
// own tool error surfaces.
func (t *ResponsesToAnthropicWriter) emitValidatedToolArgsDelta(oi, index int, fallback string) error {
	buf, ok := t.toolArgs[oi]
	delete(t.toolArgs, oi)
	buffered := ""
	if ok {
		buffered = buf.String()
	}
	raw := buffered
	if raw == "" || (!gjson.Valid(raw) && fallback != "" && gjson.Valid(fallback)) {
		raw = fallback
	}
	verdict := t.toolValidator.Check(t.toolName[oi], raw)
	if verdict.Issue != nil {
		t.toolCallIssues = append(t.toolCallIssues, *verdict.Issue)
		if verdict.Issue.Bucket == toolcheck.BucketInvalidJSON && !verdict.Issue.Repaired {
			const previewMax = 200
			preview := raw
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
	}
	args := verdict.Args
	delete(t.toolName, oi)
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

// emitStreamErrorEvent writes an `event: error` frame for a stream-level
// failure and marks the stream closed so finishStream skips the success trailer.
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
	return sse.FlushWriter(t.bw, t.flusher)
}

var _ http.ResponseWriter = (*ResponsesToAnthropicWriter)(nil)
var _ http.Flusher = (*ResponsesToAnthropicWriter)(nil)
