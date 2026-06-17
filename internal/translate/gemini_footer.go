package translate

import (
	"bufio"
	"bytes"
	"net/http"
	"strings"

	"workweave/router/internal/sse"

	"github.com/tidwall/gjson"
)

// GeminiRoutingFooterWriter wraps an http.ResponseWriter and appends a footer
// chunk at the END of a Gemini-format SSE stream — as a model text part emitted
// just before the chunk carrying finishReason — so any client that renders the
// answer also renders a post-answer affordance (a feedback thumb).
//
// It is the symmetric end-of-stream counterpart to GeminiRoutingMarkerWriter's
// leading chunk. Non-streaming responses and an empty footer pass through
// untouched. The footer is injected only when the turn ends naturally
// (finishReason == "STOP") AND emitted no functionCall part, so tool-call turns
// (intermediate agent steps) stay clean.
type GeminiRoutingFooterWriter struct {
	inner   http.ResponseWriter
	flusher http.Flusher
	bw      *bufio.Writer

	footer string

	buf bytes.Buffer

	streaming       bool
	headersEmitted  bool
	footerEmitted   bool
	sawFunctionCall bool
}

// NewGeminiRoutingFooterWriter wraps w so footer is appended as a trailing text
// part at the end of a streamed Gemini response. If footer is empty, all writes
// pass through unchanged.
func NewGeminiRoutingFooterWriter(w http.ResponseWriter, footer string) *GeminiRoutingFooterWriter {
	flusher, _ := w.(http.Flusher)
	return &GeminiRoutingFooterWriter{
		inner:   w,
		flusher: flusher,
		bw:      bufio.NewWriterSize(w, 4096),
		footer:  footer,
	}
}

func (w *GeminiRoutingFooterWriter) Header() http.Header {
	return w.inner.Header()
}

func (w *GeminiRoutingFooterWriter) WriteHeader(code int) {
	if w.headersEmitted {
		return
	}
	ct := w.inner.Header().Get("Content-Type")
	w.streaming = strings.Contains(ct, "text/event-stream") && code < 400
	w.headersEmitted = true
	w.inner.WriteHeader(code)
}

func (w *GeminiRoutingFooterWriter) Write(data []byte) (int, error) {
	if !w.streaming || w.footer == "" {
		return w.inner.Write(data)
	}
	return w.processUpstream(data)
}

// Flush implements http.Flusher.
func (w *GeminiRoutingFooterWriter) Flush() {
	if w.flusher != nil {
		w.flusher.Flush()
	}
}

// processUpstream forwards Gemini SSE chunks in order, injecting the footer chunk
// immediately before the first chunk whose candidates[0].finishReason is "STOP"
// (when the turn carried no functionCall).
func (w *GeminiRoutingFooterWriter) processUpstream(data []byte) (int, error) {
	w.buf.Write(data)
	for {
		event, n := sse.SplitNext(w.buf.Bytes())
		if n == 0 {
			break
		}
		_, payload := sse.ParseEvent(event)

		if chunkHasFunctionCall(payload) {
			w.sawFunctionCall = true
		}
		if !w.footerEmitted && w.shouldInject(payload) {
			w.emitFooterChunk()
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

// shouldInject reports whether payload terminates a natural, tool-free turn.
func (w *GeminiRoutingFooterWriter) shouldInject(payload []byte) bool {
	if w.sawFunctionCall {
		return false
	}
	return gjson.GetBytes(payload, "candidates.0.finishReason").String() == "STOP"
}

// emitFooterChunk writes a Gemini candidate carrying the footer as a model text
// part (no finishReason — it's a content chunk).
func (w *GeminiRoutingFooterWriter) emitFooterChunk() {
	w.bw.WriteString(`data: {"candidates":[{"content":{"parts":[{"text":`)
	sse.WriteJSONString(w.bw, w.footer)
	w.bw.WriteString(`}],"role":"model"},"index":0}]}`)
	w.bw.WriteString("\n\n")
}

// chunkHasFunctionCall reports whether any part of a Gemini chunk's first
// candidate is a functionCall (a tool-call turn).
func chunkHasFunctionCall(payload []byte) bool {
	found := false
	gjson.GetBytes(payload, "candidates.0.content.parts").ForEach(func(_, part gjson.Result) bool {
		if part.Get("functionCall").Exists() {
			found = true
			return false
		}
		return true
	})
	return found
}

var _ http.ResponseWriter = (*GeminiRoutingFooterWriter)(nil)
var _ http.Flusher = (*GeminiRoutingFooterWriter)(nil)
