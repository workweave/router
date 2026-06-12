package translate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"workweave/router/internal/observability"
	"workweave/router/internal/observability/otel"
	"workweave/router/internal/sse"
	"workweave/router/internal/translate/toolcheck"

	"github.com/tidwall/gjson"
)

// SSETranslator translates Anthropic streaming SSE to OpenAI chat.completion.chunk
// on the fly. Non-streaming responses buffer for Finalize.
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
	// toolIdx advances on content_block_stop for tool_use blocks.
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

// WriteHeader routes streaming success responses through SSE.
func (t *SSETranslator) WriteHeader(code int) {
	t.statusCode = code
	ct := t.inner.Header().Get("Content-Type")
	t.streaming = strings.Contains(ct, "text/event-stream") && code < 400

	// Content-Length and Content-Encoding are stale once we re-encode.
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

// Flush only forwards once streaming is committed.
func (t *SSETranslator) Flush() {
	if !t.streaming {
		return
	}
	if t.flusher != nil {
		t.flusher.Flush()
	}
}

// Finalize writes the buffered body for non-streaming responses.
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

	streaming      bool
	headersEmitted bool
	statusCode     int
	buf            bytes.Buffer

	requestModel string

	started bool
	// closed guards against double-emission after [DONE] triggers finishStream.
	closed   bool
	blockIdx int
	// textOpen avoids empty text blocks for tool-only responses.
	textOpen bool
	// thinkingOpen tracks an open Anthropic thinking content block carrying
	// upstream reasoning (OpenRouter `reasoning`, DeepSeek/Qwen `reasoning_content`).
	// Without this, reasoning either gets dropped or leaks into the visible
	// text channel; clients see "Let me check..." narration mixed with the
	// real answer.
	thinkingOpen bool
	// pendingText holds whitespace-only content deltas that arrived before any
	// non-whitespace text. DeepSeek-v4 (and other reasoning upstreams on
	// Fireworks/OpenRouter) emit a "\n\n" content delta between
	// reasoning_content and tool_calls; opening a text block for it renders as
	// an empty assistant block between the thinking block and the tool_use. We
	// hold whitespace until real text justifies a block, then flush it as the
	// block's leading content so the model's formatting survives. If only
	// whitespace ever arrives, no text block opens and the buffer is dropped.
	pendingText strings.Builder
	toolBlocks  map[int]int
	// suppressedTools holds tool_call indices we refused to open because the
	// first delta carried no function name (see emitDelta). Later argument
	// fragments reuse the same index, so we must remember to drop them too.
	suppressedTools map[int]struct{}
	// toolArgsBuffer accumulates input_json_delta fragments per Anthropic
	// content-block index so we can validate the final concatenated args at
	// content_block_stop. OpenAI-compat upstreams (GLM, Kimi, Qwen, gpt-oss on
	// vLLM/SGLang) intermittently emit a JSON object whose final form fails
	// to parse — partial keys, unbalanced braces, mid-string truncation.
	// Without validation we relay malformed tool_use input downstream and
	// the client retries or errors silently; with it we get a structured
	// warn log on every malformed turn for observability and an explicit
	// signal that active drop is the next step if this turns out frequent.
	toolArgsBuffer map[int]*strings.Builder
	// toolArgsInvalid latches per-block-index when buffered args fail to
	// parse as JSON at content_block_stop. Surfaced via Summary so the
	// proxy can count malformed-tool turns from logs alone.
	toolArgsInvalid map[int]struct{}
	// toolNames records each tool block's function name so the buffered args
	// can be validated against the right tool schema at content_block_stop.
	toolNames map[int]string
	// toolValidator validates and repairs buffered tool args against the
	// inbound request's tool schemas (see toolcheck). Nil means
	// syntax-check-only.
	toolValidator *toolcheck.Validator
	// toolCallIssues collects every validation/repair finding so the proxy
	// can emit per-block router.tool_call_invalid telemetry from Summary.
	toolCallIssues []toolcheck.Issue
	// toolUseEmitted latches true the first time a tool_use content block is
	// opened. finishStream clears toolBlocks before emitMessageDelta, so we
	// can't read len(toolBlocks) at delta time; the latch outlives the map.
	toolUseEmitted bool
	// toolUseCount counts tool_use content blocks opened across the response.
	// Like toolUseEmitted it outlives toolBlocks (which finishStream clears),
	// so a post-stream observer can report how many tools the model emitted.
	toolUseCount int
	// emittedStopReason records the stop_reason actually written by
	// emitMessageDelta (after the tool_use promotion). Surfaced via Summary so
	// the proxy can log what the client saw without re-deriving it.
	emittedStopReason string

	finishReason             string
	usageInputTokens         int
	usageOutputTokens        int
	usageCacheCreationTokens int
	usageCacheReadTokens     int
	hasUsage                 bool
	messageID                string
	modelFromUpstream        string

	usageSink otel.UsageSink

	// estimatedInputTokens is the pre-inference estimate, used to populate
	// message_start.usage.input_tokens for cross-format paths where upstream
	// usage doesn't arrive until message_delta.
	estimatedInputTokens int

	// routingMarker is emitted as a standalone text block at index 0.
	routingMarker string
	markerEmitted bool

	// requestHadTools is true when the inbound Anthropic Messages request
	// carried a non-empty tools array. Used at finishStream to detect the
	// "model produced no tool_use when tools were available" case (Gemini-3.1
	// and Mimo-v2.5 sometimes emit prose + <think> XML as plain text instead
	// of tool_use blocks) and synthesize a recovery nudge that keeps Claude
	// Code's agentic loop alive.
	requestHadTools bool

	// nudgeEmitted latches true when finishStream emitted the text-only
	// recovery nudge, so Summary can surface it for log analysis.
	nudgeEmitted bool

	// sawText latches true once any non-empty content-channel text delta is
	// emitted. Lets synthesizeTextOnlyTurnNudge tell a model that produced a
	// real (if tool-free) answer apart from a turn that emitted nothing.
	sawText bool

	// leadingContent accumulates up to leadingContentCap bytes of the content
	// channel's opening text so the nudge can tell whether the turn *led* with
	// tool-call / raw-reasoning markup. The parse-failure mode the nudge targets
	// opens the turn with the markup: a model dumping <think> reasoning or an
	// unstructured tool call into the content channel because the server didn't
	// route it to reasoning_content / tool_calls. A legitimate final answer that
	// merely mentions "<think>" mid-prose — e.g. a model explaining tag syntax —
	// must NOT trip it, so the check is anchored to the start (see
	// leadsWithToolishMarkup), not a substring scan anywhere in the text.
	leadingContent strings.Builder

	// stopReasonDemoted latches true when emitMessageDelta demoted a
	// finish_reason="tool_calls" turn to end_turn because no tool_use block
	// survived. Surfaced via Summary so the proxy can count the degenerate
	// tool-call turns that previously dead-ended agent clients.
	stopReasonDemoted bool
}

// NewAnthropicSSETranslator wraps w. Call Finalize after upstream returns.
func NewAnthropicSSETranslator(w http.ResponseWriter, requestModel string, sink otel.UsageSink) *AnthropicSSETranslator {
	flusher, _ := w.(http.Flusher)
	return &AnthropicSSETranslator{
		inner:           w,
		flusher:         flusher,
		bw:              bufio.NewWriterSize(w, 8192),
		requestModel:    requestModel,
		toolBlocks:      make(map[int]int),
		suppressedTools: make(map[int]struct{}),
		toolArgsBuffer:  make(map[int]*strings.Builder),
		toolArgsInvalid: make(map[int]struct{}),
		toolNames:       make(map[int]string),
		usageSink:       sink,
	}
}

// WithToolValidator installs the request's compiled tool-schema validator so
// buffered tool args are validated (and safely repaired) before emission.
// Pass nil to disable schema checking (no tools in the request).
func (t *AnthropicSSETranslator) WithToolValidator(v *toolcheck.Validator) *AnthropicSSETranslator {
	t.toolValidator = v
	return t
}

// WithRoutingMarker installs a text snippet emitted as a standalone content
// block at index 0 after message_start. Empty string disables it.
func (t *AnthropicSSETranslator) WithRoutingMarker(marker string) *AnthropicSSETranslator {
	t.routingMarker = marker
	return t
}

// WithEstimatedInputTokens seeds message_start.usage.input_tokens for cross-format
// paths where upstream usage arrives after message_start.
func (t *AnthropicSSETranslator) WithEstimatedInputTokens(n int) *AnthropicSSETranslator {
	if n > 0 {
		t.estimatedInputTokens = n
	}
	return t
}

// WithRequestHadTools tells the translator whether the inbound Anthropic
// Messages request carried tools. Enables the finishStream recovery path
// that synthesizes a Bash nudge when the upstream emitted prose/thinking
// text but no tool_use block — the dominant residual empty-patch failure
// mode on Gemini-3.1-Pro and Mimo-v2.5-Pro after PRs #280 / #281.
func (t *AnthropicSSETranslator) WithRequestHadTools(hadTools bool) *AnthropicSSETranslator {
	t.requestHadTools = hadTools
	return t
}

// ResponseSummary reports translated-response signals an observer can log
// after the stream completes. It exists to answer, from logs alone, what an
// OpenAI-compat upstream actually returned and whether the tool_use stop_reason
// promotion (see emitMessageDelta) had to fire for this turn.
type ResponseSummary struct {
	// UpstreamFinishReason is the raw OpenAI finish_reason as received
	// ("stop", "tool_calls", "length", ""), before any Anthropic mapping.
	UpstreamFinishReason string
	// StopReason is the Anthropic stop_reason actually emitted to the client
	// (post tool_use promotion). Empty if the stream ended before message_delta.
	StopReason string
	// StopReasonPromoted is true when tool_use blocks forced stop_reason to
	// "tool_use" over an upstream finish_reason that mapped to something else.
	StopReasonPromoted bool
	// ToolUseBlocks is the number of tool_use content blocks emitted.
	ToolUseBlocks int
	// InvalidToolArgsBlocks counts tool_use blocks whose buffered
	// input_json_delta payload failed JSON validation at content_block_stop.
	// Non-zero indicates an OpenAI-compat upstream emitted malformed tool
	// arguments that the client will likely fail to parse. See
	// validateBufferedToolArgs.
	InvalidToolArgsBlocks int
	// TextOnlyTurnNudged is true when finishStream synthesized a Bash
	// recovery nudge because the upstream produced no tool_use block on a
	// request that had tools available. See synthesizeTextOnlyTurnNudge.
	TextOnlyTurnNudged bool
	// StopReasonDemoted is true when a finish_reason="tool_calls" turn was
	// demoted to end_turn because no tool_use block survived (see
	// emitMessageDelta). It marks the degenerate tool-call turns that
	// previously dead-ended agent clients with stop_reason="tool_use" and zero
	// tool_use blocks.
	StopReasonDemoted bool
	// SuppressedToolCalls counts tool_calls dropped for carrying no function
	// name (see emitDelta). Non-zero alongside StopReasonDemoted points at the
	// nameless-call case; zero alongside it points at the call-emitted-as-text
	// case — enough to tell the two apart from logs without dumping bodies.
	SuppressedToolCalls int
	// ToolCallIssues lists every tool_use block that failed toolcheck
	// validation (invalid JSON, unknown tool, schema mismatch), including
	// ones deterministic repair recovered. The proxy logs one
	// router.tool_call_invalid event per entry.
	ToolCallIssues []toolcheck.Issue
	// OutputTokens is the upstream completion_tokens count, when reported.
	OutputTokens int
	// InputTokens is the upstream prompt_tokens count, when reported.
	InputTokens int
	// CacheReadTokens is the cache_read_input_tokens count from Anthropic or
	// cached_tokens count from OpenAI, when reported.
	CacheReadTokens int
}

// Summary returns the response summary for observability. Call after Finalize;
// before the stream completes the fields reflect partial state.
func (t *AnthropicSSETranslator) Summary() ResponseSummary {
	return ResponseSummary{
		UpstreamFinishReason:  t.finishReason,
		StopReason:            t.emittedStopReason,
		StopReasonPromoted:    t.toolUseEmitted && openAIFinishToAnthropic(t.finishReason) != "tool_use",
		ToolUseBlocks:         t.toolUseCount,
		InvalidToolArgsBlocks: len(t.toolArgsInvalid),
		TextOnlyTurnNudged:    t.nudgeEmitted,
		StopReasonDemoted:     t.stopReasonDemoted,
		SuppressedToolCalls:   len(t.suppressedTools),
		ToolCallIssues:        t.toolCallIssues,
		OutputTokens:          t.usageOutputTokens,
		InputTokens:           t.usageInputTokens,
		CacheReadTokens:       t.usageCacheReadTokens,
	}
}

func (t *AnthropicSSETranslator) Header() http.Header {
	return t.inner.Header()
}

// WriteHeader routes streaming success responses through SSE.
func (t *AnthropicSSETranslator) WriteHeader(code int) {
	if t.headersEmitted {
		return
	}
	t.statusCode = code
	ct := t.inner.Header().Get("Content-Type")
	t.streaming = strings.Contains(ct, "text/event-stream") && code < 400

	t.inner.Header().Del("Content-Length")
	t.inner.Header().Del("Content-Encoding")

	if t.streaming {
		t.inner.Header().Set("Content-Type", "text/event-stream")
		t.inner.WriteHeader(code)
		t.headersEmitted = true
	}
	observability.Get().Debug("AnthropicSSE WriteHeader",
		"upstream_status", code,
		"upstream_content_type", ct,
		"streaming", t.streaming,
	)
}

// Prelude commits SSE headers and emits message_start (+ routing marker block)
// immediately so Anthropic-format clients see the message envelope while the
// upstream provider is still doing prefill. Call right after the routing
// decision when the client requested streaming (streaming=true).
//
// estimatedInputTokens populates message_start.usage.input_tokens since real
// upstream usage doesn't arrive until message_delta on cross-format paths;
// already plumbed via WithEstimatedInputTokens.
func (t *AnthropicSSETranslator) Prelude(streaming bool) error {
	if !streaming || t.started {
		return nil
	}
	t.inner.Header().Set("Content-Type", "text/event-stream")
	t.inner.Header().Del("Content-Length")
	t.inner.Header().Del("Content-Encoding")
	t.statusCode = http.StatusOK
	t.streaming = true
	t.inner.WriteHeader(http.StatusOK)
	t.headersEmitted = true
	if err := t.emitMessageStart(); err != nil {
		return err
	}
	t.started = true
	return t.emitRoutingMarkerIfConfigured()
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

// Finalize writes the buffered body for non-streaming responses, or emits
// trailing message_delta/message_stop for streaming.
func (t *AnthropicSSETranslator) Finalize() error {
	observability.Get().Debug("AnthropicSSE Finalize entry",
		"streaming", t.streaming,
		"closed", t.closed,
		"started", t.started,
		"status_code", t.statusCode,
		"buffered_bytes", t.buf.Len(),
	)
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
				int(usage.Get("prompt_tokens_details.cache_creation_tokens").Int()),
				int(usage.Get("prompt_tokens_details.cached_tokens").Int()),
			)
		}
	}

	translated, issues, err := openAIToAnthropicResponse(body, t.requestModel, t.toolValidator)
	t.toolCallIssues = append(t.toolCallIssues, issues...)
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
	//
	// Check Str != "" rather than Exists(): some OpenAI-compat upstreams
	// (notably OpenRouter for certain models) send chunks with `"id": ""`
	// in early SSE frames before settling on a real id. gjson treats that
	// as Exists()=true, Str="", which would latch the empty string and
	// prevent later non-empty ids from overwriting — leaving message_start
	// to fall back to a generated "msg_translated_*" placeholder. Same for
	// model.
	if id := gjson.GetBytes(data, "id"); id.Str != "" && t.messageID == "" {
		t.messageID = strings.Clone(id.Str)
	}
	if m := gjson.GetBytes(data, "model"); m.Str != "" && t.modelFromUpstream == "" {
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
	cacheCreation := usage.Get("prompt_tokens_details.cache_creation_tokens").Int()
	t.usageCacheCreationTokens = int(cacheCreation)
	t.usageCacheReadTokens = int(cachedRead)
	t.hasUsage = true
	if t.usageSink != nil {
		t.usageSink.RecordUsage(int(prompt), int(completion))
		t.usageSink.RecordCacheUsage(int(cacheCreation), int(cachedRead))
	}
}

func (t *AnthropicSSETranslator) emitDelta(delta gjson.Result) error {
	// Reasoning arrives under different keys depending on upstream:
	//   - OpenRouter normalizes to `reasoning`
	//   - DeepSeek / Qwen native expose `reasoning_content`
	// Either way it must surface as an Anthropic thinking block, not text.
	reasoning := delta.Get("reasoning_content").Str
	if reasoning == "" {
		reasoning = delta.Get("reasoning").Str
	}
	if reasoning != "" {
		if t.textOpen {
			if err := t.emitContentBlockStop(t.blockIdx - 1); err != nil {
				return err
			}
			t.textOpen = false
		}
		if !t.thinkingOpen {
			if err := t.emitContentBlockStartThinking(t.blockIdx); err != nil {
				return err
			}
			t.thinkingOpen = true
			t.blockIdx++
		}
		if err := t.emitContentBlockDeltaThinking(t.blockIdx-1, reasoning); err != nil {
			return err
		}
	}

	if content := delta.Get("content").Str; content != "" {
		// Defer opening a text block until non-whitespace arrives. A
		// whitespace-only delta with no text block open yet would otherwise
		// surface as an empty text block wedged between a thinking block and a
		// tool_use (DeepSeek-v4 emits "\n\n" there). Buffer it instead; it is
		// flushed once real text justifies a block, or dropped if the turn ends
		// on tool_use. Whitespace inside an already-open text block is
		// legitimate formatting and emits normally. Falls through to tool_calls
		// since a single delta can carry both.
		if !t.textOpen && strings.TrimSpace(content) == "" {
			t.pendingText.WriteString(content)
		} else {
			if t.thinkingOpen {
				if err := t.emitContentBlockStop(t.blockIdx - 1); err != nil {
					return err
				}
				t.thinkingOpen = false
			}
			if !t.textOpen {
				if err := t.emitContentBlockStartText(t.blockIdx); err != nil {
					return err
				}
				t.textOpen = true
				t.blockIdx++
			}
			if t.pendingText.Len() > 0 {
				content = t.pendingText.String() + content
				t.pendingText.Reset()
			}
			t.sawText = true
			if n := t.leadingContent.Len(); n < leadingContentCap {
				if room := leadingContentCap - n; len(content) > room {
					t.leadingContent.WriteString(content[:room])
				} else {
					t.leadingContent.WriteString(content)
				}
			}
			if err := t.emitContentBlockDeltaText(t.blockIdx-1, content); err != nil {
				return err
			}
		}
	}

	// GLM-5.1 (and other vLLM/SGLang OpenAI-compat upstreams) emit
	// `"tool_calls": null` on every plain-text delta. gjson treats null as
	// Exists()=true but IsArray()=false, and ForEach over a null yields ONE
	// zero-value iteration (index=0, name=""). That spuriously trips the
	// nameless-call guard below and latches suppressedTools[0], so the real
	// named tool_call that arrives later at index 0 gets dropped as a
	// "fragment of a suppressed call" — the turn ends as an empty end_turn and
	// the agent idles. Only iterate real arrays.
	toolCalls := delta.Get("tool_calls")
	if !toolCalls.IsArray() {
		return nil
	}

	var emitErr error
	toolCalls.ForEach(func(_, tc gjson.Result) bool {
		idx := int(tc.Get("index").Int())
		if _, suppressed := t.suppressedTools[idx]; suppressed {
			// Argument fragments of a tool_call we refused to open. Drop them
			// so they don't stream into a stale/zero block index.
			return true
		}
		blockIdx, ok := t.toolBlocks[idx]
		if !ok {
			id := tc.Get("id").Str
			name := tc.Get("function.name").Str
			// A tool_call whose first delta carries no function name is
			// malformed. OpenAI-compat upstreams (GLM, Qwen, Kimi, gpt-oss on
			// vLLM/SGLang/DeepInfra) intermittently emit one, often closing the
			// turn with finish_reason="stop". Emitting it as a tool_use block
			// makes the client invoke tool "" -> "No such tool available" ->
			// retry -> infinite loop. Drop it: the turn ends on its real
			// stop_reason and any text already streamed survives. Guard runs
			// before closing text/thinking so a dropped tool can't truncate a
			// legitimate text block.
			if name == "" {
				t.suppressedTools[idx] = struct{}{}
				return true
			}
			if t.textOpen {
				if err := t.emitContentBlockStop(t.blockIdx - 1); err != nil {
					emitErr = err
					return false
				}
				t.textOpen = false
			}
			if t.thinkingOpen {
				if err := t.emitContentBlockStop(t.blockIdx - 1); err != nil {
					emitErr = err
					return false
				}
				t.thinkingOpen = false
			}
			sig := tc.Get("function.thought_signature").Str
			if sig == "" {
				sig = tc.Get("thought_signature").Str
			}
			blockIdx = t.blockIdx
			t.toolBlocks[idx] = blockIdx
			t.toolNames[blockIdx] = name
			t.toolUseEmitted = true
			t.toolUseCount++
			t.blockIdx++
			if emitErr = t.emitContentBlockStartTool(blockIdx, id, name, sig); emitErr != nil {
				return false
			}
		}
		if args := tc.Get("function.arguments").Str; args != "" {
			// Buffer-only: do NOT forward input_json_delta yet. The translator
			// emits exactly one input_json_delta per tool block at
			// content_block_stop time, after validation. This converts the
			// "OpenAI-compat upstream streams malformed args → Claude Code
			// parser dies on content_block_stop" failure mode into a clean
			// recoverable turn: invalid args are replaced with `{}` and CC
			// runs the tool (which then errors on missing required params),
			// instead of CC's strict parser rejecting the entire turn.
			//
			// Latency cost: tool-use TTFB rises from "as upstream emits each
			// args fragment" to "one delta at block close." Tool args are
			// typically <1KB even for Write/Edit, so the perceptible cost is
			// well under the per-tool dispatch budget.
			buf, ok := t.toolArgsBuffer[blockIdx]
			if !ok {
				buf = &strings.Builder{}
				t.toolArgsBuffer[blockIdx] = buf
			}
			buf.WriteString(args)
		}
		return true
	})
	return emitErr
}

// emitValidatedToolArgsDelta emits exactly one input_json_delta event for the
// given tool block, carrying the buffered arguments after toolcheck
// validation and repair. Called once per tool block at content_block_stop
// time. A no-op for blocks with no buffered args (tools that take no input).
// Unparseable args still degrade to `{}`, which converts a
// stream-parser-fatal turn into a tool-call that the client can dispatch
// (the tool then errors on missing required params, which the client retries
// via a user message — re-routing through the scorer to a different model);
// schema mismatches that repair can't fix forward as-emitted so the client's
// own tool error surfaces (forward + telemetry policy).
func (t *AnthropicSSETranslator) emitValidatedToolArgsDelta(blockIdx int) error {
	buf, ok := t.toolArgsBuffer[blockIdx]
	if !ok || buf.Len() == 0 {
		return nil
	}
	payload := buf.String()
	verdict := t.toolValidator.Check(t.toolNames[blockIdx], payload)
	if verdict.Issue != nil {
		t.toolCallIssues = append(t.toolCallIssues, *verdict.Issue)
		if verdict.Issue.Bucket == toolcheck.BucketInvalidJSON && !verdict.Issue.Repaired {
			t.toolArgsInvalid[blockIdx] = struct{}{}
			preview := payload
			const previewMax = 200
			if len(preview) > previewMax {
				preview = preview[:previewMax]
			}
			observability.Get().Error(
				"AnthropicSSE tool_use args failed JSON validation — substituting empty args",
				"block_index", blockIdx,
				"upstream_model", t.modelFromUpstream,
				"args_len", len(payload),
				"args_preview", preview,
			)
		}
	}
	return t.emitContentBlockDeltaJSON(blockIdx, verdict.Args)
}

// routerNudgeCommand is the Bash payload synthesized when the upstream
// produced no tool_use on a request that had tools available. The echo
// surfaces as a tool_result in the next turn's context, nudging the model
// to use a real tool. Bash is chosen because every Claude Code request
// includes it in tools, making the fabricated call dispatchable.
const routerNudgeCommand = "echo '[router] previous turn produced no tool_use; please use Edit/Write/Read/Bash/Grep — do not respond with prose or thinking tags only.'"

// leadingContentCap bounds how much of the content channel's opening text the
// translator retains for the leadsWithToolishMarkup check. Large enough to hold
// any marker plus a little leading whitespace; small enough to stay cheap.
const leadingContentCap = 64

// toolishMarkupMarkers are the opening tokens of a tool call a model leaked
// into the content channel as XML instead of emitting a structured tool_use
// block. Claude Code's strict parser rejects these ("tool call could not be
// parsed"), dead-ending the turn — which is exactly what the nudge rescues.
// Matched only at the START of the turn (see leadsWithToolishMarkup): the leak
// opens the turn with the markup, whereas a legitimate answer that discusses
// these tags has them mid-prose.
//
// Reasoning markup (<think>, <redacted_thinking>) is deliberately NOT here.
// Models like Mimo-v2.5 stream visible chain-of-thought as <think>…</think>
// text and then continue with a real answer, finishing with
// finish_reason="stop". That is a complete, valid turn — Claude Code renders
// the text fine; there is no parse failure to rescue. Treating a leading
// <think> as a failure stapled a synthetic Bash call onto every such answer,
// promoted the turn to stop_reason="tool_use", and looped the session (the
// client ran the echo, re-pinned the same model, got another <think>+answer,
// repeat). Only genuine tool-call markup is parse-fatal, so only it nudges.
var toolishMarkupMarkers = []string{"<tool_call", "<function", "<invoke"}

// leadsWithToolishMarkup reports whether the turn's content opens with
// tool-call/raw-reasoning markup, ignoring leading whitespace. Anchoring to the
// start is deliberate: a substring scan would misfire on a clean final answer
// that merely mentions "<think>" somewhere in its prose.
func leadsWithToolishMarkup(content string) bool {
	s := strings.TrimLeft(content, " \t\r\n")
	for _, m := range toolishMarkupMarkers {
		if strings.HasPrefix(s, m) {
			return true
		}
	}
	return false
}

// synthesizeTextOnlyTurnNudge fabricates a single Bash tool_use block when the
// upstream produced an assistant turn with no tool_use blocks AND the inbound
// request had tools available. Targets the bucket-C residual empty-patch
// failure mode: Gemini-3.1-Pro and Mimo-v2.5-Pro sometimes emit prose plus
// XML thinking tags as plain text, which Claude Code's strict parser refuses
// with "tool call could not be parsed (retry also failed)" and the session
// dies. Substituting a synthetic Bash echo turns the dead-end into a normal
// tool_use turn: Claude Code dispatches the Bash, gets the nudge as a
// tool_result, the next assistant turn re-emits with a real tool, and the
// agentic loop survives.
//
// No-op when the upstream already emitted a tool_use, when the inbound
// request had no tools (the model legitimately had nothing else to do), or
// when nothing has been written to the stream yet (an unrelated upstream
// error — preludeBuffer + flushErr handles that path).
//
// Final-answer guard: a tool-free text turn is only nudged when it actually
// looks like the parse-failure mode, not when a model simply finished its
// work. Concretely, the nudge is suppressed when finish_reason="stop" (or "")
// AND the emitted text is clean prose (no tool-call-like markup). That is the
// shape of a legitimate final answer — e.g. DeepSeek summarizing completed
// work — and stapling a synthetic Bash call onto it would revive an
// already-finished turn. finish_reason="tool_calls" (the upstream signaled a
// tool the parser couldn't structure) and content carrying tool-call markup
// still nudge; finish_reason="length" (truncation) never does, since a Bash
// echo cannot help a turn that was cut off mid-output.
//
// CRITICAL no-op on Gemini-3.x: the synthesized block is a tool_use with no
// thoughtSignature. Gemini-3.x requires a thoughtSignature on every
// functionCall part across turns; on the next turn writeGeminiFromAnthropic
// sees the sig-less nudge, sets anyToolUseMissingSig and drops the ENTIRE
// tool_use/tool_result history (emit_gemini.go dropToolBlocks). That wipes
// every prior Read/Grep/Bash result, so the model loses its working context,
// re-runs the same discovery commands, never edits, and loops to the turn
// ceiling (error_max_turns). Empirically this nudge made Gemini-3.x strictly
// worse (≥90-turn loops 4 → 46 across the v0.59 SWE-bench bake-off), so we
// suppress it here and let the turn end as text. The OpenAI-compat models the
// nudge was built for (e.g. Mimo-v2.5) have no such guard and still benefit.
func (t *AnthropicSSETranslator) synthesizeTextOnlyTurnNudge() error {
	if t.toolUseEmitted || !t.requestHadTools || !t.started {
		return nil
	}
	// The model DID emit a structured tool call this turn — it was just
	// dropped for being malformed (nameless function; see emitDelta /
	// suppressedTools). The drop already saved the client from invoking tool ""
	// in a loop; synthesizing a "use a tool" nudge on top would re-add a
	// tool_use the model never sent and, for finish_reason="tool_calls"
	// upstreams (GLM-5.1, Qwen, Kimi on vLLM/SGLang/DeepInfra), fire on every
	// turn the upstream emits the degenerate shape, looping to the turn ceiling.
	if len(t.suppressedTools) > 0 {
		return nil
	}
	switch t.finishReason {
	case "length":
		// Truncated mid-output; the model needs to continue, not run a Bash
		// echo. Nudging here just burns a turn.
		return nil
	case "stop", "":
		// Ambiguous bucket: a clean prose answer (model genuinely done) and a
		// turn that led with a leaked tool call serialized as XML both land here.
		// Only nudge the latter; a turn that produced prose not opening with
		// tool-call markup is a real final answer — including one that opens with
		// visible <think> reasoning, which is text, not a parse failure.
		if t.sawText && !leadsWithToolishMarkup(t.leadingContent.String()) {
			return nil
		}
	}
	if isGemini3xModel(t.requestModel) || isGemini3xModel(t.modelFromUpstream) {
		observability.Get().Debug(
			"AnthropicSSE suppressed text-only-turn nudge on Gemini-3.x (sig-less tool_use would poison next-turn history)",
			"upstream_model", t.modelFromUpstream,
			"request_model", t.requestModel,
		)
		return nil
	}
	blockIdx := t.blockIdx
	id := "toolu_router_nudge_" + t.messageID
	if err := t.emitContentBlockStartTool(blockIdx, id, "Bash", ""); err != nil {
		return err
	}
	args, err := json.Marshal(map[string]string{
		"command":     routerNudgeCommand,
		"description": "router recovery nudge: previous turn had no tool_use",
	})
	if err != nil {
		return err
	}
	if err := t.emitContentBlockDeltaJSON(blockIdx, string(args)); err != nil {
		return err
	}
	if err := t.emitContentBlockStop(blockIdx); err != nil {
		return err
	}
	t.blockIdx++
	t.toolUseEmitted = true
	t.toolUseCount++
	t.nudgeEmitted = true
	observability.Get().Error(
		"AnthropicSSE synthesized text-only-turn recovery nudge",
		"upstream_model", t.modelFromUpstream,
		"request_model", t.requestModel,
	)
	return nil
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
	if t.thinkingOpen {
		if err := t.emitContentBlockStop(t.blockIdx - 1); err != nil {
			return err
		}
		t.thinkingOpen = false
	}
	for _, blockIdx := range t.toolBlocks {
		if err := t.emitValidatedToolArgsDelta(blockIdx); err != nil {
			return err
		}
		if err := t.emitContentBlockStop(blockIdx); err != nil {
			return err
		}
	}
	t.toolBlocks = map[int]int{}

	if err := t.synthesizeTextOnlyTurnNudge(); err != nil {
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
	// message_start usually fires eagerly from Prelude, before any upstream
	// chunk carries an id, so a generated fallback is the common case. It MUST
	// be unique per response: clients (notably ccusage) dedupe usage records
	// by message id, so a constant placeholder collapses every turn of a
	// session into one record and massively undercounts tokens/cost. The
	// "msg_translated_" prefix is kept as a route marker for debugging.
	id := t.messageID
	if id == "" {
		id = "msg_translated_" + randomHex(8)
		t.messageID = id
	}
	observability.Get().Debug("AnthropicSSE emit", "event", "message_start")
	t.bw.WriteString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":")
	sse.WriteJSONString(t.bw, id)
	t.bw.WriteString(",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":")
	sse.WriteJSONString(t.bw, model)
	t.bw.WriteString(",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":")
	sse.WriteJSONInt(t.bw, int64(t.estimatedInputTokens))
	t.bw.WriteString(",\"output_tokens\":0}}}\n\n")
	return t.flushEvent()
}

func (t *AnthropicSSETranslator) emitContentBlockStartText(index int) error {
	observability.Get().Debug("AnthropicSSE emit", "event", "content_block_start", "type", "text")
	t.bw.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString(",\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	return t.flushEvent()
}

// emitContentBlockStartTool emits a tool_use content_block_start. sig, when
// non-empty, is an opaque thought_signature for Gemini 3.x round-trips,
// smuggled as a non-standard field so passthrough clients preserve it.
func (t *AnthropicSSETranslator) emitContentBlockStartTool(index int, id, name, sig string) error {
	observability.Get().Debug("AnthropicSSE emit", "event", "content_block_start", "type", "tool_use", "name", name)
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

func (t *AnthropicSSETranslator) emitContentBlockStartThinking(index int) error {
	observability.Get().Debug("AnthropicSSE emit", "event", "content_block_start", "type", "thinking")
	t.bw.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString(",\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n")
	return t.flushEvent()
}

func (t *AnthropicSSETranslator) emitContentBlockDeltaThinking(index int, text string) error {
	observability.Get().Debug("AnthropicSSE emit", "event", "content_block_delta", "type", "thinking_delta")
	t.bw.WriteString("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString(",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":")
	sse.WriteJSONString(t.bw, text)
	t.bw.WriteString("}}\n\n")
	return t.flushEvent()
}

func (t *AnthropicSSETranslator) emitContentBlockDeltaText(index int, text string) error {
	observability.Get().Debug("AnthropicSSE emit", "event", "content_block_delta", "type", "text_delta")
	t.bw.WriteString("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString(",\"delta\":{\"type\":\"text_delta\",\"text\":")
	sse.WriteJSONString(t.bw, text)
	t.bw.WriteString("}}\n\n")
	return t.flushEvent()
}

func (t *AnthropicSSETranslator) emitContentBlockDeltaJSON(index int, partial string) error {
	observability.Get().Debug("AnthropicSSE emit", "event", "content_block_delta", "type", "input_json_delta")
	t.bw.WriteString("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString(",\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":")
	sse.WriteJSONString(t.bw, partial)
	t.bw.WriteString("}}\n\n")
	return t.flushEvent()
}

func (t *AnthropicSSETranslator) emitContentBlockStop(index int) error {
	observability.Get().Debug("AnthropicSSE emit", "event", "content_block_stop")
	t.bw.WriteString("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString("}\n\n")
	return t.flushEvent()
}

func (t *AnthropicSSETranslator) emitMessageDelta() error {
	stopReason := openAIFinishToAnthropic(t.finishReason)
	// Anthropic invariant: a response containing tool_use blocks MUST report
	// stop_reason="tool_use". OpenAI-compat upstreams (notably GLM-5.1 on
	// DeepInfra/vLLM, plus various Qwen/MiMo serves) sometimes close a tool
	// turn with finish_reason="stop" or "" instead of "tool_calls". Without
	// this promotion, the client receives tool_use blocks alongside
	// stop_reason="end_turn"; Claude Code executes the (often partial-arg)
	// tool_use anyway, gets the same result, and we loop.
	if t.toolUseEmitted {
		stopReason = "tool_use"
	} else if stopReason == "tool_use" {
		// finish_reason="tool_calls" but no tool_use block survived: every
		// tool_call was nameless and dropped (see emitDelta), or the upstream
		// emitted the call as plain text the parser never structured. Relaying
		// stop_reason="tool_use" with zero tool_use blocks dead-ends agent
		// clients — they wait for a tool call that never arrives and the turn
		// stalls, so the user keeps nudging ("keep going") to no effect. The
		// text-only-turn nudge handles the tools-present case before we reach
		// here; this guards the rest (request carried no tools, or the nudge
		// did not fire) so the Anthropic invariant — tool_use stop_reason iff a
		// tool_use block exists — holds in both directions.
		stopReason = "end_turn"
		t.stopReasonDemoted = true
	}
	t.emittedStopReason = stopReason
	observability.Get().Debug("AnthropicSSE emit", "event", "message_delta", "stop_reason", stopReason)
	t.bw.WriteString("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":")
	sse.WriteJSONString(t.bw, stopReason)
	t.bw.WriteString(",\"stop_sequence\":null},\"usage\":{")
	if t.hasUsage {
		// Anthropic's input_tokens is fresh-only; subtract cache so the
		// statusline formula doesn't double-count.
		freshInput := max(0, t.usageInputTokens-t.usageCacheCreationTokens-t.usageCacheReadTokens)
		t.bw.WriteString("\"input_tokens\":")
		sse.WriteJSONInt(t.bw, int64(freshInput))
		t.bw.WriteString(",\"output_tokens\":")
		sse.WriteJSONInt(t.bw, int64(t.usageOutputTokens))
		if t.usageCacheCreationTokens > 0 {
			t.bw.WriteString(",\"cache_creation_input_tokens\":")
			sse.WriteJSONInt(t.bw, int64(t.usageCacheCreationTokens))
		}
		if t.usageCacheReadTokens > 0 {
			t.bw.WriteString(",\"cache_read_input_tokens\":")
			sse.WriteJSONInt(t.bw, int64(t.usageCacheReadTokens))
		}
	} else {
		t.bw.WriteString("\"output_tokens\":0")
	}
	t.bw.WriteString("}}\n\n")
	return t.flushEvent()
}

func (t *AnthropicSSETranslator) emitMessageStop() error {
	observability.Get().Debug("AnthropicSSE emit", "event", "message_stop")
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
