// Package google is the providers.Client adapter for Google Gemini's
// OpenAI-compatible Chat Completions endpoint. The wire format on the request
// path is identical to OpenAI's /v1/chat/completions, so the upstream call,
// streaming, and most of the field-stripping logic mirror the openai adapter.
// Kept as a separate package (not a parameter on the openai client) so the
// composition root can register them under distinct provider names and the
// auth header / base URL stay obvious at a read.
package google

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"workweave/router/internal/observability/otel"
	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
)

// DefaultBaseURL is the public OpenAI-compatible endpoint for Gemini.
// Override via GOOGLE_PROVIDER_BASE_URL when pointing at a regional endpoint
// or a Vertex AI proxy that speaks the same wire format.
const DefaultBaseURL = "https://generativelanguage.googleapis.com/v1beta/openai"

const flushChunk = 4 * 1024

type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

// NewClient is pooled for sustained traffic to a single host.
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
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(prep.Body))
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

	providers.CopyUpstreamHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	status := resp.StatusCode

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, flushChunk)
	for {
		n, readErr := resp.Body.Read(buf)
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

// Passthrough forwards an inbound request to the same path on Gemini's
// OpenAI-compat surface without model rewriting. Used for /v1/models style
// probes that some clients send to discover capability.
//
// DefaultBaseURL already carries Gemini's /v1beta/openai version prefix, so
// the inbound /v1/... path must be stripped to its resource suffix
// (/models, /chat/completions, ...) before concatenating; otherwise the
// upstream URL would double-prefix to /v1beta/openai/v1/models and 404.
func (c *Client) Passthrough(ctx context.Context, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	suffix := strings.TrimPrefix(r.URL.Path, "/v1")
	url := c.baseURL + suffix
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
