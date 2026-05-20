package translate

import (
	"bufio"
	"net/http"
	"strings"
	"time"

	"workweave/router/internal/sse"
)

// OpenAIRoutingMarkerWriter wraps an http.ResponseWriter and injects a
// routing-marker chunk at the start of an OpenAI-format SSE stream.
// Non-streaming responses pass through unmodified.
type OpenAIRoutingMarkerWriter struct {
	inner   http.ResponseWriter
	flusher http.Flusher
	bw      *bufio.Writer

	marker string
	model  string

	streaming      bool
	headersEmitted bool
	markerEmitted  bool
}

// NewOpenAIRoutingMarkerWriter creates a writer that emits marker as the first
// chat.completion.chunk before any upstream data. If marker is empty, all
// writes pass through unchanged.
func NewOpenAIRoutingMarkerWriter(w http.ResponseWriter, model, marker string) *OpenAIRoutingMarkerWriter {
	flusher, _ := w.(http.Flusher)
	return &OpenAIRoutingMarkerWriter{
		inner:   w,
		flusher: flusher,
		bw:      bufio.NewWriterSize(w, 4096),
		marker:  marker,
		model:   model,
	}
}

func (w *OpenAIRoutingMarkerWriter) Header() http.Header {
	return w.inner.Header()
}

func (w *OpenAIRoutingMarkerWriter) WriteHeader(code int) {
	if w.headersEmitted {
		return
	}
	ct := w.inner.Header().Get("Content-Type")
	w.streaming = strings.Contains(ct, "text/event-stream") && code < 400
	w.headersEmitted = true
	w.inner.WriteHeader(code)
}

func (w *OpenAIRoutingMarkerWriter) Write(data []byte) (int, error) {
	if w.streaming && !w.markerEmitted {
		w.markerEmitted = true
		if w.marker != "" {
			if err := w.emitMarkerChunk(); err != nil {
				return 0, err
			}
		}
	}
	return w.inner.Write(data)
}

// Prelude commits headers and emits the routing marker immediately, before the
// upstream provider has returned a single byte. Call this right after the
// routing decision is made when the client requested streaming (streaming=true)
// so first-byte latency drops to ~routing time rather than upstream prefill +
// first decode. Safe to call once; subsequent Write/WriteHeader calls are
// idempotent. No-op when streaming is false or marker is empty.
func (w *OpenAIRoutingMarkerWriter) Prelude(streaming bool) error {
	if !streaming || w.markerEmitted {
		return nil
	}
	w.inner.Header().Set("Content-Type", "text/event-stream")
	w.streaming = true
	if !w.headersEmitted {
		w.headersEmitted = true
		w.inner.WriteHeader(http.StatusOK)
	}
	w.markerEmitted = true
	if w.marker == "" {
		// Still flush a comment so TCP gets a packet — TTFB is what we're optimizing for.
		w.bw.WriteString(": routing complete\n\n")
		if err := w.bw.Flush(); err != nil {
			return err
		}
		if w.flusher != nil {
			w.flusher.Flush()
		}
		return nil
	}
	return w.emitMarkerChunk()
}

// Flush implements http.Flusher.
func (w *OpenAIRoutingMarkerWriter) Flush() {
	if w.flusher != nil {
		w.flusher.Flush()
	}
}

func (w *OpenAIRoutingMarkerWriter) emitMarkerChunk() error {
	w.bw.WriteString(`data: {"id":`)
	sse.WriteJSONString(w.bw, generateChatCmplID())
	w.bw.WriteString(`,"object":"chat.completion.chunk","created":`)
	sse.WriteJSONInt(w.bw, time.Now().Unix())
	w.bw.WriteString(`,"model":`)
	sse.WriteJSONString(w.bw, w.model)
	w.bw.WriteString(`,"choices":[{"index":0,"delta":{"role":"assistant","content":`)
	sse.WriteJSONString(w.bw, w.marker)
	w.bw.WriteString(`},"finish_reason":null}]}`)
	w.bw.WriteString("\n\n")
	if err := w.bw.Flush(); err != nil {
		return err
	}
	if w.flusher != nil {
		w.flusher.Flush()
	}
	return nil
}

var _ http.ResponseWriter = (*OpenAIRoutingMarkerWriter)(nil)
var _ http.Flusher = (*OpenAIRoutingMarkerWriter)(nil)
