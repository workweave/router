// Package anthropic is the providers.Client adapter for Anthropic's Messages API.
package anthropic

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"workweave/router/internal/observability"
	"workweave/router/internal/observability/otel"
	"workweave/router/internal/providers"
	"workweave/router/internal/providers/httputil"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
)

const DefaultBaseURL = "https://api.anthropic.com"

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
		http:    &http.Client{Transport: httputil.NewTransport(10*time.Second, 10*time.Second)},
	}
}

// setAuth applies authentication to the upstream request. Precedence:
// (1) per-request BYOK credentials in ctx; (2) deployment-level API key;
// (3) passthrough of the client's own Anthropic auth headers (OAuth bearer or
// x-api-key). Router-only credentials are deliberately not forwarded.
func (c *Client) setAuth(ctx context.Context, upstream *http.Request, inbound *http.Request) {
	if creds := proxy.CredentialsFromContext(ctx); creds != nil {
		upstream.Header.Set("x-api-key", string(creds.APIKey))
		return
	}
	if c.apiKey != "" {
		upstream.Header.Set("x-api-key", c.apiKey)
		return
	}
	if v := inbound.Header.Get("authorization"); v != "" {
		upstream.Header.Set("authorization", v)
	}
	if v := inbound.Header.Get("x-api-key"); v != "" {
		upstream.Header.Set("x-api-key", v)
	}
}

func (c *Client) Proxy(ctx context.Context, decision router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(prep.Body))
	if err != nil {
		return fmt.Errorf("build upstream request: %w", err)
	}
	upstream.Header.Set("content-type", "application/json")
	c.setAuth(ctx, upstream, r)
	for k, vs := range prep.Headers {
		upstream.Header[http.CanonicalHeaderKey(k)] = vs
	}
	if v := r.Header.Get("accept"); v != "" {
		upstream.Header.Set("accept", v)
	}

	t := otel.TimingFrom(ctx)
	t.StampUpstreamRequest()
	resp, err := c.http.Do(upstream)
	if err != nil {
		return fmt.Errorf("upstream call: %w", err)
	}
	defer resp.Body.Close()
	t.StampUpstreamHeaders()

	providers.CopyUpstreamHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)

	if resp.StatusCode >= 400 {
		var snip [1024]byte
		n, _ := io.ReadFull(resp.Body, snip[:])
		if n > 0 {
			t.StampUpstreamFirstByte()
		}
		_, snipWriteErr := w.Write(snip[:n])
		rest, copyErr := io.Copy(w, resp.Body)
		if copyErr == nil {
			t.StampUpstreamEOF()
		}
		logUpstreamStatus(
			"Upstream Anthropic returned error status",
			resp.StatusCode,
			"routed_model", decision.Model,
			"body_preview", string(snip[:n]),
			"body_total_bytes", int64(n)+rest,
		)
		if snipWriteErr != nil {
			return snipWriteErr
		}
		if copyErr != nil {
			return copyErr
		}
		return &providers.UpstreamStatusError{Status: resp.StatusCode}
	}

	return httputil.StreamBody(resp.Body, resp.StatusCode, w, t)
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
	if ct := r.Header.Get("content-type"); ct != "" {
		upstream.Header.Set("content-type", ct)
	}
	c.setAuth(ctx, upstream, r)
	for k, vs := range prep.Headers {
		upstream.Header[http.CanonicalHeaderKey(k)] = vs
	}
	if v := r.Header.Get("accept"); v != "" {
		upstream.Header.Set("accept", v)
	}

	resp, err := c.http.Do(upstream)
	if err != nil {
		return fmt.Errorf("upstream passthrough call: %w", err)
	}
	defer resp.Body.Close()

	providers.CopyUpstreamHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	if resp.StatusCode >= 400 {
		var snip [1024]byte
		n, _ := io.ReadFull(resp.Body, snip[:])
		_, snipWriteErr := w.Write(snip[:n])
		rest, copyErr := io.Copy(w, resp.Body)
		logUpstreamStatus(
			"Upstream Anthropic returned error status (passthrough)",
			resp.StatusCode,
			"path", r.URL.Path,
			"body_preview", string(snip[:n]),
			"body_total_bytes", int64(n)+rest,
		)
		if snipWriteErr != nil {
			return snipWriteErr
		}
		if copyErr != nil {
			return copyErr
		}
		return &providers.UpstreamStatusError{Status: resp.StatusCode}
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

func logUpstreamStatus(msg string, status int, attrs ...any) {
	merged := append([]any{"status", status}, attrs...)
	if status >= 500 || (status >= 400 && status != http.StatusTooManyRequests) {
		observability.Get().Error(msg, merged...)
		return
	}
	observability.Get().Warn(msg, merged...)
}

var _ providers.Client = (*Client)(nil)
