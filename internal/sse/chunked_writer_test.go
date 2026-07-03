package sse_test

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"testing"

	"workweave/router/internal/sse"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flushRecorder wraps httptest.ResponseRecorder to also implement http.Flusher
// and count Flush calls, so tests can assert FlushEvent/Flush actually reach
// the underlying flusher.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (f *flushRecorder) Flush() { f.flushes++ }

func TestChunkedWriter_WriteHeaderDetectsStreaming(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/event-stream")
	c := sse.NewChunkedWriter(rec, 4096)

	c.WriteHeader(http.StatusOK)

	assert.True(t, c.Streaming)
	assert.True(t, c.HeadersEmitted)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestChunkedWriter_WriteHeaderNonSSENotStreaming(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/json")
	c := sse.NewChunkedWriter(rec, 4096)

	c.WriteHeader(http.StatusOK)

	assert.False(t, c.Streaming)
}

func TestChunkedWriter_WriteHeaderErrorStatusNotStreaming(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/event-stream")
	c := sse.NewChunkedWriter(rec, 4096)

	c.WriteHeader(http.StatusInternalServerError)

	assert.False(t, c.Streaming, "a >=400 status must not be treated as a streaming SSE response")
}

func TestChunkedWriter_WriteHeaderIdempotent(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/event-stream")
	c := sse.NewChunkedWriter(rec, 4096)

	c.WriteHeader(http.StatusOK)
	c.WriteHeader(http.StatusInternalServerError)

	assert.Equal(t, http.StatusOK, rec.Code, "second WriteHeader call must be a no-op")
	assert.True(t, c.Streaming, "state from the first call must not be overwritten by the no-op second call")
}

func TestChunkedWriter_HeaderReturnsInnerHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	c := sse.NewChunkedWriter(rec, 4096)

	c.Header().Set("X-Test", "1")

	assert.Equal(t, "1", rec.Header().Get("X-Test"))
}

func TestChunkedWriter_FlushForwardsToUnderlyingFlusher(t *testing.T) {
	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	c := sse.NewChunkedWriter(rec, 4096)

	c.Flush()

	assert.Equal(t, 1, rec.flushes)
}

func TestChunkedWriter_FlushNilFlusherNoPanic(t *testing.T) {
	// httptest.ResponseRecorder implements http.Flusher, so wrap a writer that
	// deliberately doesn't, to exercise the nil-flusher branch.
	w := struct{ http.ResponseWriter }{httptest.NewRecorder()}
	c := sse.NewChunkedWriter(w, 4096)

	require.Nil(t, c.Flusher)
	assert.NotPanics(t, func() { c.Flush() })
}

func TestChunkedWriter_FlushEventFlushesBufferThenFlusher(t *testing.T) {
	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	c := sse.NewChunkedWriter(rec, 4096)

	c.BW.WriteString("hello")
	err := c.FlushEvent()

	require.NoError(t, err)
	assert.Equal(t, "hello", rec.Body.String(), "FlushEvent must flush the buffered bytes to the inner writer")
	assert.Equal(t, 1, rec.flushes, "FlushEvent must also flush the underlying http.Flusher")
}

func TestFlushWriter_FlushesBufferThenFlusher(t *testing.T) {
	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	bw := bufio.NewWriterSize(rec, 4096)
	bw.WriteString("world")

	err := sse.FlushWriter(bw, rec)

	require.NoError(t, err)
	assert.Equal(t, "world", rec.Body.String())
	assert.Equal(t, 1, rec.flushes)
}

func TestFlushWriter_NilFlusherNoPanic(t *testing.T) {
	rec := httptest.NewRecorder()
	bw := bufio.NewWriterSize(rec, 4096)
	bw.WriteString("ok")

	err := sse.FlushWriter(bw, nil)

	require.NoError(t, err)
	assert.Equal(t, "ok", rec.Body.String())
}
