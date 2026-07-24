package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// proxy is the forward-proxy handler. It handles only CONNECT (the shape
// Go's HTTP transport uses for HTTPS through a proxy), one request per tunnel.
type proxy struct {
	cfg   config
	ca    *ca
	store *store
	live  *http.Client
}

func (p *proxy) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "mitmproxy: only CONNECT is supported", http.StatusMethodNotAllowed)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "mitmproxy: hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		log.Printf("mitmproxy: hijack failed: %v", err)
		return
	}
	defer clientConn.Close()

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		log.Printf("mitmproxy: write CONNECT ack: %v", err)
		return
	}

	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host // r.Host had no port (defaults to 443 for CONNECT targets)
	}

	tlsConn := tls.Server(clientConn, p.ca.tlsConfigFor())
	defer tlsConn.Close()
	if err := tlsConn.Handshake(); err != nil {
		log.Printf("mitmproxy: TLS handshake with client for %s: %v", host, err)
		return
	}

	// One request per CONNECT tunnel — the router doesn't pipeline, and
	// sequential smoke calls keep the implementation simple and correct.
	if err := p.serveOne(tlsConn, host); err != nil && !errors.Is(err, io.EOF) {
		log.Printf("mitmproxy: serve %s: %v", host, err)
	}
}

func (p *proxy) serveOne(conn net.Conn, host string) error {
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return fmt.Errorf("read tunneled request: %w", err)
	}
	defer req.Body.Close()

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return fmt.Errorf("read request body: %w", err)
	}

	key := requestKey(req.Method, req.URL.Path, body)
	log.Printf("mitmproxy: %s %s%s key=%s mode=%s", req.Method, host, req.URL.Path, key[:12], p.cfg.mode)

	c, err := p.resolve(host, req, body, key)
	if err != nil {
		// Synthesize a clean 502 so a cache-miss appears as an assertion failure
		// in the smoke suite rather than a confusing transport error.
		writeErr := writeCassetteResponse(conn, &cassette{
			StatusCode: http.StatusBadGateway,
			Headers:    map[string]string{"Content-Type": "text/plain"},
			Body:       []byte(err.Error()),
		})
		if writeErr != nil {
			return fmt.Errorf("resolve response: %w (and failed to write error response: %v)", err, writeErr)
		}
		return fmt.Errorf("resolve response: %w", err)
	}

	return writeCassetteResponse(conn, c)
}

// resolve returns the cassette to serve, per the configured mode.
func (p *proxy) resolve(host string, req *http.Request, body []byte, key string) (*cassette, error) {
	switch p.cfg.mode {
	case modeReplayOnly:
		c, ok := p.store.load(key)
		if !ok {
			return nil, fmt.Errorf("%w: %s %s (key=%s) — record a cassette first (SMOKE_PROXY_MODE=record or replay-or-record)",
				errCacheMiss, req.Method, req.URL.Path, key)
		}
		return c, nil

	case modeRecord:
		return p.recordLive(host, req, body, key)

	default: // modeReplayOrRecord
		if c, ok := p.store.load(key); ok {
			return c, nil
		}
		return p.recordLive(host, req, body, key)
	}
}

// recordLive dispatches the request to the real upstream and persists the
// (sanitized) response as a cassette before returning it.
func (p *proxy) recordLive(host string, req *http.Request, body []byte, key string) (*cassette, error) {
	targetURL := &url.URL{
		Scheme:   "https",
		Host:     host,
		Path:     req.URL.Path,
		RawQuery: req.URL.RawQuery,
	}
	target := &http.Request{
		Method: req.Method,
		URL:    targetURL,
		Header: req.Header.Clone(),
		Body:   io.NopCloser(bytes.NewReader(body)),
	}
	target.ContentLength = int64(len(body))

	resp, err := p.live.Do(target)
	if err != nil {
		return nil, fmt.Errorf("live upstream call: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read live response body: %w", err)
	}

	// Decompress before persisting: the cloned request carries Accept-Encoding verbatim, so
	// Go's transparent gzip decompression never fires and resp.Body is raw gzip. Storing
	// compressed bytes makes cassettes binary blobs, undiffable and opaque to secret-scanning.
	headers := sanitizeHeaders(resp.Header)
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gr, gzErr := gzip.NewReader(bytes.NewReader(respBody))
		if gzErr != nil {
			return nil, fmt.Errorf("open gzip response body: %w", gzErr)
		}
		decompressed, readErr := io.ReadAll(gr)
		gr.Close()
		if readErr != nil {
			return nil, fmt.Errorf("decompress gzip response body: %w", readErr)
		}
		respBody = decompressed
		delete(headers, "Content-Encoding")
	}

	c := &cassette{
		Method:     req.Method,
		Path:       req.URL.Path,
		StatusCode: resp.StatusCode,
		Headers:    headers,
		Body:       respBody,
	}
	if err := p.store.save(key, c); err != nil {
		log.Printf("mitmproxy: WARNING failed to persist cassette %s: %v", key, err)
	} else {
		log.Printf("mitmproxy: recorded cassette key=%s status=%d bytes=%d", key[:12], resp.StatusCode, len(respBody))
	}
	return c, nil
}

// writeCassetteResponse writes a stored cassette back to the client connection
// as a raw HTTP/1.1 response.
func writeCassetteResponse(conn net.Conn, c *cassette) error {
	resp := &http.Response{
		StatusCode: c.StatusCode,
		Status:     http.StatusText(c.StatusCode),
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header, len(c.Headers)),
		Body:       io.NopCloser(bytes.NewReader(c.Body)),
		// ContentLength:-1 lets resp.Write use chunked encoding — fine for both
		// JSON and SSE since the router's SSE reader doesn't require a specific transfer-encoding.
		ContentLength: -1,
	}
	for k, v := range c.Headers {
		resp.Header.Set(k, v)
	}
	return resp.Write(conn)
}
