package httputil

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workweave/router/internal/providers"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadCapped_TruncatesAndDrainsRest(t *testing.T) {
	body := strings.Repeat("a", 10) + strings.Repeat("b", 10)
	prefix, total, err := ReadCapped(strings.NewReader(body), 10)
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("a", 10), string(prefix))
	assert.EqualValues(t, 20, total)
}

func TestPreviewBytes_CapsAt1KB(t *testing.T) {
	body := []byte(strings.Repeat("x", 2000))
	preview := PreviewBytes(body)
	assert.Len(t, preview, 1024)

	small := []byte("short body")
	assert.Equal(t, "short body", PreviewBytes(small))
}

func TestHeaderCapture_CapturesHeaderWithoutWriting(t *testing.T) {
	h := http.Header{}
	hc := HeaderCapture{H: h}
	hc.Header().Set("X-Test", "value")
	n, err := hc.Write([]byte("ignored"))
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.Equal(t, "value", h.Get("X-Test"))
}

func TestWritePassthroughError_WritesBodyLogsAndReturnsStatusError(t *testing.T) {
	upstreamBody := "upstream failure detail"
	resp := &http.Response{
		StatusCode: http.StatusBadGateway,
		Body:       http.NoBody,
	}
	resp.Body = io.NopCloser(strings.NewReader(upstreamBody))

	rec := httptest.NewRecorder()
	var firstByteCalls, eofCalls int
	err := WritePassthroughError(rec, resp, func() { firstByteCalls++ }, func() { eofCalls++ }, "upstream failed", "path", "/v1/messages")

	var statusErr *providers.UpstreamStatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, http.StatusBadGateway, statusErr.Status)
	assert.Equal(t, upstreamBody, rec.Body.String())
	assert.Equal(t, 1, firstByteCalls)
	assert.Equal(t, 1, eofCalls)
}

func TestWritePassthroughError_NilHooksAreSafe(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader("boom")),
	}
	rec := httptest.NewRecorder()
	err := WritePassthroughError(rec, resp, nil, nil, "upstream failed")
	require.Error(t, err)
	assert.Equal(t, "boom", rec.Body.String())
}
