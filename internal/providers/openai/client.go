// Package openai is the providers.Client adapter for OpenAI's Chat Completions API.
package openai

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"
	"workweave/router/internal/observability/otel"
	"workweave/router/internal/providers"
	"workweave/router/internal/providers/httputil"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
)

const DefaultBaseURL = "https://api.openai.com"

type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

func NewClient(apiKey, baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		apiKey:  apiKey,
		baseURL: baseURL,
		http:    &http.Client{Transport: httputil.NewTransport(5*time.Second, 5*time.Second)},
	}
}

// setAuth applies authentication to the upstream request. Precedence:
// (1) per-request BYOK credentials in ctx; (2) deployment-level API key;
// (3) passthrough of the client's own OpenAI auth header (Codex plan flow).
//
// The passthrough tier strips `Authorization: Bearer rk_...` because the
// router auth middleware accepts the same header for router-key auth — we
// must not relay a router credential to OpenAI just because no BYOK or
// deployment key is configured. Mirrors the !HasAPIKeyPrefix guard in
// proxy.ExtractClientCredentials.
func (c *Client) setAuth(ctx context.Context, upstream *http.Request, inbound *http.Request) {
	if creds := proxy.CredentialsFromContext(ctx); creds != nil {
		upstream.Header.Set("Authorization", "Bearer "+string(creds.APIKey))
		return
	}
	if c.apiKey != "" {
		upstream.Header.Set("Authorization", "Bearer "+c.apiKey)
		return
	}
	v := inbound.Header.Get("authorization")
	if v == "" {
		return
	}
	// Only forward if the Bearer token isn't a router-issued key. Any other
	// shape (incl. raw or malformed) we still forward — upstream will 401 on
	// invalid creds, which is the correct failure mode for "no auth resolvable".
	if raw, found := strings.CutPrefix(v, "Bearer "); found {
		if auth.HasAPIKeyPrefix(strings.TrimSpace(raw)) {
			return
		}
	}
	upstream.Header.Set("Authorization", v)
}

func (c *Client) Proxy(ctx context.Context, decision router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(prep.Body))
	if err != nil {
		return fmt.Errorf("build upstream request: %w", err)
	}
	upstream.Header.Set("Content-Type", "application/json")
	c.setAuth(ctx, upstream, r)
	for k, vs := range prep.Headers {
		upstream.Header[http.CanonicalHeaderKey(k)] = vs
	}
	if v := r.Header.Get("Accept"); v != "" {
		upstream.Header.Set("Accept", v)
	}

	t := otel.TimingFrom(ctx)
	t.StampUpstreamRequest()
	resp, err := c.http.Do(upstream)
	if err != nil {
		return fmt.Errorf("upstream call: %w", err)
	}
	defer resp.Body.Close()
	t.StampUpstreamHeaders()

	log := observability.Get()
	log.Debug("OpenAI upstream response",
		"status", resp.StatusCode,
		"content_type", resp.Header.Get("Content-Type"),
		"transfer_encoding", resp.Header.Get("Transfer-Encoding"),
		"content_encoding", resp.Header.Get("Content-Encoding"),
		"request_id", resp.Header.Get("X-Request-Id"),
	)

	providers.CopyUpstreamHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	status := resp.StatusCode

	// Manual stream loop for per-chunk diagnostics; non-debug takes the fast path.
	if !log.Enabled(ctx, slog.LevelDebug) {
		return httputil.StreamBody(resp.Body, status, w, t)
	}

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, httputil.FlushChunk)
	bytesRead := 0
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			t.StampUpstreamFirstByte()
			if bytesRead == 0 {
				log.Debug("OpenAI upstream first chunk",
					"bytes", n,
					"preview", truncateBytes(buf[:n], 320),
				)
			}
			bytesRead += n
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				log.Debug("OpenAI upstream write failed", "err", writeErr, "bytes_read", bytesRead)
				return writeErr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr == io.EOF {
			t.StampUpstreamEOF()
			log.Debug("OpenAI upstream stream complete", "bytes_total", bytesRead)
			if status < 200 || status >= 300 {
				return &providers.UpstreamStatusError{Status: status}
			}
			return nil
		}
		if readErr != nil {
			log.Debug("OpenAI upstream read failed", "err", readErr, "bytes_read", bytesRead)
			return readErr
		}
	}
}

func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

func (c *Client) Passthrough(ctx context.Context, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	url := c.baseURL + r.URL.Path
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}

	upstream, err := http.NewRequestWithContext(ctx, r.Method, url, bytes.NewReader(prep.Body))
	if err != nil {
		return fmt.Errorf("build upstream passthrough request: %w", err)
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		upstream.Header.Set("Content-Type", ct)
	}
	c.setAuth(ctx, upstream, r)
	for k, vs := range prep.Headers {
		upstream.Header[http.CanonicalHeaderKey(k)] = vs
	}
	if v := r.Header.Get("Accept"); v != "" {
		upstream.Header.Set("Accept", v)
	}

	resp, err := c.http.Do(upstream)
	if err != nil {
		return fmt.Errorf("upstream passthrough call: %w", err)
	}
	defer resp.Body.Close()

	providers.CopyUpstreamHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, resp.Body)
	return err
}

var _ providers.Client = (*Client)(nil)
