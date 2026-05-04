package proxy

import (
	"bytes"
	"net/http"
)

// captureWriter wraps an http.ResponseWriter to mirror writes into an
// in-memory buffer so the proxy can store the wire-format response in
// the semantic cache after it streams. Writes pass through to the
// underlying writer unchanged; the buffer is purely a side channel.
//
// Bodies that exceed maxBytes mark the capture as overflowed: the
// pass-through continues so the client still gets its full response,
// but the buffer is dropped and the proxy skips the post-Proxy Store
// call. This bounds peak memory at maxBytes per concurrent in-flight
// non-streaming request.
type captureWriter struct {
	w           http.ResponseWriter
	body        bytes.Buffer
	statusCode  int
	wroteHeader bool
	maxBytes    int
	overflow    bool
}

func newCaptureWriter(w http.ResponseWriter, maxBytes int) *captureWriter {
	return &captureWriter{w: w, statusCode: http.StatusOK, maxBytes: maxBytes}
}

func (c *captureWriter) Header() http.Header { return c.w.Header() }

func (c *captureWriter) WriteHeader(code int) {
	if c.wroteHeader {
		return
	}
	c.statusCode = code
	c.wroteHeader = true
	c.w.WriteHeader(code)
}

func (c *captureWriter) Write(p []byte) (int, error) {
	if !c.overflow {
		if c.body.Len()+len(p) > c.maxBytes {
			c.overflow = true
			c.body.Reset()
		} else {
			c.body.Write(p)
		}
	}
	return c.w.Write(p)
}

// Flush forwards to the underlying writer when it implements
// http.Flusher (gin's default does). Provider clients call Flush()
// during SSE streaming; we honor it transparently. Non-streaming
// paths Flush() once at end-of-body and the captured bytes are still
// complete.
func (c *captureWriter) Flush() {
	if f, ok := c.w.(http.Flusher); ok {
		f.Flush()
	}
}

// captured reports whether the buffer still holds the full body and
// returns it. Callers must check the bool before reading body.
func (c *captureWriter) captured() ([]byte, int, bool) {
	if c.overflow {
		return nil, 0, false
	}
	out := make([]byte, c.body.Len())
	copy(out, c.body.Bytes())
	return out, c.statusCode, true
}
