// Package openai is the providers.Client adapter for OpenAI's Chat Completions
// API. Proxy rewrites the request body's `model` field to the routed model and
// streams the response back without protocol translation, since both the
// inbound and outbound surfaces are OpenAI Chat Completions.
package openai

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"workweave/router/internal/observability"
	"workweave/router/internal/observability/otel"
	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
)

const DefaultBaseURL = "https://api.openai.com"

const flushChunk = 4 * 1024

// upstreamTraceEnabled mirrors ROUTER_DEBUG_UPSTREAM_TRACE=true. When on, the
// OpenAI/Google adapter dumps response status, headers, and a body preview
// per request — pair with ROUTER_DEBUG_SSE_TRACE to see exactly what's
// crossing the OpenAI ↔ Anthropic translation boundary.
var upstreamTraceEnabled = os.Getenv("ROUTER_DEBUG_UPSTREAM_TRACE") == "true"

type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

// NewClient is pooled for sustained traffic to a single host (api.openai.com).
func NewClient(apiKey, baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConnsPerHost:   64,
		MaxIdleConns:          256,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &Client{
		apiKey:  apiKey,
		baseURL: baseURL,
		http:    &http.Client{Transport: transport},
	}
}

// Complete is intentionally unimplemented — all chat completions traffic flows
// through Proxy. Satisfies providers.Client.
func (c *Client) Complete(_ context.Context, _ providers.Request) (providers.Response, error) {
	return providers.Response{}, providers.ErrNotImplemented
}

func (c *Client) Proxy(ctx context.Context, decision router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(prep.Body))
	if err != nil {
		return fmt.Errorf("build upstream request: %w", err)
	}
	upstream.Header.Set("Content-Type", "application/json")
	// Use per-request BYOK credentials when available; fall back to the
	// deployment-level API key (plan-based auth).
	if creds := proxy.CredentialsFromContext(ctx); creds != nil {
		upstream.Header.Set("Authorization", "Bearer "+string(creds.APIKey))
	} else if c.apiKey != "" {
		upstream.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
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

	if upstreamTraceEnabled {
		observability.Get().Debug("OpenAI upstream response",
			"status", resp.StatusCode,
			"content_type", resp.Header.Get("Content-Type"),
			"transfer_encoding", resp.Header.Get("Transfer-Encoding"),
			"content_encoding", resp.Header.Get("Content-Encoding"),
			"request_id", resp.Header.Get("X-Request-Id"),
		)
	}

	providers.CopyUpstreamHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	status := resp.StatusCode

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, flushChunk)
	bytesRead := 0
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			t.StampUpstreamFirstByte()
			if upstreamTraceEnabled && bytesRead == 0 {
				observability.Get().Debug("OpenAI upstream first chunk",
					"bytes", n,
					"preview", truncateBytes(buf[:n], 320),
				)
			}
			bytesRead += n
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				if upstreamTraceEnabled {
					observability.Get().Debug("OpenAI upstream write failed", "err", writeErr, "bytes_read", bytesRead)
				}
				return writeErr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr == io.EOF {
			t.StampUpstreamEOF()
			if upstreamTraceEnabled {
				observability.Get().Debug("OpenAI upstream stream complete", "bytes_total", bytesRead)
			}
			if status < 200 || status >= 300 {
				return &providers.UpstreamStatusError{Status: status}
			}
			return nil
		}
		if readErr != nil {
			if upstreamTraceEnabled {
				observability.Get().Debug("OpenAI upstream read failed", "err", readErr, "bytes_read", bytesRead)
			}
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

// Passthrough forwards an inbound request to the same path on OpenAI without
// model rewriting or routing logic. Used for endpoints like /v1/models that
// clients probe for model availability.
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
	// Use per-request BYOK credentials when available; fall back to the
	// deployment-level API key (plan-based auth).
	if creds := proxy.CredentialsFromContext(ctx); creds != nil {
		upstream.Header.Set("Authorization", "Bearer "+string(creds.APIKey))
	} else if c.apiKey != "" {
		upstream.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
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
