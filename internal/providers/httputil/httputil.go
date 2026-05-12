// Package httputil provides shared HTTP transport and streaming helpers for provider adapters.
package httputil

import (
	"io"
	"net"
	"net/http"
	"time"

	"workweave/router/internal/observability/otel"
	"workweave/router/internal/providers"
)

// FlushChunk is the read buffer size used by all streaming provider adapters.
const FlushChunk = 4 * 1024

// NewTransport returns a pooled http.Transport sized for sustained traffic to a single upstream host.
func NewTransport(dialTimeout, tlsTimeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConnsPerHost:   64,
		MaxIdleConns:          256,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   tlsTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ForceAttemptHTTP2:     true,
	}
}

// StreamBody reads r chunk-by-chunk into w, flushing after each write. Returns
// UpstreamStatusError when status is non-2xx.
func StreamBody(r io.Reader, status int, w http.ResponseWriter, t *otel.Timing) error {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, FlushChunk)
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			t.StampUpstreamFirstByte()
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr == io.EOF {
			t.StampUpstreamEOF()
			if status < 200 || status >= 300 {
				return &providers.UpstreamStatusError{Status: status}
			}
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
