package translate

import (
	"bufio"
	"net/http"

	"workweave/router/internal/observability"
	"workweave/router/internal/observability/otel"
	"workweave/router/internal/sse"

	"github.com/tidwall/gjson"
)

// ResponsesToAnthropicWriter adapts a NON-streaming OpenAI Responses upstream
// response into an Anthropic Messages response for the client. It buffers the
// upstream JSON, translates it with ResponsesToAnthropicResponse, then either
// writes a one-shot Anthropic JSON body (non-streaming client) or replays it as
// a synthetic Anthropic SSE sequence (streaming client). It exposes the same
// Prelude/Write/Finalize/Summary surface the proxy's OpenAI dispatch expects.
type ResponsesToAnthropicWriter struct {
	inner        http.ResponseWriter
	flusher      http.Flusher
	bw           *bufio.Writer
	requestModel string
	usageSink    otel.UsageSink

	routingMarker        string
	estimatedInputTokens int
	requestHadTools      bool

	buf            []byte
	statusCode     int
	streaming      bool
	headersEmitted bool
	started        bool

	// summary fields
	emittedStopReason string
	toolUseCount      int
	outputTokens      int
}

// NewResponsesToAnthropicWriter wraps w to translate a buffered Responses
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

// WriteHeader captures the upstream status. The Responses upstream is always
// non-streaming JSON, so we never switch to SSE here — the streaming decision
// is the CLIENT's, committed in Prelude.
func (t *ResponsesToAnthropicWriter) WriteHeader(code int) {
	if t.headersEmitted {
		return
	}
	t.statusCode = code
}

func (t *ResponsesToAnthropicWriter) Write(data []byte) (int, error) {
	t.buf = append(t.buf, data...)
	return len(data), nil
}

func (t *ResponsesToAnthropicWriter) Flush() {
	if t.streaming && t.flusher != nil {
		t.flusher.Flush()
	}
}

// Prelude commits SSE headers + message_start eagerly when the client requested
// streaming, mirroring AnthropicSSETranslator so the dispatch closure is
// identical. The content blocks are emitted later in Finalize once the buffered
// upstream body is translated.
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
	if err := t.emitMessageStartFrame(t.requestModel, "msg_responses", t.estimatedInputTokens); err != nil {
		return err
	}
	return t.emitRoutingMarkerFrame()
}

// Finalize translates the buffered Responses body and emits it: SSE replay for
// a streaming client, one-shot JSON otherwise.
func (t *ResponsesToAnthropicWriter) Finalize() error {
	if t.statusCode >= 400 {
		return t.finalizeError()
	}
	anthropic, err := ResponsesToAnthropicResponse(t.buf, t.requestModel)
	if err != nil {
		observability.Get().Error("ResponsesToAnthropic: translate failed", "err", err)
		return t.finalizeError()
	}
	root := gjson.ParseBytes(anthropic)
	t.recordUsage(root.Get("usage"))
	t.emittedStopReason = root.Get("stop_reason").String()
	t.outputTokens = int(root.Get("usage.output_tokens").Int())

	if !t.streaming {
		t.inner.Header().Set("Content-Type", "application/json")
		t.inner.Header().Del("Content-Length")
		t.inner.WriteHeader(http.StatusOK)
		_, err := t.inner.Write(anthropic)
		return err
	}
	return t.replayAsSSE(root)
}

func (t *ResponsesToAnthropicWriter) Summary() ResponseSummary {
	return ResponseSummary{
		StopReason:    t.emittedStopReason,
		ToolUseBlocks: t.toolUseCount,
		OutputTokens:  t.outputTokens,
	}
}

func (t *ResponsesToAnthropicWriter) recordUsage(usage gjson.Result) {
	if t.usageSink == nil || !usage.Exists() {
		return
	}
	in := int(usage.Get("input_tokens").Int())
	out := int(usage.Get("output_tokens").Int())
	cacheRead := int(usage.Get("cache_read_input_tokens").Int())
	t.usageSink.RecordUsage(in, out)
	t.usageSink.RecordCacheUsage(0, cacheRead)
}

// replayAsSSE emits the translated Anthropic message as an SSE sequence.
// message_start (+ routing marker) was already emitted by Prelude; here we emit
// the content blocks starting at the next index, then message_delta + stop.
func (t *ResponsesToAnthropicWriter) replayAsSSE(msg gjson.Result) error {
	idx := 0
	if t.routingMarker != "" {
		idx = 1 // routing marker occupies block 0
	}
	var emitErr error
	msg.Get("content").ForEach(func(_, block gjson.Result) bool {
		switch block.Get("type").String() {
		case "thinking":
			emitErr = t.emitThinkingBlock(idx, block.Get("thinking").String())
		case "text":
			emitErr = t.emitTextBlock(idx, block.Get("text").String())
		case "tool_use":
			t.toolUseCount++
			emitErr = t.emitToolUseBlock(idx, block.Get("id").String(), block.Get("name").String(), block.Get("input").Raw)
		default:
			return true
		}
		idx++
		return emitErr == nil
	})
	if emitErr != nil {
		return emitErr
	}
	if err := t.emitMessageDeltaFrame(msg.Get("stop_reason").String(), t.outputTokens); err != nil {
		return err
	}
	return t.emitMessageStopFrame()
}

// --- SSE frame emitters (self-contained; mirror AnthropicSSETranslator wire shapes) ---

func (t *ResponsesToAnthropicWriter) flushEvent() error {
	if err := t.bw.Flush(); err != nil {
		return err
	}
	if t.flusher != nil {
		t.flusher.Flush()
	}
	return nil
}

func (t *ResponsesToAnthropicWriter) emitMessageStartFrame(model, id string, inputTokens int) error {
	t.bw.WriteString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":")
	sse.WriteJSONString(t.bw, id)
	t.bw.WriteString(",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":")
	sse.WriteJSONString(t.bw, model)
	t.bw.WriteString(",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":")
	sse.WriteJSONInt(t.bw, int64(inputTokens))
	t.bw.WriteString(",\"output_tokens\":0}}}\n\n")
	return t.flushEvent()
}

func (t *ResponsesToAnthropicWriter) emitRoutingMarkerFrame() error {
	if t.routingMarker == "" {
		return nil
	}
	if err := t.emitTextBlock(0, t.routingMarker); err != nil {
		return err
	}
	return nil
}

func (t *ResponsesToAnthropicWriter) emitTextBlock(index int, text string) error {
	t.bw.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString(",\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	t.bw.WriteString("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString(",\"delta\":{\"type\":\"text_delta\",\"text\":")
	sse.WriteJSONString(t.bw, text)
	t.bw.WriteString("}}\n\n")
	return t.emitBlockStop(index)
}

func (t *ResponsesToAnthropicWriter) emitThinkingBlock(index int, text string) error {
	t.bw.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString(",\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n")
	t.bw.WriteString("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString(",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":")
	sse.WriteJSONString(t.bw, text)
	t.bw.WriteString("}}\n\n")
	return t.emitBlockStop(index)
}

func (t *ResponsesToAnthropicWriter) emitToolUseBlock(index int, id, name, inputRaw string) error {
	if inputRaw == "" {
		inputRaw = "{}"
	}
	t.bw.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString(",\"content_block\":{\"type\":\"tool_use\",\"id\":")
	sse.WriteJSONString(t.bw, id)
	t.bw.WriteString(",\"name\":")
	sse.WriteJSONString(t.bw, name)
	t.bw.WriteString(",\"input\":{}}}\n\n")
	t.bw.WriteString("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString(",\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":")
	sse.WriteJSONString(t.bw, inputRaw)
	t.bw.WriteString("}}\n\n")
	return t.emitBlockStop(index)
}

func (t *ResponsesToAnthropicWriter) emitBlockStop(index int) error {
	t.bw.WriteString("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString("}\n\n")
	return t.flushEvent()
}

func (t *ResponsesToAnthropicWriter) emitMessageDeltaFrame(stopReason string, outputTokens int) error {
	if stopReason == "" {
		stopReason = "end_turn"
	}
	t.bw.WriteString("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":")
	sse.WriteJSONString(t.bw, stopReason)
	t.bw.WriteString(",\"stop_sequence\":null},\"usage\":{\"output_tokens\":")
	sse.WriteJSONInt(t.bw, int64(outputTokens))
	t.bw.WriteString("}}\n\n")
	return t.flushEvent()
}

func (t *ResponsesToAnthropicWriter) emitMessageStopFrame() error {
	t.bw.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	return t.flushEvent()
}

func (t *ResponsesToAnthropicWriter) finalizeError() error {
	errBody := ResponsesToAnthropicError(t.buf)
	if t.streaming {
		t.bw.WriteString("event: error\ndata: ")
		t.bw.Write(errBody)
		t.bw.WriteString("\n\n")
		return t.flushEvent()
	}
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
