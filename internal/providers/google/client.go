// Package google is the providers.Client adapter for Gemini's OpenAI-compatible endpoint.
package google

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"workweave/router/internal/observability/otel"
	"workweave/router/internal/providers"
	"workweave/router/internal/providers/httputil"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
)

// DefaultBaseURL is the public OpenAI-compatible endpoint for Gemini.
const DefaultBaseURL = "https://generativelanguage.googleapis.com/v1beta/openai"

type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client
	// sseIdleTimeout, when > 0, overrides httputil.DefaultSSEIdleTimeout for the
	// byte-idle watchdog. Production uses the default; tests inject a small value
	// so the output-stall watchdog can be exercised without the byte-idle one
	// firing first.
	sseIdleTimeout time.Duration
	// outputStall, when > 0, overrides httputil.DefaultOutputStallTimeout for the
	// output-progress watchdog. Production uses the default; tests inject a small
	// value to drive the output-stall trip without waiting out the real budget.
	outputStall time.Duration
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

// NewClientWithStallTimeouts is NewClient with injected byte-idle and
// output-stall watchdog budgets. Exists so a test can drive the output-progress
// watchdog with a small budget while keeping the byte-idle watchdog large enough
// that it isn't what fires.
func NewClientWithStallTimeouts(apiKey, baseURL string, sseIdleTimeout, outputStall time.Duration) *Client {
	c := NewClient(apiKey, baseURL)
	c.sseIdleTimeout = sseIdleTimeout
	c.outputStall = outputStall
	return c
}

// idleTimeout returns the byte-idle watchdog budget: the injected test override
// when set, else httputil.DefaultSSEIdleTimeout.
func (c *Client) idleTimeout() time.Duration {
	if c.sseIdleTimeout > 0 {
		return c.sseIdleTimeout
	}
	return httputil.DefaultSSEIdleTimeout
}

// outputStallTimeout returns the output-progress watchdog budget: the injected
// test override when set, else httputil.DefaultOutputStallTimeout.
func (c *Client) outputStallTimeout() time.Duration {
	if c.outputStall > 0 {
		return c.outputStall
	}
	return httputil.DefaultOutputStallTimeout
}

func (c *Client) Proxy(ctx context.Context, decision router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(prep.Body))
	if err != nil {
		return fmt.Errorf("build upstream request: %w", err)
	}
	upstream.Header.Set("Content-Type", "application/json")
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

	// Output-progress watchdog: see NativeClient.Proxy for the rationale. The
	// byte-idle watchdog resets on any byte, so a byte-alive/output-silent stream
	// rides to the request cap; this second watchdog measures
	// time-since-last-OUTPUT, fed by the GeminiToOpenAISSETranslator's mark on
	// output-bearing events only.
	if arm, ok := w.(providers.OutputProgressArmer); ok {
		outMark, outStop := httputil.StartIdleWatchdogCause(ctx, cancel, c.outputStallTimeout(), httputil.ErrUpstreamOutputStall)
		if arm.ArmOutputProgress(outMark) {
			defer outStop()
		} else {
			outStop()
		}
	}

	return httputil.StreamBody(ctx, cancel, c.idleTimeout(), resp.Body, resp.StatusCode, w, t)
}

// Passthrough strips the inbound /v1 prefix to avoid double-prefixing with DefaultBaseURL.
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
