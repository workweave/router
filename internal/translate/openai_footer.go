package translate

import (
	"bufio"
	"bytes"
	"net/http"
	"strings"
	"time"

	"workweave/router/internal/sse"

	"github.com/tidwall/gjson"
)

// OpenAIRoutingFooterWriter wraps an http.ResponseWriter and appends a footer
// chunk at the END of an OpenAI-format SSE stream — as a delta-content
// chat.completion.chunk emitted just before the chunk that carries
// finish_reason — so any client that renders the answer also renders a
// post-answer affordance (a feedback thumb).
//
// It is the symmetric end-of-stream counterpart to OpenAIRoutingMarkerWriter's
// leading chunk. Non-streaming responses and an empty footer pass through
// untouched, and the footer is injected only when the turn finishes naturally
// (finish_reason == "stop"), so tool_calls / length turns stay clean.
type OpenAIRoutingFooterWriter struct {
	inner   http.ResponseWriter
	flusher http.Flusher
	bw      *bufio.Writer

	footer string

	buf bytes.Buffer

	streaming      bool
	headersEmitted bool
	footerEmitted  bool
}

// NewOpenAIRoutingFooterWriter wraps w so footer is appended as a trailing
// content chunk at the end of a streamed OpenAI response. If footer is empty,
// all writes pass through unchanged.
func NewOpenAIRoutingFooterWriter(w http.ResponseWriter, footer string) *OpenAIRoutingFooterWriter {
	flusher, _ := w.(http.Flusher)
	return &OpenAIRoutingFooterWriter{
		inner:   w,
		flusher: flusher,
		bw:      bufio.NewWriterSize(w, 4096),
		footer:  footer,
	}
}

func (w *OpenAIRoutingFooterWriter) Header() http.Header {
	return w.inner.Header()
}

func (w *OpenAIRoutingFooterWriter) WriteHeader(code int) {
	if w.headersEmitted {
		return
	}
	ct := w.inner.Header().Get("Content-Type")
	w.streaming = strings.Contains(ct, "text/event-stream") && code < 400
	w.headersEmitted = true
	w.inner.WriteHeader(code)
}

func (w *OpenAIRoutingFooterWriter) Write(data []byte) (int, error) {
	if !w.streaming || w.footer == "" {
		return w.inner.Write(data)
	}
	return w.processUpstream(data)
}

// Flush implements http.Flusher.
func (w *OpenAIRoutingFooterWriter) Flush() {
	if w.flusher != nil {
		w.flusher.Flush()
	}
}

// processUpstream forwards OpenAI SSE chunks in order, injecting the footer chunk
// immediately before the first chunk whose choices[0].finish_reason is "stop".
func (w *OpenAIRoutingFooterWriter) processUpstream(data []byte) (int, error) {
	w.buf.Write(data)
	for {
		event, n := sse.SplitNext(w.buf.Bytes())
		if n == 0 {
			break
		}
		_, payload := sse.ParseEvent(event)

		if !w.footerEmitted && w.shouldInject(payload) {
			w.emitFooterChunk(payload)
			w.footerEmitted = true
		}
		w.bw.Write(event[:n])
		w.buf.Next(n)
	}
	if err := w.bw.Flush(); err != nil {
		return 0, err
	}
	if w.flusher != nil {
		w.flusher.Flush()
	}
	return len(data), nil
}

// shouldInject reports whether payload is the terminating chunk of a naturally
// finished turn ("[DONE]" carries no JSON, so it never matches).
func (w *OpenAIRoutingFooterWriter) shouldInject(payload []byte) bool {
	if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
		return false
	}
	return gjson.GetBytes(payload, "choices.0.finish_reason").String() == "stop"
}

// emitFooterChunk writes a chat.completion.chunk carrying the footer as
// assistant delta content. It reuses the id/model from the finish chunk so the
// injected chunk is indistinguishable from upstream framing.
func (w *OpenAIRoutingFooterWriter) emitFooterChunk(finishChunk []byte) {
	id := gjson.GetBytes(finishChunk, "id").String()
	if id == "" {
		id = generateChatCmplID()
	}
	model := gjson.GetBytes(finishChunk, "model").String()

	w.bw.WriteString(`data: {"id":`)
	sse.WriteJSONString(w.bw, id)
	w.bw.WriteString(`,"object":"chat.completion.chunk","created":`)
	sse.WriteJSONInt(w.bw, time.Now().Unix())
	w.bw.WriteString(`,"model":`)
	sse.WriteJSONString(w.bw, model)
	w.bw.WriteString(`,"choices":[{"index":0,"delta":{"content":`)
	sse.WriteJSONString(w.bw, w.footer)
	w.bw.WriteString(`},"finish_reason":null}]}`)
	w.bw.WriteString("\n\n")
}

var _ http.ResponseWriter = (*OpenAIRoutingFooterWriter)(nil)
var _ http.Flusher = (*OpenAIRoutingFooterWriter)(nil)
