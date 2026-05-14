package translate

import (
	"bufio"
	"bytes"
	"net/http"
	"strings"
	"time"

	"workweave/router/internal/observability/otel"
	"workweave/router/internal/sse"

	"github.com/tidwall/gjson"
)

// GeminiToOpenAISSETranslator translates Gemini :streamGenerateContent SSE
// into OpenAI chat.completion.chunk SSE on the fly. Non-streaming responses
// flush via Finalize.
//
// For Anthropic-inbound → Google, this is chained: the inner sink is an
// AnthropicSSETranslator that re-encodes the OpenAI chunks we emit.
type GeminiToOpenAISSETranslator struct {
	inner   http.ResponseWriter
	flusher http.Flusher
	bw      *bufio.Writer

	streaming  bool
	statusCode int
	buf        bytes.Buffer

	model    string
	chatID   string
	created  int64
	started  bool
	closed   bool
	toolIdx  int
	finished bool
	// pendingSig is a thoughtSignature observed on a leading text part with
	// no functionCall sibling to carry it. Gemini 3.x requires the next turn
	// to round-trip the signature; we smuggle it onto the first tool_call
	// chunk so the downstream Anthropic translator lands it on tool_use.
	pendingSig string

	usageSink otel.UsageSink
}

// NewGeminiToOpenAISSETranslator wraps w. Call Finalize after upstream returns.
func NewGeminiToOpenAISSETranslator(w http.ResponseWriter, model string, sink otel.UsageSink) *GeminiToOpenAISSETranslator {
	flusher, _ := w.(http.Flusher)
	return &GeminiToOpenAISSETranslator{
		inner:     w,
		flusher:   flusher,
		bw:        bufio.NewWriterSize(w, 8192),
		model:     model,
		chatID:    generateChatCmplID(),
		created:   time.Now().Unix(),
		usageSink: sink,
	}
}

func (t *GeminiToOpenAISSETranslator) Header() http.Header {
	return t.inner.Header()
}

func (t *GeminiToOpenAISSETranslator) WriteHeader(code int) {
	t.statusCode = code
	ct := t.inner.Header().Get("Content-Type")
	t.streaming = strings.Contains(ct, "text/event-stream") && code < 400

	t.inner.Header().Del("Content-Length")
	t.inner.Header().Del("Content-Encoding")

	if t.streaming {
		t.inner.Header().Set("Content-Type", "text/event-stream")
		t.inner.WriteHeader(code)
	}
}

func (t *GeminiToOpenAISSETranslator) Write(data []byte) (int, error) {
	n := len(data)
	t.buf.Write(data)
	if !t.streaming {
		return n, nil
	}
	return n, t.processSSEBuffer()
}

func (t *GeminiToOpenAISSETranslator) Flush() {
	if !t.streaming {
		return
	}
	if t.flusher != nil {
		t.flusher.Flush()
	}
}

// Finalize flushes the buffered body for non-streaming responses, or emits
// a trailing [DONE] for streams that ended without a finishReason (defensive
// — Gemini sometimes sends usage in its own chunk).
func (t *GeminiToOpenAISSETranslator) Finalize() error {
	if t.streaming {
		if t.closed {
			return nil
		}
		if !t.finished {
			if err := t.emitFinalChunk("stop", nil); err != nil {
				return err
			}
		}
		return t.emitDone()
	}

	body := t.buf.Bytes()
	if t.statusCode >= 400 {
		t.inner.Header().Set("Content-Type", "application/json")
		t.inner.WriteHeader(t.statusCode)
		_, err := t.inner.Write(GeminiToOpenAIError(body))
		return err
	}

	if t.usageSink != nil {
		usage := gjson.GetBytes(body, "usageMetadata")
		if usage.Exists() {
			t.usageSink.RecordUsage(
				int(usage.Get("promptTokenCount").Int()),
				int(usage.Get("candidatesTokenCount").Int()),
			)
			if cached := int(usage.Get("cachedContentTokenCount").Int()); cached > 0 {
				t.usageSink.RecordCacheUsage(0, cached)
			}
		}
	}

	translated, err := GeminiToOpenAIResponse(body, t.model)
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

func (t *GeminiToOpenAISSETranslator) processSSEBuffer() error {
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

func (t *GeminiToOpenAISSETranslator) translateEvent(raw []byte) error {
	_, data := sse.ParseEvent(raw)
	if len(data) == 0 {
		return nil
	}

	if !t.started {
		if err := t.emitFirstChunk(); err != nil {
			return err
		}
		t.started = true
	}

	candidate := gjson.GetBytes(data, "candidates.0")
	parts := candidate.Get("content.parts")
	if parts.IsArray() {
		var emitErr error
		parts.ForEach(func(_, part gjson.Result) bool {
			if fc := part.Get("functionCall"); fc.Exists() {
				name := fc.Get("name").String()
				argsRaw := fc.Get("args").Raw
				if argsRaw == "" {
					argsRaw = "{}"
				}
				sig := part.Get("thoughtSignature").String()
				if sig == "" && t.pendingSig != "" {
					sig = t.pendingSig
				}
				t.pendingSig = ""
				if err := t.emitToolCallChunk(t.toolIdx, name, argsRaw, sig); err != nil {
					emitErr = err
					return false
				}
				t.toolIdx++
				return true
			}
			if text := part.Get("text"); text.Exists() && text.String() != "" {
				if sig := part.Get("thoughtSignature").String(); sig != "" && t.pendingSig == "" {
					t.pendingSig = sig
				}
				if err := t.emitTextDelta(text.String()); err != nil {
					emitErr = err
					return false
				}
			}
			return true
		})
		if emitErr != nil {
			return emitErr
		}
	}

	usage := geminiUsageFromBytes(data)
	finishReason := candidate.Get("finishReason").String()
	if finishReason != "" {
		mapped := mapGeminiFinishReason(finishReason, t.toolIdx > 0)
		if err := t.emitFinalChunk(mapped, usage); err != nil {
			return err
		}
		t.finished = true
		return t.emitDone()
	}
	if usage != nil {
		// Gemini sometimes sends usage in a trailing chunk without a
		// finishReason; emit a usage-only chunk so downstream consumers
		// see it before [DONE].
		if err := t.emitUsageOnlyChunk(usage); err != nil {
			return err
		}
	}
	return nil
}

func geminiUsageFromBytes(data []byte) map[string]int {
	r := gjson.GetBytes(data, "usageMetadata")
	if !r.Exists() {
		return nil
	}
	prompt := int(r.Get("promptTokenCount").Int())
	completion := int(r.Get("candidatesTokenCount").Int())
	total := int(r.Get("totalTokenCount").Int())
	cached := int(r.Get("cachedContentTokenCount").Int())
	if total == 0 {
		total = prompt + completion
	}
	return map[string]int{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      total,
		"cached_tokens":     cached,
	}
}

func (t *GeminiToOpenAISSETranslator) emitFirstChunk() error {
	t.writeChunkHeader()
	t.bw.WriteString(`"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`)
	t.bw.WriteString("\n\n")
	return t.flushEvent()
}

func (t *GeminiToOpenAISSETranslator) emitTextDelta(text string) error {
	t.writeChunkHeader()
	t.bw.WriteString(`"choices":[{"index":0,"delta":{"content":`)
	sse.WriteJSONString(t.bw, text)
	t.bw.WriteString(`},"finish_reason":null}]}`)
	t.bw.WriteString("\n\n")
	return t.flushEvent()
}

// emitToolCallChunk emits a single OpenAI tool_calls delta carrying name and
// the full arguments JSON in one chunk; Gemini does not split functionCall
// args across chunks. thoughtSignature is smuggled as
// function.thought_signature (off-spec but preserved by passthrough clients)
// so the next request can round-trip it.
func (t *GeminiToOpenAISSETranslator) emitToolCallChunk(idx int, name, argsRaw, sig string) error {
	id := embedSignatureInID(generateToolCallID(), sig)
	t.writeChunkHeader()
	t.bw.WriteString(`"choices":[{"index":0,"delta":{"tool_calls":[{"index":`)
	sse.WriteJSONInt(t.bw, int64(idx))
	t.bw.WriteString(`,"id":`)
	sse.WriteJSONString(t.bw, id)
	t.bw.WriteString(`,"type":"function","function":{"name":`)
	sse.WriteJSONString(t.bw, name)
	t.bw.WriteString(`,"arguments":`)
	// arguments must be a JSON-encoded string in OpenAI's wire format.
	sse.WriteJSONString(t.bw, argsRaw)
	if sig != "" {
		t.bw.WriteString(`,"thought_signature":`)
		sse.WriteJSONString(t.bw, sig)
	}
	t.bw.WriteString(`}}]},"finish_reason":null}]}`)
	t.bw.WriteString("\n\n")
	return t.flushEvent()
}

func (t *GeminiToOpenAISSETranslator) emitFinalChunk(finishReason string, usage map[string]int) error {
	t.writeChunkHeader()
	t.bw.WriteString(`"choices":[{"index":0,"delta":{},"finish_reason":`)
	sse.WriteJSONString(t.bw, finishReason)
	t.bw.WriteString(`}]`)
	if usage != nil {
		t.writeUsageJSON(usage)
		t.recordUsageOnSink(usage)
	}
	t.bw.WriteString("}\n\n")
	return t.flushEvent()
}

func (t *GeminiToOpenAISSETranslator) emitUsageOnlyChunk(usage map[string]int) error {
	t.writeChunkHeader()
	t.bw.WriteString(`"choices":[]`)
	t.writeUsageJSON(usage)
	t.bw.WriteString("}\n\n")
	t.recordUsageOnSink(usage)
	return t.flushEvent()
}

// writeUsageJSON serializes an OpenAI-shape usage object onto t.bw, including
// prompt_tokens_details.cached_tokens when Gemini reported a cached prefix.
// The nested shape matches what AnthropicSSETranslator.extractAndForwardUsage
// reads via gjson, so cache numbers propagate through the chain.
func (t *GeminiToOpenAISSETranslator) writeUsageJSON(usage map[string]int) {
	t.bw.WriteString(`,"usage":{"prompt_tokens":`)
	sse.WriteJSONInt(t.bw, int64(usage["prompt_tokens"]))
	t.bw.WriteString(`,"completion_tokens":`)
	sse.WriteJSONInt(t.bw, int64(usage["completion_tokens"]))
	t.bw.WriteString(`,"total_tokens":`)
	sse.WriteJSONInt(t.bw, int64(usage["total_tokens"]))
	if cached := usage["cached_tokens"]; cached > 0 {
		t.bw.WriteString(`,"prompt_tokens_details":{"cached_tokens":`)
		sse.WriteJSONInt(t.bw, int64(cached))
		t.bw.WriteByte('}')
	}
	t.bw.WriteByte('}')
}

func (t *GeminiToOpenAISSETranslator) recordUsageOnSink(usage map[string]int) {
	if t.usageSink == nil {
		return
	}
	t.usageSink.RecordUsage(usage["prompt_tokens"], usage["completion_tokens"])
	if cached := usage["cached_tokens"]; cached > 0 {
		t.usageSink.RecordCacheUsage(0, cached)
	}
}

func (t *GeminiToOpenAISSETranslator) emitDone() error {
	t.bw.WriteString("data: [DONE]\n\n")
	t.closed = true
	return t.flushEvent()
}

func (t *GeminiToOpenAISSETranslator) writeChunkHeader() {
	t.bw.WriteString(`data: {"id":`)
	sse.WriteJSONString(t.bw, t.chatID)
	t.bw.WriteString(`,"object":"chat.completion.chunk","created":`)
	sse.WriteJSONInt(t.bw, t.created)
	t.bw.WriteString(`,"model":`)
	sse.WriteJSONString(t.bw, t.model)
	t.bw.WriteByte(',')
}

func (t *GeminiToOpenAISSETranslator) flushEvent() error {
	if err := t.bw.Flush(); err != nil {
		return err
	}
	if t.flusher != nil {
		t.flusher.Flush()
	}
	return nil
}

var _ http.ResponseWriter = (*GeminiToOpenAISSETranslator)(nil)
var _ http.Flusher = (*GeminiToOpenAISSETranslator)(nil)
