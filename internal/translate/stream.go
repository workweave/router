package translate

import (
	"bufio"
	"bytes"
	"net/http"
	"os"
	"strings"
	"time"

	"workweave/router/internal/observability"
	"workweave/router/internal/observability/otel"
	"workweave/router/internal/sse"

	"github.com/tidwall/gjson"
)

// sseTraceEnabled enables verbose SSE translation logging; read once at init
// to avoid hot-path os.Getenv.
var sseTraceEnabled = os.Getenv("ROUTER_DEBUG_SSE_TRACE") == "true"

// SSETranslator translates Anthropic streaming SSE to OpenAI
// chat.completion.chunk on the fly. Non-streaming responses buffer for Finalize.
type SSETranslator struct {
	inner   http.ResponseWriter
	flusher http.Flusher
	bw      *bufio.Writer

	streaming  bool
	statusCode int
	buf        bytes.Buffer

	msgID   string
	model   string
	created int64
	// toolIdx advances on content_block_stop for tool_use blocks so the start
	// chunk and all input_json_deltas share the same index.
	toolIdx       int
	currentIsTool bool

	usageSink otel.UsageSink
}

// NewSSETranslator wraps w. Call Finalize after upstream returns.
func NewSSETranslator(w http.ResponseWriter, model string, sink otel.UsageSink) *SSETranslator {
	flusher, _ := w.(http.Flusher)
	return &SSETranslator{
		inner:     w,
		flusher:   flusher,
		bw:        bufio.NewWriterSize(w, 8192),
		model:     model,
		created:   time.Now().Unix(),
		usageSink: sink,
	}
}

func (t *SSETranslator) Header() http.Header {
	return t.inner.Header()
}

// WriteHeader routes streaming success responses through SSE; errors and
// non-streaming defer to Finalize.
func (t *SSETranslator) WriteHeader(code int) {
	t.statusCode = code
	ct := t.inner.Header().Get("Content-Type")
	t.streaming = strings.Contains(ct, "text/event-stream") && code < 400

	// Stale once we re-encode the body to a different size.
	t.inner.Header().Del("Content-Length")
	t.inner.Header().Del("Content-Encoding")

	if t.streaming {
		t.inner.Header().Set("Content-Type", "text/event-stream")
		t.inner.WriteHeader(code)
	}
}

func (t *SSETranslator) Write(data []byte) (int, error) {
	n := len(data)
	t.buf.Write(data)
	if !t.streaming {
		return n, nil
	}
	return n, t.processSSEBuffer()
}

// Flush only forwards once committed to streaming; flushing during buffered
// non-streaming would prematurely commit gin's default 200 status.
func (t *SSETranslator) Flush() {
	if !t.streaming {
		return
	}
	if t.flusher != nil {
		t.flusher.Flush()
	}
}

// Finalize writes the buffered body for non-streaming responses. Streaming
// is a no-op — [DONE] was already emitted on message_stop.
func (t *SSETranslator) Finalize() error {
	if t.streaming {
		return nil
	}

	body := t.buf.Bytes()
	if t.statusCode >= 400 {
		t.inner.Header().Set("Content-Type", "application/json")
		t.inner.WriteHeader(t.statusCode)
		_, err := t.inner.Write(AnthropicToOpenAIError(body))
		return err
	}

	if t.usageSink != nil {
		usage := gjson.GetBytes(body, "usage")
		if usage.Exists() {
			t.usageSink.RecordUsage(
				int(usage.Get("input_tokens").Int()),
				int(usage.Get("output_tokens").Int()),
			)
			t.usageSink.RecordCacheUsage(
				int(usage.Get("cache_creation_input_tokens").Int()),
				int(usage.Get("cache_read_input_tokens").Int()),
			)
		}
	}

	translated, err := AnthropicToOpenAIResponse(body, t.model)
	if err != nil {
		t.inner.Header().Set("Content-Type", "application/json")
		t.inner.WriteHeader(http.StatusBadGateway)
		_, _ = t.inner.Write([]byte(`{"error":{"message":"translation failed","type":"api_error"}}`))
		return err
	}
	t.inner.Header().Set("Content-Type", "application/json")
	t.inner.WriteHeader(t.statusCode)
	_, err = t.inner.Write(translated)
	return err
}

func (t *SSETranslator) processSSEBuffer() error {
	for {
		event, n := sse.SplitNext(t.buf.Bytes())
		if n == 0 {
			return nil
		}
		err := t.translateEvent(event)
		t.buf.Next(n)
		if err != nil {
			return err
		}
	}
}

func (t *SSETranslator) translateEvent(raw []byte) error {
	eventType, data := sse.ParseEvent(raw)
	if len(data) == 0 {
		return nil
	}

	switch string(eventType) {
	case "message_start":
		return t.handleMessageStart(data)
	case "content_block_start":
		return t.handleContentBlockStart(data)
	case "content_block_delta":
		return t.handleContentBlockDelta(data)
	case "content_block_stop":
		return t.handleContentBlockStop()
	case "message_delta":
		return t.handleMessageDelta(data)
	case "message_stop":
		return t.emitDone()
	}
	return nil
}

func (t *SSETranslator) handleMessageStart(data []byte) error {
	// strings.Clone: gjson returns strings backed by the buffer via unsafe;
	// these fields outlive the event, so copy to survive buffer compaction.
	if id := gjson.GetBytes(data, "message.id").Str; id != "" {
		t.msgID = strings.Clone(id)
	}
	if m := gjson.GetBytes(data, "message.model").Str; m != "" {
		t.model = strings.Clone(m)
	}

	t.writeChunkHeader()
	t.bw.WriteString(`"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]`)

	usageResult := gjson.GetBytes(data, "message.usage")
	if usageResult.Exists() {
		inputTokens := usageResult.Get("input_tokens").Int()
		outputTokens := usageResult.Get("output_tokens").Int()
		t.bw.WriteString(`,"usage":{"prompt_tokens":`)
		sse.WriteJSONInt(t.bw, inputTokens)
		t.bw.WriteString(`,"completion_tokens":`)
		sse.WriteJSONInt(t.bw, outputTokens)
		t.bw.WriteString(`,"total_tokens":`)
		sse.WriteJSONInt(t.bw, inputTokens+outputTokens)
		t.bw.WriteByte('}')
		if t.usageSink != nil {
			t.usageSink.RecordUsage(int(inputTokens), 0)
			t.usageSink.RecordCacheUsage(
				int(usageResult.Get("cache_creation_input_tokens").Int()),
				int(usageResult.Get("cache_read_input_tokens").Int()),
			)
		}
	}

	t.bw.WriteString("}\n\n")
	return t.flushEvent()
}

func (t *SSETranslator) handleContentBlockStart(data []byte) error {
	bType := gjson.GetBytes(data, "content_block.type").Str
	t.currentIsTool = bType == "tool_use"
	if !t.currentIsTool {
		return nil
	}

	id := gjson.GetBytes(data, "content_block.id").Str
	name := gjson.GetBytes(data, "content_block.name").Str

	t.writeChunkHeader()
	t.bw.WriteString(`"choices":[{"index":0,"delta":{"tool_calls":[{"index":`)
	sse.WriteJSONInt(t.bw, int64(t.toolIdx))
	t.bw.WriteString(`,"id":`)
	sse.WriteJSONString(t.bw, id)
	t.bw.WriteString(`,"type":"function","function":{"name":`)
	sse.WriteJSONString(t.bw, name)
	t.bw.WriteString(`,"arguments":""}}]},"finish_reason":null}]}`)
	t.bw.WriteString("\n\n")
	return t.flushEvent()
}

func (t *SSETranslator) handleContentBlockDelta(data []byte) error {
	deltaType := gjson.GetBytes(data, "delta.type").Str

	switch deltaType {
	case "text_delta":
		text := gjson.GetBytes(data, "delta.text").Str
		t.writeChunkHeader()
		t.bw.WriteString(`"choices":[{"index":0,"delta":{"content":`)
		sse.WriteJSONString(t.bw, text)
		t.bw.WriteString(`},"finish_reason":null}]}`)
		t.bw.WriteString("\n\n")
		return t.flushEvent()

	case "input_json_delta":
		partial := gjson.GetBytes(data, "delta.partial_json").Str
		t.writeChunkHeader()
		t.bw.WriteString(`"choices":[{"index":0,"delta":{"tool_calls":[{"index":`)
		sse.WriteJSONInt(t.bw, int64(t.toolIdx))
		t.bw.WriteString(`,"function":{"arguments":`)
		sse.WriteJSONString(t.bw, partial)
		t.bw.WriteString(`}}]},"finish_reason":null}]}`)
		t.bw.WriteString("\n\n")
		return t.flushEvent()

	default:
		return nil
	}
}

func (t *SSETranslator) handleContentBlockStop() error {
	if t.currentIsTool {
		t.toolIdx++
	}
	t.currentIsTool = false
	return nil
}

func (t *SSETranslator) handleMessageDelta(data []byte) error {
	delta := gjson.GetBytes(data, "delta")
	if !delta.Exists() {
		return nil
	}

	finishReason := mapStopReason(delta.Get("stop_reason").Str)

	t.writeChunkHeader()
	t.bw.WriteString(`"choices":[{"index":0,"delta":{},"finish_reason":`)
	sse.WriteJSONString(t.bw, finishReason)
	t.bw.WriteString(`}]`)

	usageResult := gjson.GetBytes(data, "usage")
	if usageResult.Exists() {
		inputTokens := usageResult.Get("input_tokens").Int()
		outputTokens := usageResult.Get("output_tokens").Int()
		t.bw.WriteString(`,"usage":{"prompt_tokens":`)
		sse.WriteJSONInt(t.bw, inputTokens)
		t.bw.WriteString(`,"completion_tokens":`)
		sse.WriteJSONInt(t.bw, outputTokens)
		t.bw.WriteString(`,"total_tokens":`)
		sse.WriteJSONInt(t.bw, inputTokens+outputTokens)
		t.bw.WriteByte('}')
		if t.usageSink != nil {
			t.usageSink.RecordUsage(0, int(outputTokens))
		}
	}

	t.bw.WriteString("}\n\n")
	return t.flushEvent()
}

func (t *SSETranslator) writeChunkHeader() {
	t.bw.WriteString(`data: {"id":`)
	sse.WriteJSONString(t.bw, t.msgID)
	t.bw.WriteString(`,"object":"chat.completion.chunk","created":`)
	sse.WriteJSONInt(t.bw, t.created)
	t.bw.WriteString(`,"model":`)
	sse.WriteJSONString(t.bw, t.model)
	t.bw.WriteByte(',')
}

func (t *SSETranslator) emitDone() error {
	t.bw.WriteString("data: [DONE]\n\n")
	return t.flushEvent()
}

func (t *SSETranslator) flushEvent() error {
	if err := t.bw.Flush(); err != nil {
		return err
	}
	if t.flusher != nil {
		t.flusher.Flush()
	}
	return nil
}

var _ http.ResponseWriter = (*SSETranslator)(nil)
var _ http.Flusher = (*SSETranslator)(nil)

// AnthropicSSETranslator translates OpenAI Chat Completions SSE to Anthropic
// Messages format on the fly. Non-streaming responses buffer for Finalize.
type AnthropicSSETranslator struct {
	inner   http.ResponseWriter
	flusher http.Flusher
	bw      *bufio.Writer

	streaming  bool
	statusCode int
	buf        bytes.Buffer

	requestModel string

	started bool
	// closed guards against double-emission: [DONE] triggers finishStream
	// mid-stream, and Finalize runs again after Proxy returns.
	closed   bool
	blockIdx int
	// textOpen is lazily set on first content delta to avoid empty blocks for
	// tool-only responses.
	textOpen   bool
	toolBlocks map[int]int

	finishReason      string
	usageInputTokens  int
	usageOutputTokens int
	hasUsage          bool
	messageID         string
	modelFromUpstream string

	usageSink otel.UsageSink

	// usageCacheReadTokens is the prompt-cache hit count; kept on the
	// translator so the closing-marker callback can compute savings.
	usageCacheReadTokens int

	// routingMarker, when non-empty, is emitted as a standalone text block
	// at index 0 right after message_start. markerEmitted guards single emission.
	routingMarker string
	markerEmitted bool

	// closingMarkerFn, when non-nil, is invoked from finishStream after the
	// last upstream block closes; a non-empty return is emitted as a final
	// text block before message_delta.
	closingMarkerFn      func(Usage) string
	closingMarkerEmitted bool
}

// Usage is the upstream-observed token breakdown passed to the closing-marker
// callback. CacheCreationTokens is zero for OpenAI-style upstreams.
type Usage struct {
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
}

// NewAnthropicSSETranslator wraps w. Call Finalize after upstream returns.
func NewAnthropicSSETranslator(w http.ResponseWriter, requestModel string, sink otel.UsageSink) *AnthropicSSETranslator {
	flusher, _ := w.(http.Flusher)
	return &AnthropicSSETranslator{
		inner:        w,
		flusher:      flusher,
		bw:           bufio.NewWriterSize(w, 8192),
		requestModel: requestModel,
		toolBlocks:   make(map[int]int),
		usageSink:    sink,
	}
}

// WithRoutingMarker installs a text snippet emitted as a standalone content
// block at index 0 immediately after message_start. Empty string disables it.
func (t *AnthropicSSETranslator) WithRoutingMarker(marker string) *AnthropicSSETranslator {
	t.routingMarker = marker
	return t
}

// WithClosingMarker installs a callback invoked from finishStream after the
// last upstream block closes; a non-empty return is emitted as a final text
// block before message_delta.
func (t *AnthropicSSETranslator) WithClosingMarker(fn func(Usage) string) *AnthropicSSETranslator {
	t.closingMarkerFn = fn
	return t
}

func (t *AnthropicSSETranslator) usage() Usage {
	return Usage{
		InputTokens:     t.usageInputTokens,
		OutputTokens:    t.usageOutputTokens,
		CacheReadTokens: t.usageCacheReadTokens,
	}
}

func (t *AnthropicSSETranslator) Header() http.Header {
	return t.inner.Header()
}

// WriteHeader routes streaming success responses through SSE; errors and
// non-streaming defer to Finalize.
func (t *AnthropicSSETranslator) WriteHeader(code int) {
	t.statusCode = code
	ct := t.inner.Header().Get("Content-Type")
	t.streaming = strings.Contains(ct, "text/event-stream") && code < 400

	t.inner.Header().Del("Content-Length")
	t.inner.Header().Del("Content-Encoding")

	if t.streaming {
		t.inner.Header().Set("Content-Type", "text/event-stream")
		t.inner.WriteHeader(code)
	}
	if sseTraceEnabled {
		observability.Get().Debug("AnthropicSSE WriteHeader",
			"upstream_status", code,
			"upstream_content_type", ct,
			"streaming", t.streaming,
		)
	}
}

func (t *AnthropicSSETranslator) Write(data []byte) (int, error) {
	n := len(data)
	t.buf.Write(data)
	if !t.streaming {
		return n, nil
	}
	return n, t.processOpenAISSEBuffer()
}

func (t *AnthropicSSETranslator) Flush() {
	if !t.streaming {
		return
	}
	if t.flusher != nil {
		t.flusher.Flush()
	}
}

// Finalize writes the buffered body for non-streaming responses; for streaming,
// emits trailing message_delta/message_stop if not already closed.
func (t *AnthropicSSETranslator) Finalize() error {
	if sseTraceEnabled {
		observability.Get().Debug("AnthropicSSE Finalize entry",
			"streaming", t.streaming,
			"closed", t.closed,
			"started", t.started,
			"status_code", t.statusCode,
			"buffered_bytes", t.buf.Len(),
			"buffered_preview", truncate(t.buf.String(), 240),
		)
	}
	if t.streaming {
		if t.closed {
			return nil
		}
		if t.started {
			return t.finishStream()
		}
		return nil
	}

	body := t.buf.Bytes()
	if t.statusCode >= 400 {
		t.inner.Header().Set("Content-Type", "application/json")
		t.inner.WriteHeader(t.statusCode)
		_, err := t.inner.Write(OpenAIToAnthropicError(body))
		return err
	}

	if t.usageSink != nil {
		usage := gjson.GetBytes(body, "usage")
		if usage.Exists() {
			t.usageSink.RecordUsage(
				int(usage.Get("prompt_tokens").Int()),
				int(usage.Get("completion_tokens").Int()),
			)
			t.usageSink.RecordCacheUsage(
				0,
				int(usage.Get("prompt_tokens_details.cached_tokens").Int()),
			)
		}
	}

	translated, err := OpenAIToAnthropicResponse(body, t.requestModel)
	if err != nil {
		t.inner.Header().Set("Content-Type", "application/json")
		t.inner.WriteHeader(http.StatusBadGateway)
		_, _ = t.inner.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"translation failed"}}`))
		return err
	}
	t.inner.Header().Set("Content-Type", "application/json")
	t.inner.WriteHeader(t.statusCode)
	_, err = t.inner.Write(translated)
	return err
}

func (t *AnthropicSSETranslator) processOpenAISSEBuffer() error {
	for {
		event, n := sse.SplitNext(t.buf.Bytes())
		if n == 0 {
			return nil
		}
		err := t.translateOpenAIEvent(event)
		t.buf.Next(n)
		if err != nil {
			return err
		}
	}
}

func (t *AnthropicSSETranslator) translateOpenAIEvent(raw []byte) error {
	_, data := sse.ParseEvent(raw)
	if len(data) == 0 {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return t.finishStream()
	}

	// strings.Clone: gjson returns strings backed by the buffer via unsafe;
	// these fields outlive the event, so copy to survive buffer compaction.
	if id := gjson.GetBytes(data, "id"); id.Exists() && t.messageID == "" {
		t.messageID = strings.Clone(id.Str)
	}
	if m := gjson.GetBytes(data, "model"); m.Exists() && t.modelFromUpstream == "" {
		t.modelFromUpstream = strings.Clone(m.Str)
	}

	choices := gjson.GetBytes(data, "choices")
	if !choices.IsArray() {
		t.extractAndForwardUsage(data)
		return nil
	}
	firstChoice := gjson.GetBytes(data, "choices.0")
	if !firstChoice.Exists() {
		t.extractAndForwardUsage(data)
		return nil
	}

	if !t.started {
		if err := t.emitMessageStart(); err != nil {
			return err
		}
		t.started = true
		if err := t.emitRoutingMarkerIfConfigured(); err != nil {
			return err
		}
	}

	delta := firstChoice.Get("delta")
	if delta.Exists() {
		if err := t.emitDelta(delta); err != nil {
			return err
		}
	}

	if fr := firstChoice.Get("finish_reason").Str; fr != "" {
		t.finishReason = strings.Clone(fr)
	}
	t.extractAndForwardUsage(data)
	return nil
}

func (t *AnthropicSSETranslator) extractAndForwardUsage(data []byte) {
	usage := gjson.GetBytes(data, "usage")
	if !usage.Exists() {
		return
	}
	prompt := usage.Get("prompt_tokens").Int()
	completion := usage.Get("completion_tokens").Int()
	cachedRead := usage.Get("prompt_tokens_details.cached_tokens").Int()
	t.usageInputTokens = int(prompt)
	t.usageOutputTokens = int(completion)
	t.usageCacheReadTokens = int(cachedRead)
	t.hasUsage = true
	if t.usageSink != nil {
		t.usageSink.RecordUsage(int(prompt), int(completion))
		t.usageSink.RecordCacheUsage(0, int(cachedRead))
	}
}

func (t *AnthropicSSETranslator) emitDelta(delta gjson.Result) error {
	if content := delta.Get("content").Str; content != "" {
		if !t.textOpen {
			if err := t.emitContentBlockStartText(t.blockIdx); err != nil {
				return err
			}
			t.textOpen = true
			t.blockIdx++
		}
		if err := t.emitContentBlockDeltaText(t.blockIdx-1, content); err != nil {
			return err
		}
	}

	toolCalls := delta.Get("tool_calls")
	if !toolCalls.Exists() {
		return nil
	}

	var emitErr error
	toolCalls.ForEach(func(_, tc gjson.Result) bool {
		idx := int(tc.Get("index").Int())
		blockIdx, ok := t.toolBlocks[idx]
		if !ok {
			if t.textOpen {
				if err := t.emitContentBlockStop(t.blockIdx - 1); err != nil {
					emitErr = err
					return false
				}
				t.textOpen = false
			}
			id := tc.Get("id").Str
			name := tc.Get("function.name").Str
			sig := tc.Get("function.thought_signature").Str
			if sig == "" {
				sig = tc.Get("thought_signature").Str
			}
			blockIdx = t.blockIdx
			t.toolBlocks[idx] = blockIdx
			t.blockIdx++
			if emitErr = t.emitContentBlockStartTool(blockIdx, id, name, sig); emitErr != nil {
				return false
			}
		}
		if args := tc.Get("function.arguments").Str; args != "" {
			if emitErr = t.emitContentBlockDeltaJSON(blockIdx, args); emitErr != nil {
				return false
			}
		}
		return true
	})
	return emitErr
}

// emitRoutingMarkerIfConfigured emits the routing marker as a standalone text
// block at the current index, once per response.
func (t *AnthropicSSETranslator) emitRoutingMarkerIfConfigured() error {
	if t.markerEmitted || t.routingMarker == "" {
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
	t.markerEmitted = true
	return nil
}

// emitClosingMarkerIfConfigured invokes the callback (if any) and emits a
// final text block when it returns non-empty. Empty returns are a no-op.
func (t *AnthropicSSETranslator) emitClosingMarkerIfConfigured() error {
	if t.closingMarkerEmitted || t.closingMarkerFn == nil {
		return nil
	}
	text := t.closingMarkerFn(t.usage())
	if text == "" {
		t.closingMarkerEmitted = true
		return nil
	}
	idx := t.blockIdx
	if err := t.emitContentBlockStartText(idx); err != nil {
		return err
	}
	if err := t.emitContentBlockDeltaText(idx, text); err != nil {
		return err
	}
	if err := t.emitContentBlockStop(idx); err != nil {
		return err
	}
	t.blockIdx++
	t.closingMarkerEmitted = true
	return nil
}

func (t *AnthropicSSETranslator) finishStream() error {
	if !t.started {
		if err := t.emitMessageStart(); err != nil {
			return err
		}
		t.started = true
		if err := t.emitRoutingMarkerIfConfigured(); err != nil {
			return err
		}
	}
	if t.textOpen {
		if err := t.emitContentBlockStop(t.blockIdx - 1); err != nil {
			return err
		}
		t.textOpen = false
	}
	for _, blockIdx := range t.toolBlocks {
		if err := t.emitContentBlockStop(blockIdx); err != nil {
			return err
		}
	}
	t.toolBlocks = map[int]int{}

	if err := t.emitClosingMarkerIfConfigured(); err != nil {
		return err
	}

	if err := t.emitMessageDelta(); err != nil {
		return err
	}
	if err := t.emitMessageStop(); err != nil {
		return err
	}
	t.closed = true
	return nil
}

func (t *AnthropicSSETranslator) emitMessageStart() error {
	model := t.modelFromUpstream
	if model == "" {
		model = t.requestModel
	}
	id := t.messageID
	if id == "" {
		id = "msg_translated"
	}
	if sseTraceEnabled {
		observability.Get().Debug("AnthropicSSE emit", "event", "message_start")
	}
	t.bw.WriteString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":")
	sse.WriteJSONString(t.bw, id)
	t.bw.WriteString(",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":")
	sse.WriteJSONString(t.bw, model)
	t.bw.WriteString(",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n")
	return t.flushEvent()
}

func (t *AnthropicSSETranslator) emitContentBlockStartText(index int) error {
	if sseTraceEnabled {
		observability.Get().Debug("AnthropicSSE emit", "event", "content_block_start", "type", "text")
	}
	t.bw.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString(",\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	return t.flushEvent()
}

// emitContentBlockStartTool emits an Anthropic content_block_start for a
// tool_use block. sig, when non-empty, is the opaque thought_signature that
// Gemini 3.x requires round-tripped on the next turn's functionCall part —
// smuggled here as a non-standard field on the tool_use block so clients
// that pass through unknown fields preserve it.
func (t *AnthropicSSETranslator) emitContentBlockStartTool(index int, id, name, sig string) error {
	if sseTraceEnabled {
		observability.Get().Debug("AnthropicSSE emit", "event", "content_block_start", "type", "tool_use", "name", name)
	}
	t.bw.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString(",\"content_block\":{\"type\":\"tool_use\",\"id\":")
	sse.WriteJSONString(t.bw, id)
	t.bw.WriteString(",\"name\":")
	sse.WriteJSONString(t.bw, name)
	if sig != "" {
		t.bw.WriteString(",\"thought_signature\":")
		sse.WriteJSONString(t.bw, sig)
	}
	t.bw.WriteString(",\"input\":{}}}\n\n")
	return t.flushEvent()
}

func (t *AnthropicSSETranslator) emitContentBlockDeltaText(index int, text string) error {
	if sseTraceEnabled {
		observability.Get().Debug("AnthropicSSE emit", "event", "content_block_delta", "type", "text_delta")
	}
	t.bw.WriteString("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString(",\"delta\":{\"type\":\"text_delta\",\"text\":")
	sse.WriteJSONString(t.bw, text)
	t.bw.WriteString("}}\n\n")
	return t.flushEvent()
}

func (t *AnthropicSSETranslator) emitContentBlockDeltaJSON(index int, partial string) error {
	if sseTraceEnabled {
		observability.Get().Debug("AnthropicSSE emit", "event", "content_block_delta", "type", "input_json_delta")
	}
	t.bw.WriteString("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString(",\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":")
	sse.WriteJSONString(t.bw, partial)
	t.bw.WriteString("}}\n\n")
	return t.flushEvent()
}

func (t *AnthropicSSETranslator) emitContentBlockStop(index int) error {
	if sseTraceEnabled {
		observability.Get().Debug("AnthropicSSE emit", "event", "content_block_stop")
	}
	t.bw.WriteString("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString("}\n\n")
	return t.flushEvent()
}

func (t *AnthropicSSETranslator) emitMessageDelta() error {
	stopReason := openAIFinishToAnthropic(t.finishReason)
	if sseTraceEnabled {
		observability.Get().Debug("AnthropicSSE emit", "event", "message_delta", "stop_reason", stopReason)
	}
	t.bw.WriteString("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":")
	sse.WriteJSONString(t.bw, stopReason)
	t.bw.WriteString(",\"stop_sequence\":null},\"usage\":{")
	if t.hasUsage {
		t.bw.WriteString("\"input_tokens\":")
		sse.WriteJSONInt(t.bw, int64(t.usageInputTokens))
		t.bw.WriteString(",\"output_tokens\":")
		sse.WriteJSONInt(t.bw, int64(t.usageOutputTokens))
	} else {
		t.bw.WriteString("\"output_tokens\":0")
	}
	t.bw.WriteString("}}\n\n")
	return t.flushEvent()
}

func (t *AnthropicSSETranslator) emitMessageStop() error {
	if sseTraceEnabled {
		observability.Get().Debug("AnthropicSSE emit", "event", "message_stop")
	}
	t.bw.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	return t.flushEvent()
}

func (t *AnthropicSSETranslator) flushEvent() error {
	if err := t.bw.Flush(); err != nil {
		return err
	}
	if t.flusher != nil {
		t.flusher.Flush()
	}
	return nil
}

func openAIFinishToAnthropic(reason string) string {
	switch reason {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "stop", "":
		return "end_turn"
	default:
		return "end_turn"
	}
}

var _ http.ResponseWriter = (*AnthropicSSETranslator)(nil)
var _ http.Flusher = (*AnthropicSSETranslator)(nil)

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
