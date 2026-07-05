package translate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"workweave/router/internal/observability"
	"workweave/router/internal/providers"
	"workweave/router/internal/sse"
	"workweave/router/internal/translate/toolcheck"

	"github.com/tidwall/gjson"
)

var _ providers.OutputProgressArmer = (*AnthropicSSETranslator)(nil)

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

	usageSink   UsageSink
	inputTokens int // persists input token count from message_start for use in handleMessageDelta
}

// NewSSETranslator wraps w. Call Finalize after upstream returns.
func NewSSETranslator(w http.ResponseWriter, model string, sink UsageSink) *SSETranslator {
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
		t.inputTokens = int(inputTokens)
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
		outputTokens := usageResult.Get("output_tokens").Int()
		t.bw.WriteString(`,"usage":{"prompt_tokens":`)
		sse.WriteJSONInt(t.bw, int64(t.inputTokens))
		t.bw.WriteString(`,"completion_tokens":`)
		sse.WriteJSONInt(t.bw, outputTokens)
		t.bw.WriteString(`,"total_tokens":`)
		sse.WriteJSONInt(t.bw, int64(t.inputTokens)+outputTokens)
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
	return sse.FlushWriter(t.bw, t.flusher)
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
	// thinkingOpen tracks an open thinking block for upstream reasoning
	// (OpenRouter `reasoning`, DeepSeek/Qwen `reasoning_content`) — without it,
	// reasoning leaks into the visible text channel.
	thinkingOpen bool
	// pendingText buffers whitespace-only deltas that arrive before real text.
	// DeepSeek-v4 et al emit a bare "\n\n" between reasoning and tool_calls;
	// opening a text block for it would render as an empty block. Flushed once
	// real text arrives, or dropped if only whitespace ever comes.
	pendingText strings.Builder
	toolBlocks  map[int]int
	// suppressedTools holds tool_call indices refused for carrying no function
	// name (see emitDelta); later argument fragments reuse the index and must
	// be dropped too.
	suppressedTools map[int]struct{}
	// toolArgsBuffer accumulates input_json_delta fragments per content-block
	// index so the concatenated args can be validated at content_block_stop.
	// OpenAI-compat upstreams (GLM, Kimi, Qwen, gpt-oss) intermittently emit
	// JSON that fails to parse — partial keys, unbalanced braces, truncation.
	toolArgsBuffer map[int]*strings.Builder
	// toolArgsInvalid latches per-block-index when buffered args fail JSON
	// parsing at content_block_stop. Surfaced via Summary.
	toolArgsInvalid map[int]struct{}
	// toolNames records each block's function name for schema validation at
	// content_block_stop.
	toolNames map[int]string
	// toolValidator validates/repairs buffered tool args (see toolcheck). Nil
	// means syntax-check-only.
	toolValidator *toolcheck.Validator
	// toolCallIssues collects validation/repair findings for Summary.
	toolCallIssues []toolcheck.Issue
	// toolUseEmitted latches on first tool_use block opened. finishStream
	// clears toolBlocks before emitMessageDelta, so this outlives the map.
	toolUseEmitted bool
	// toolUseCount counts tool_use blocks opened; like toolUseEmitted it
	// outlives toolBlocks for post-stream reporting.
	toolUseCount int
	// emittedStopReason is the stop_reason emitMessageDelta actually wrote
	// (post tool_use promotion), surfaced via Summary.
	emittedStopReason string

	finishReason             string
	usageInputTokens         int
	usageOutputTokens        int
	usageCacheCreationTokens int
	usageCacheReadTokens     int
	hasUsage                 bool
	messageID                string
	modelFromUpstream        string

	usageSink UsageSink

	// onOutputProgress fires on every output-bearing delta (text, reasoning,
	// tool-call args, terminal finish), never on keepalives/empty deltas. Feeds
	// the output-progress watchdog (httputil.DefaultOutputStallTimeout). nil
	// disables it.
	onOutputProgress func()

	// estimatedInputTokens seeds message_start.usage.input_tokens for
	// cross-format paths where real usage arrives later, at message_delta.
	estimatedInputTokens int

	// routingMarker is emitted as a standalone text block at index 0.
	routingMarker string
	markerEmitted bool

	// requestHadTools is true when the inbound request carried tools. Used at
	// finishStream to detect "model produced no tool_use though tools were
	// available" (Gemini-3.1, Mimo-v2.5 sometimes emit prose/<think> instead)
	// and synthesize a recovery nudge.
	requestHadTools bool

	// thinkTagReasoning reroutes a leading <think>…</think> in content into
	// thinking blocks, for upstreams (e.g. mimo-v2.5-pro) that stream
	// chain-of-thought as inline tags instead of reasoning_content/reasoning.
	// Off by default.
	thinkTagReasoning bool
	splitter          thinkTagSplitter

	// escapeNormalize repairs literal `\n`/`\t`/`\r` sequences that upstream
	// models occasionally double-escape (`\\n` on the wire) in file-edit tool
	// args (Edit/Write/MultiEdit). Off by default: the transform can corrupt
	// legitimate source containing literal `\n`/`\t` (e.g. a Python string
	// `"\\n"`).
	escapeNormalize bool

	// nudgeEmitted latches when finishStream emits the text-only recovery
	// nudge, surfaced via Summary.
	nudgeEmitted bool

	// sawText latches once any non-empty text delta is emitted, so
	// synthesizeTextOnlyTurnNudge can distinguish a real tool-free answer from
	// a turn that emitted nothing.
	sawText bool

	// leadingContent keeps up to leadingContentCap bytes of the turn's opening
	// text so the nudge can check whether it *leads* with tool-call/reasoning
	// markup (a model leaking <think> or an unstructured tool call instead of
	// routing to the proper channel). Anchored to the start — a legitimate
	// answer that merely mentions "<think>" mid-prose must not trip it.
	leadingContent strings.Builder

	// stopReasonDemoted latches when emitMessageDelta demotes a
	// finish_reason="tool_calls" turn to end_turn because no tool_use block
	// survived — the degenerate case that used to dead-end agent clients.
	stopReasonDemoted bool

	// upstreamErrorStatus is set when WriteHeader sees a >=400 status after the
	// Prelude already committed SSE headers + message_start (wire status can no
	// longer change). Without it the error body would parse as empty SSE and
	// finishStream would emit a clean end_turn, masking the failure. When set,
	// Write diverts the body into upstreamErrorBody and finishStream emits an
	// Anthropic `error` event instead.
	upstreamErrorStatus int
	upstreamErrorBody   strings.Builder
}

// upstreamErrorBodyCap bounds how much of an upstream error body is retained
// for the surfaced error message, so a pathological body can't grow unbounded.
const upstreamErrorBodyCap = 8 << 10

// NewAnthropicSSETranslator wraps w. Call Finalize after upstream returns.
func NewAnthropicSSETranslator(w http.ResponseWriter, requestModel string, sink UsageSink) *AnthropicSSETranslator {
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

// WithToolValidator installs the compiled tool-schema validator so buffered
// tool args are validated (and repaired) before emission. Pass nil to disable.
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

// WithRequestHadTools tells the translator whether the inbound request
// carried tools. Enables the finishStream recovery nudge for prose-only
// turns — the dominant empty-patch failure on Gemini-3.1-Pro/Mimo-v2.5-Pro
// after PRs #280/#281.
func (t *AnthropicSSETranslator) WithRequestHadTools(hadTools bool) *AnthropicSSETranslator {
	t.requestHadTools = hadTools
	return t
}

// WithThinkTagReasoning enables rerouting of a leading <think>…</think> into
// thinking blocks. Only for upstreams (e.g. xiaomi/mimo-v2.5-pro) that stream
// chain-of-thought as inline content tags rather than reasoning_content.
func (t *AnthropicSSETranslator) WithThinkTagReasoning(on bool) *AnthropicSSETranslator {
	t.thinkTagReasoning = on
	return t
}

// WithEscapeNormalize enables the escape-repair pass on file-edit tool
// (Edit/Write/MultiEdit) args for the buffered non-streaming response path.
func (t *AnthropicSSETranslator) WithEscapeNormalize(on bool) *AnthropicSSETranslator {
	t.escapeNormalize = on
	return t
}

// ArmOutputProgress installs mark to fire on output-bearing upstream deltas
// (text, reasoning, tool-call args, terminal finish), never on keepalives —
// so the watchdog tracks time-since-last-output, not time-since-last-byte.
// Returns false when the client isn't streaming, since the buffered path only
// translates at Finalize and would false-trip. Call after Prelude/WriteHeader.
func (t *AnthropicSSETranslator) ArmOutputProgress(mark func()) (armed bool) {
	if !t.streaming {
		return false
	}
	t.onOutputProgress = mark
	return true
}

func (t *AnthropicSSETranslator) markOutputProgress() {
	if t.onOutputProgress != nil {
		t.onOutputProgress()
	}
}

// ResponseSummary reports translated-response signals for post-stream
// logging: what the upstream returned and whether stop_reason promotion
// (see emitMessageDelta) fired for this turn.
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
	// InvalidToolArgsBlocks counts tool_use blocks whose buffered args failed
	// JSON validation at content_block_stop — a malformed upstream payload.
	InvalidToolArgsBlocks int
	// TextOnlyTurnNudged is true when finishStream synthesized a Bash recovery
	// nudge for a tool-free turn on a request that had tools. See
	// synthesizeTextOnlyTurnNudge.
	TextOnlyTurnNudged bool
	// StopReasonDemoted is true when a finish_reason="tool_calls" turn was
	// demoted to end_turn because no tool_use block survived (see
	// emitMessageDelta) — the degenerate case that used to dead-end clients.
	StopReasonDemoted bool
	// SuppressedToolCalls counts tool_calls dropped for carrying no function
	// name (see emitDelta). Distinguishes the nameless-call case from the
	// call-emitted-as-text case when paired with StopReasonDemoted.
	SuppressedToolCalls int
	// ToolCallIssues lists every tool_use block that failed toolcheck
	// validation, including ones repair recovered. Logged as
	// router.tool_call_invalid per entry.
	ToolCallIssues []toolcheck.Issue
	// OutputTokens is the upstream completion_tokens count, when reported.
	OutputTokens int
	// InputTokens is the upstream prompt_tokens count, when reported.
	InputTokens int
	// CacheReadTokens is cache_read_input_tokens (Anthropic) or cached_tokens
	// (OpenAI), when reported.
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
	// Capture the error status even if Prelude already emitted headers (status
	// is then dropped from the wire); finishStream needs it to surface an
	// `error` event instead of a clean end_turn.
	if code >= 400 {
		t.upstreamErrorStatus = code
	}
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

// Prelude commits SSE headers and emits message_start (+ routing marker)
// immediately so the client sees the message envelope while upstream is
// still doing prefill. Call after the routing decision when streaming=true.
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
	// Once streaming and an error status has been seen, subsequent bytes are
	// the upstream error body, not SSE content — divert them (bounded) so
	// finishStream can surface an `error` event instead of feeding a non-SSE
	// body to the SSE parser (which would silently yield nothing).
	if t.upstreamErrorStatus != 0 && t.streaming {
		if remaining := upstreamErrorBodyCap - t.upstreamErrorBody.Len(); remaining > 0 {
			capped := data
			if len(capped) > remaining {
				capped = capped[:remaining]
			}
			t.upstreamErrorBody.Write(capped)
		}
		return n, nil
	}
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

	translated, issues, err := openAIToAnthropicResponse(body, t.requestModel, t.toolValidator, t.thinkTagReasoning, t.escapeNormalize)
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

	// strings.Clone: gjson strings are unsafe-backed by the buffer; these
	// outlive the event, so copy them.
	//
	// Check Str != "" not Exists(): some upstreams (e.g. OpenRouter) send
	// early frames with `"id": ""` before settling on a real id; Exists()
	// would latch the empty value and block the later real id. Same for model.
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
		// Terminal output: the upstream signaled the turn is ending.
		t.markOutputProgress()
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

// appendThinking emits text into a thinking block, closing an open text block
// first if needed. Shared by the reasoning_content branch and the
// <think>-tag splitter.
func (t *AnthropicSSETranslator) appendThinking(text string) error {
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
	return t.emitContentBlockDeltaThinking(t.blockIdx-1, text)
}

// appendText emits text into a text content block. Whitespace-only content
// with no block open yet is buffered in pendingText (see field doc) rather
// than opening an empty block; whitespace inside an already-open block emits
// normally as legitimate formatting.
func (t *AnthropicSSETranslator) appendText(content string) error {
	if !t.textOpen && strings.TrimSpace(content) == "" {
		t.pendingText.WriteString(content)
		return nil
	}
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
	return t.emitContentBlockDeltaText(t.blockIdx-1, content)
}

func (t *AnthropicSSETranslator) emitDelta(delta gjson.Result) error {
	// Reasoning arrives as `reasoning` (OpenRouter) or `reasoning_content`
	// (DeepSeek/Qwen native); either way it must surface as a thinking block.
	reasoning := delta.Get("reasoning_content").Str
	if reasoning == "" {
		reasoning = delta.Get("reasoning").Str
	}
	if reasoning != "" {
		// Streamed reasoning is real upstream output (rendered as a thinking
		// block), so it counts as output progress and resets the stall watchdog.
		t.markOutputProgress()
		if err := t.appendThinking(reasoning); err != nil {
			return err
		}
	}

	if content := delta.Get("content").Str; content != "" {
		// Even whitespace-only content is real output, distinct from a keepalive.
		t.markOutputProgress()
		if t.thinkTagReasoning {
			// Reroute a leading <think>…</think> into thinking blocks; the
			// splitter passes everything else through as text (see think_tag.go).
			for _, seg := range t.splitter.Feed(content) {
				switch seg.kind {
				case segThinking:
					if err := t.appendThinking(seg.text); err != nil {
						return err
					}
				default:
					if err := t.appendText(seg.text); err != nil {
						return err
					}
				}
			}
		} else if err := t.appendText(content); err != nil {
			return err
		}
	}

	// GLM-5.1 (and other vLLM/SGLang upstreams) emit `"tool_calls": null` on
	// plain-text deltas. gjson's ForEach over null yields one zero-value
	// iteration (index=0, name=""), which would spuriously trip the
	// nameless-call guard below and drop the real tool_call that later arrives
	// at index 0. Only iterate real arrays.
	toolCalls := delta.Get("tool_calls")
	if !toolCalls.IsArray() {
		return nil
	}
	t.markOutputProgress()

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
			// A tool_call with no function name (GLM/Qwen/Kimi/gpt-oss
			// occasionally emit one) would make the client invoke tool "" and
			// loop. Drop it before closing text/thinking so it can't truncate
			// a legitimate text block; the turn ends on its real stop_reason.
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
			// Buffer only — one input_json_delta is emitted per tool block at
			// content_block_stop, after validation. Malformed upstream args
			// then degrade to `{}` instead of the client's strict parser
			// rejecting the whole turn. Costs one delta's worth of latency;
			// tool args are typically <1KB.
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

// emitValidatedToolArgsDelta emits one input_json_delta per tool block at
// content_block_stop, after toolcheck validation/repair. No-op if no args
// were buffered. Unparseable args degrade to `{}` so the client can still
// dispatch the tool (it then errors on missing params) rather than the whole
// turn failing to parse; schema mismatches repair can't fix forward as-is.
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
// produced no tool_use on a request that had tools. Bash is used because
// every Claude Code request includes it, so the fabricated call is always
// dispatchable; the echo surfaces as a tool_result nudging the model to act.
const routerNudgeCommand = "echo '[router] previous turn produced no tool_use; use Edit/Write/Read/Bash/Grep — do not respond with prose or thinking tags only.'"

// leadingContentCap bounds retained opening text for leadsWithToolishMarkup —
// enough for any marker plus leading whitespace.
const leadingContentCap = 64

// toolishMarkupMarkers are opening tokens of a tool call leaked into the
// content channel as XML instead of a structured tool_use block; Claude
// Code's strict parser rejects these and dead-ends the turn, which the nudge
// rescues. Matched only at the turn's start (see leadsWithToolishMarkup) so a
// legitimate answer that discusses these tags mid-prose isn't misflagged.
//
// Reasoning markup (<think>) is deliberately excluded: models like Mimo-v2.5
// stream visible chain-of-thought as <think>…</think> then a real answer with
// finish_reason="stop" — a valid turn, not a parse failure. Nudging on it
// looped the session (echo -> re-pin -> another <think>+answer -> repeat).
var toolishMarkupMarkers = []string{"<tool_call", "<function", "<invoke"}

// leadsWithToolishMarkup reports whether content opens (ignoring leading
// whitespace) with tool-call markup — anchored to the start so a substring
// scan doesn't misfire on prose that merely mentions the tag.
func leadsWithToolishMarkup(content string) bool {
	s := strings.TrimLeft(content, " \t\r\n")
	for _, m := range toolishMarkupMarkers {
		if strings.HasPrefix(s, m) {
			return true
		}
	}
	return false
}

// synthesizeTextOnlyTurnNudge fabricates a Bash tool_use block when the
// upstream produced no tool_use on a request that had tools. Targets
// Gemini-3.1-Pro/Mimo-v2.5-Pro sometimes emitting prose+XML thinking tags as
// plain text, which Claude Code's strict parser rejects, killing the session.
// The synthetic Bash echo turns that dead-end into a normal tool_use turn
// the client can dispatch, keeping the agentic loop alive.
//
// No-op if a tool_use was already emitted, the request had no tools, or
// nothing has streamed yet.
//
// Final-answer guard: suppressed when finish_reason is "stop"/"" and the text
// is clean prose (no tool-call markup) — that's a legitimate finished answer,
// and nudging it would revive an already-done turn. finish_reason="length"
// never nudges (truncation, not a parse failure).
//
// Gemini-3.x is excluded entirely: the synthesized tool_use has no
// thoughtSignature, and Gemini-3.x requires one on every functionCall across
// turns. A sig-less nudge makes writeGeminiFromAnthropic drop the whole
// tool_use/tool_result history (emit_gemini.go dropToolBlocks), wiping prior
// context and looping the model to the turn ceiling — empirically far worse
// (≥90-turn loops 4→46 in the v0.59 bake-off). OpenAI-compat models this was
// built for have no such guard and still benefit.
func (t *AnthropicSSETranslator) synthesizeTextOnlyTurnNudge() error {
	if t.toolUseEmitted || !t.requestHadTools || !t.started {
		return nil
	}
	// A structured tool call was already dropped as malformed (see
	// suppressedTools); nudging on top would re-add a tool_use the model
	// never sent and loop degenerate-shape upstreams to the turn ceiling.
	if len(t.suppressedTools) > 0 {
		return nil
	}
	switch t.finishReason {
	case "length":
		// Truncated mid-output — needs continuation, not a Bash echo.
		return nil
	case "stop", "":
		// Ambiguous: a genuine finished answer and a leaked-tool-call-as-XML
		// turn both land here. Only nudge the latter.
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
	if t.thinkTagReasoning {
		// Drain any buffered <think> content (an unclosed tag surfaces as
		// thinking; a buffered partial open-tag surfaces as text).
		for _, seg := range t.splitter.Flush() {
			switch seg.kind {
			case segThinking:
				if err := t.appendThinking(seg.text); err != nil {
					return err
				}
			default:
				if err := t.appendText(seg.text); err != nil {
					return err
				}
			}
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

	// Upstream error seen after Prelude committed: surface it as an `error`
	// event, not a clean end_turn (which agent harnesses would silently accept
	// as an empty turn).
	if t.upstreamErrorStatus != 0 {
		if err := t.emitErrorEvent(); err != nil {
			return err
		}
		if err := t.emitMessageStop(); err != nil {
			return err
		}
		t.closed = true
		return nil
	}

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

// emitErrorEvent emits an Anthropic `error` event from the captured upstream
// status/body, so clients treat it as a retryable turn error, not a success.
func (t *AnthropicSSETranslator) emitErrorEvent() error {
	errType := anthropicErrorTypeForStatus(t.upstreamErrorStatus)
	msg := upstreamErrorMessage(t.upstreamErrorBody.String(), t.upstreamErrorStatus)
	observability.Get().Debug("AnthropicSSE emit",
		"event", "error",
		"upstream_status", t.upstreamErrorStatus,
		"error_type", errType,
	)
	t.bw.WriteString("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":")
	sse.WriteJSONString(t.bw, errType)
	t.bw.WriteString(",\"message\":")
	sse.WriteJSONString(t.bw, msg)
	t.bw.WriteString("}}\n\n")
	return t.flushEvent()
}

// anthropicErrorTypeForStatus maps an upstream HTTP status to the closest
// Anthropic error type so clients apply the right retry/backoff semantics.
func anthropicErrorTypeForStatus(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusServiceUnavailable:
		return "overloaded_error"
	case status >= 500:
		return "api_error"
	case status == http.StatusBadRequest:
		return "invalid_request_error"
	default:
		return "api_error"
	}
}

// upstreamErrorMessage extracts a message from an OpenAI- or Anthropic-shaped
// error body, falling back to a generic status line.
func upstreamErrorMessage(body string, status int) string {
	if body != "" {
		if m := gjson.Get(body, "error.message"); m.Exists() && m.String() != "" {
			return m.String()
		}
		if m := gjson.Get(body, "message"); m.Exists() && m.String() != "" {
			return m.String()
		}
	}
	return fmt.Sprintf("upstream provider returned HTTP %d", status)
}

func (t *AnthropicSSETranslator) emitMessageStart() error {
	model := t.modelFromUpstream
	if model == "" {
		model = t.requestModel
	}
	// Fires eagerly from Prelude before any upstream id arrives, so a
	// generated fallback is the common case. Must be unique per response:
	// clients like ccusage dedupe usage records by message id, so a constant
	// placeholder would collapse every turn into one record.
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
// non-empty, is a Gemini 3.x thoughtSignature smuggled into the tool id
// (embedSignatureInID) rather than an off-spec block field, which Anthropic
// upstreams reject with a 400 when the history routes back to them.
// embedSignatureInID is idempotent.
func (t *AnthropicSSETranslator) emitContentBlockStartTool(index int, id, name, sig string) error {
	observability.Get().Debug("AnthropicSSE emit", "event", "content_block_start", "type", "tool_use", "name", name)
	t.bw.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":")
	sse.WriteJSONInt(t.bw, int64(index))
	t.bw.WriteString(",\"content_block\":{\"type\":\"tool_use\",\"id\":")
	sse.WriteJSONString(t.bw, embedSignatureInID(sanitizeToolUseID(id), sig))
	t.bw.WriteString(",\"name\":")
	sse.WriteJSONString(t.bw, name)
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
	// Anthropic invariant: tool_use blocks require stop_reason="tool_use".
	// Some OpenAI-compat upstreams (GLM-5.1, Qwen, MiMo) close a tool turn
	// with finish_reason="stop"/"" instead of "tool_calls" — promote it.
	if t.toolUseEmitted {
		stopReason = "tool_use"
	} else if stopReason == "tool_use" {
		// finish_reason="tool_calls" but no tool_use block survived (all
		// nameless-dropped, or emitted as unstructured text). Relaying
		// stop_reason="tool_use" with zero blocks dead-ends agent clients
		// waiting on a call that never arrives, so demote to end_turn —
		// keeping the invariant symmetric in both directions.
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
	return sse.FlushWriter(t.bw, t.flusher)
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
