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

	streaming     bool
	markerEmitted bool
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
	ct := w.inner.Header().Get("Content-Type")
	w.streaming = strings.Contains(ct, "text/event-stream") && code < 400
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
