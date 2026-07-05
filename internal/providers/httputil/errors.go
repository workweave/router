package httputil

import (
	"io"
	"net/http"

	"workweave/router/internal/observability"
	"workweave/router/internal/providers"
)

// ReadCapped buffers up to limit bytes from r, then drains (without retaining)
// up to maxDrain more to bound failover latency on a large error body. Returns
// the buffered prefix, total bytes read, and any read error (io.EOF -> nil).
func ReadCapped(r io.Reader, limit int) ([]byte, int64, error) {
	prefix, err := io.ReadAll(io.LimitReader(r, int64(limit)))
	totalRead := int64(len(prefix))
	if err != nil {
		return prefix, totalRead, err
	}
	const maxDrain = 1 << 20 // 1 MiB
	rest, drainErr := io.Copy(io.Discard, io.LimitReader(r, maxDrain))
	totalRead += rest
	return prefix, totalRead, drainErr
}

// PreviewBytes returns the first 1KB of body as a string for logging.
func PreviewBytes(body []byte) string {
	const previewLimit = 1024
	if len(body) > previewLimit {
		return string(body[:previewLimit])
	}
	return string(body)
}

// HeaderCapture is a minimal http.ResponseWriter that captures headers only,
// used to reuse providers.CopyUpstreamHeaders against an http.Header we own.
// Write/WriteHeader are no-ops.
type HeaderCapture struct{ H http.Header }

// Header returns the captured header set.
func (c HeaderCapture) Header() http.Header { return c.H }

// Write is a no-op; HeaderCapture only captures headers.
func (c HeaderCapture) Write([]byte) (int, error) { return 0, nil }

// WriteHeader is a no-op; HeaderCapture only captures headers.
func (c HeaderCapture) WriteHeader(int) {}

// LogUpstreamStatus logs non-2xx upstream responses with a body preview, at
// ERROR except 429 (routine rate-limit signal handled via failover), which
// logs at WARN.
func LogUpstreamStatus(msg string, status int, attrs ...any) {
	merged := append([]any{"status", status}, attrs...)
	if status >= 500 || (status >= 400 && status != http.StatusTooManyRequests) {
		observability.Get().Error(msg, merged...)
		return
	}
	observability.Get().Warn(msg, merged...)
}

// WritePassthroughError streams up to 1KB of resp.Body to w, logs via
// LogUpstreamStatus (body_preview/body_total_bytes appended automatically),
// and returns UpstreamStatusError. onFirstByte/onEOF are nil-safe OTel
// timing hooks; pass nil, nil on paths that don't stamp upstream timing.
func WritePassthroughError(w http.ResponseWriter, resp *http.Response, onFirstByte, onEOF func(), msg string, attrs ...any) error {
	var snip [1024]byte
	n, _ := io.ReadFull(resp.Body, snip[:])
	if n > 0 && onFirstByte != nil {
		onFirstByte()
	}
	_, snipWriteErr := w.Write(snip[:n])
	rest, copyErr := io.Copy(w, resp.Body)
	if copyErr == nil && onEOF != nil {
		onEOF()
	}
	merged := append(append([]any{}, attrs...), "body_preview", string(snip[:n]), "body_total_bytes", int64(n)+rest)
	LogUpstreamStatus(msg, resp.StatusCode, merged...)
	if snipWriteErr != nil {
		return snipWriteErr
	}
	if copyErr != nil {
		return copyErr
	}
	return &providers.UpstreamStatusError{Status: resp.StatusCode}
}
