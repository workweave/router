// Package openaicompat is a generic providers.Client adapter for upstreams
// that expose the OpenAI Chat Completions wire shape.
//
// OpenRouter, vLLM, Together, Fireworks, DeepInfra, and customer-hosted
// OpenAI-compatible endpoints all use the same request/response path. Keeping
// this separate from the first-party OpenAI adapter lets the composition root
// register those pools under their own provider names and API keys.
package openaicompat

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
	"workweave/router/internal/router"
)

const DefaultBaseURL = "https://openrouter.ai/api/v1"

const flushChunk = 4 * 1024

type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

// NewClient is pooled for sustained traffic to a single OpenAI-compatible host.
func NewClient(apiKey, baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
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

// Complete is intentionally unimplemented. All chat completions traffic flows
// through Proxy after the request envelope has been translated.
func (c *Client) Complete(_ context.Context, _ providers.Request) (providers.Response, error) {
	return providers.Response{}, providers.ErrNotImplemented
}

func (c *Client) Proxy(ctx context.Context, decision router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(prep.Body))
	if err != nil {
		return fmt.Errorf("build upstream request: %w", err)
	}
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("Authorization", "Bearer "+c.apiKey)
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

// Passthrough forwards an inbound request to the same resource suffix on the
// upstream OpenAI-compatible endpoint. The configured baseURL already includes
// the provider's version prefix (for example /api/v1), so strip the inbound
// /v1 before joining.
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
	upstream.Header.Set("Authorization", "Bearer "+c.apiKey)
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
