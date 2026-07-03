package sse

import (
	"bufio"
	"net/http"
	"strings"
)

// ChunkedWriter is an embeddable base for http.ResponseWriter wrappers that
// prepend synthetic content (e.g. a routing marker) to a live SSE stream
// before upstream bytes arrive. It owns the header/streaming-detection and
// flush bookkeeping shared by every such wrapper; embedders add their own
// format-specific Write and emit* methods.
type ChunkedWriter struct {
	Inner   http.ResponseWriter
	Flusher http.Flusher
	BW      *bufio.Writer

	Streaming      bool
	HeadersEmitted bool
}

// NewChunkedWriter wraps w, buffering emitted bytes through a bufio.Writer of
// bufSize bytes.
func NewChunkedWriter(w http.ResponseWriter, bufSize int) ChunkedWriter {
	flusher, _ := w.(http.Flusher)
	return ChunkedWriter{
		Inner:   w,
		Flusher: flusher,
		BW:      bufio.NewWriterSize(w, bufSize),
	}
}

// Header implements http.ResponseWriter.
func (c *ChunkedWriter) Header() http.Header {
	return c.Inner.Header()
}

// WriteHeader implements http.ResponseWriter. It latches Streaming based on
// whether the upstream response is a non-error SSE stream, and is idempotent
// — a call after the first is a no-op.
func (c *ChunkedWriter) WriteHeader(code int) {
	if c.HeadersEmitted {
		return
	}
	ct := c.Inner.Header().Get("Content-Type")
	c.Streaming = strings.Contains(ct, "text/event-stream") && code < 400
	c.HeadersEmitted = true
	c.Inner.WriteHeader(code)
}

// Flush implements http.Flusher.
func (c *ChunkedWriter) Flush() {
	if c.Flusher != nil {
		c.Flusher.Flush()
	}
}

// FlushEvent flushes the buffered writer, then the underlying http.Flusher —
// the sequence every emit* method needs after writing one SSE event.
func (c *ChunkedWriter) FlushEvent() error {
	return FlushWriter(c.BW, c.Flusher)
}

// FlushWriter flushes bw, then f if non-nil. Shared by writers that keep
// their own bufio.Writer/http.Flusher pair instead of embedding ChunkedWriter
// (e.g. because they also need bespoke WriteHeader/streaming-detection logic
// that ChunkedWriter's can't safely stand in for).
func FlushWriter(bw *bufio.Writer, f http.Flusher) error {
	if err := bw.Flush(); err != nil {
		return err
	}
	if f != nil {
		f.Flush()
	}
	return nil
}
