package translate_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/translate"
)

// teeWriter mirrors every byte written to it into capture, simulating the
// content-capture writer the proxy splices in via WrapInner.
type teeWriter struct {
	inner   http.ResponseWriter
	capture *bytes.Buffer
}

func (t *teeWriter) Header() http.Header { return t.inner.Header() }
func (t *teeWriter) WriteHeader(c int)   { t.inner.WriteHeader(c) }
func (t *teeWriter) Write(p []byte) (int, error) {
	t.capture.Write(p)
	return t.inner.Write(p)
}
func (t *teeWriter) Flush() {
	if f, ok := t.inner.(http.Flusher); ok {
		f.Flush()
	}
}

// TestResponsesWriter_WrapInnerCapturesPrelude asserts WrapInner reroutes both
// the buffered writer and the raw inner so the eager response.created prelude —
// which previously bypassed an externally-wrapped capture writer — reaches the
// spliced-in writer, and that what it captures equals what the client receives.
func TestResponsesWriter_WrapInnerCapturesPrelude(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := translate.NewResponsesWriter(rec, "claude-opus-4-8")

	var captured bytes.Buffer
	rw.WrapInner(func(inner http.ResponseWriter) http.ResponseWriter {
		return &teeWriter{inner: inner, capture: &captured}
	})

	require.NoError(t, rw.Prelude(true))
	rw.Flush()

	assert.NotEmpty(t, captured.Bytes(), "prelude bytes must reach the wrapped writer")
	assert.Contains(t, captured.String(), "response.created")
	// Capture is byte-identical to what the client actually received.
	assert.Equal(t, rec.Body.String(), captured.String())
}
