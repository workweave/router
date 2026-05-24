package translate

import (
	"bufio"
	"net/http"
	"strings"

	"workweave/router/internal/sse"
)

// GeminiRoutingMarkerWriter wraps an http.ResponseWriter and injects a
// routing-marker chunk at the start of a Gemini-format SSE stream.
// Non-streaming responses pass through unmodified.
type GeminiRoutingMarkerWriter struct {
	inner   http.ResponseWriter
	flusher http.Flusher
	bw      *bufio.Writer

	marker string

	streaming      bool
	headersEmitted bool
	markerEmitted  bool
}

// NewGeminiRoutingMarkerWriter creates a writer that emits marker as the first
// Gemini SSE candidate before any upstream data. If marker is empty, all
// writes pass through unchanged.
func NewGeminiRoutingMarkerWriter(w http.ResponseWriter, marker string) *GeminiRoutingMarkerWriter {
	flusher, _ := w.(http.Flusher)
	return &GeminiRoutingMarkerWriter{
		inner:   w,
		flusher: flusher,
		bw:      bufio.NewWriterSize(w, 4096),
		marker:  marker,
	}
}

func (w *GeminiRoutingMarkerWriter) Header() http.Header {
	return w.inner.Header()
}

func (w *GeminiRoutingMarkerWriter) WriteHeader(code int) {
	if w.headersEmitted {
		return
	}
	ct := w.inner.Header().Get("Content-Type")
	w.streaming = strings.Contains(ct, "text/event-stream") && code < 400
	w.headersEmitted = true
	w.inner.WriteHeader(code)
}

func (w *GeminiRoutingMarkerWriter) Write(data []byte) (int, error) {
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
// upstream provider has returned a single byte. See OpenAIRoutingMarkerWriter.Prelude.
func (w *GeminiRoutingMarkerWriter) Prelude(streaming bool) error {
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
func (w *GeminiRoutingMarkerWriter) Flush() {
	if w.flusher != nil {
		w.flusher.Flush()
	}
}

func (w *GeminiRoutingMarkerWriter) emitMarkerChunk() error {
	w.bw.WriteString(`data: {"candidates":[{"content":{"parts":[{"text":`)
	sse.WriteJSONString(w.bw, w.marker)
	w.bw.WriteString(`}],"role":"model"},"index":0}]}`)
	w.bw.WriteString("\n\n")
	if err := w.bw.Flush(); err != nil {
		return err
	}
	if w.flusher != nil {
		w.flusher.Flush()
	}
	return nil
}

var _ http.ResponseWriter = (*GeminiRoutingMarkerWriter)(nil)
var _ http.Flusher = (*GeminiRoutingMarkerWriter)(nil)
