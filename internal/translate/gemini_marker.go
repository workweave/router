package translate

import (
	"net/http"

	"workweave/router/internal/sse"
)

// GeminiRoutingMarkerWriter wraps an http.ResponseWriter and injects a
// routing-marker chunk at the start of a Gemini-format SSE stream.
// Non-streaming responses pass through unmodified.
type GeminiRoutingMarkerWriter struct {
	sse.ChunkedWriter

	marker string

	markerEmitted bool
}

// NewGeminiRoutingMarkerWriter creates a writer that emits marker as the first
// Gemini SSE candidate before any upstream data. If marker is empty, all
// writes pass through unchanged.
func NewGeminiRoutingMarkerWriter(w http.ResponseWriter, marker string) *GeminiRoutingMarkerWriter {
	return &GeminiRoutingMarkerWriter{
		ChunkedWriter: sse.NewChunkedWriter(w, 4096),
		marker:        marker,
	}
}

func (w *GeminiRoutingMarkerWriter) Write(data []byte) (int, error) {
	if w.Streaming && !w.markerEmitted {
		w.markerEmitted = true
		if w.marker != "" {
			if err := w.emitMarkerChunk(); err != nil {
				return 0, err
			}
		}
	}
	return w.Inner.Write(data)
}

// Prelude commits headers and emits the routing marker immediately, before the
// upstream provider has returned a single byte. See OpenAIRoutingMarkerWriter.Prelude.
func (w *GeminiRoutingMarkerWriter) Prelude(streaming bool) error {
	if !streaming || w.markerEmitted {
		return nil
	}
	w.Inner.Header().Set("Content-Type", "text/event-stream")
	w.Streaming = true
	if !w.HeadersEmitted {
		w.HeadersEmitted = true
		w.Inner.WriteHeader(http.StatusOK)
	}
	w.markerEmitted = true
	if w.marker == "" {
		w.BW.WriteString(": routing complete\n\n")
		return w.FlushEvent()
	}
	return w.emitMarkerChunk()
}

func (w *GeminiRoutingMarkerWriter) emitMarkerChunk() error {
	w.BW.WriteString(`data: {"candidates":[{"content":{"parts":[{"text":`)
	sse.WriteJSONString(w.BW, w.marker)
	w.BW.WriteString(`}],"role":"model"},"index":0}]}`)
	w.BW.WriteString("\n\n")
	return w.FlushEvent()
}

var _ http.ResponseWriter = (*GeminiRoutingMarkerWriter)(nil)
var _ http.Flusher = (*GeminiRoutingMarkerWriter)(nil)
