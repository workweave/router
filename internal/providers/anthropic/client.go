// Package anthropic is the providers.Client adapter for Anthropic's Messages API.
package anthropic

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"workweave/router/internal/providers"
	"workweave/router/internal/providers/httputil"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
	"workweave/router/internal/timing"
)

const DefaultBaseURL = "https://api.anthropic.com"

type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client
	// sseIdleTimeout overrides httputil.DefaultSSEIdleTimeout when > 0; tests set
	// it small so the output-stall watchdog fires before this one.
	sseIdleTimeout time.Duration
	// outputStall overrides httputil.DefaultOutputStallTimeout when > 0; used by
	// tests to trip output-stall without waiting out the real budget.
	outputStall time.Duration
	// throughput* override the minimum-throughput watchdog budgets when set.
	throughputWindow     time.Duration
	throughputMinElapsed time.Duration
	throughputMinDeltas  int
	throughputOverride   bool
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

// NewClientWithStallTimeouts is NewClient with watchdog budgets injected for testing.
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

// throughputParams returns the minimum-throughput watchdog budgets: the injected
// test overrides when set, else the httputil defaults.
func (c *Client) throughputParams() (window, minElapsed time.Duration, minDeltas int) {
	if c.throughputOverride {
		return c.throughputWindow, c.throughputMinElapsed, c.throughputMinDeltas
	}
	return httputil.DefaultThroughputWindow, httputil.DefaultThroughputMinElapsed, httputil.DefaultMinThroughputDeltasPerWindow
}

// oauthBetaToken is the anthropic-beta flag Anthropic requires for Claude
// subscription (Claude.ai OAuth) tokens on /v1/messages.
const oauthBetaToken = "oauth-2025-04-20"

// subscriptionTokenPrefix marks a Claude subscription bearer (sk-ant-oat… /
// sk-ant-oat01…) used to gate the oauth beta header on the pure-passthrough path.
const subscriptionTokenPrefix = "sk-ant-oat"

// setAuth resolves credentials in precedence order: resolved per-request
// credential (subscription/BYOK/client), deployment key, then client-sent auth
// headers. A subscription OAuth credential authenticates via Authorization:
// Bearer and must NOT send x-api-key; everything else uses x-api-key.
//
// The passthrough tier scrubs router-issued Bearer tokens via
// httputil.SanitizeInboundAuthHeader before relaying inbound auth upstream.
func (c *Client) setAuth(ctx context.Context, upstream *http.Request, inbound *http.Request) {
	if creds := proxy.CredentialsFromContext(ctx); creds != nil {
		if creds.OAuth {
			upstream.Header.Set("authorization", "Bearer "+string(creds.APIKey))
			return
		}
		upstream.Header.Set("x-api-key", string(creds.APIKey))
		return
	}
	if c.apiKey != "" {
		upstream.Header.Set("x-api-key", c.apiKey)
		return
	}
	if v := httputil.SanitizeInboundAuthHeader(inbound.Header.Get("authorization")); v != "" {
		upstream.Header.Set("authorization", v)
	}
	if v := inbound.Header.Get("x-api-key"); v != "" {
		upstream.Header.Set("x-api-key", v)
	}
}

// applyOAuthBeta merges the oauth beta flag into anthropic-beta when the
// request authenticates with a Claude subscription token — either a resolved
// OAuth credential, or (pure passthrough, no deployment key) a raw inbound
// sk-ant-oat Authorization bearer. Must run AFTER prep.Headers is copied onto
// the upstream request so it merges with, rather than is clobbered by, the
// model-capability-filtered anthropic-beta that translate produced.
func applyOAuthBeta(ctx context.Context, upstream, inbound *http.Request) {
	if !subscriptionAuth(ctx, inbound) {
		return
	}
	upstream.Header.Set("anthropic-beta", mergeBeta(upstream.Header.Get("anthropic-beta"), oauthBetaToken))
}

// subscriptionAuth reports whether this request will authenticate with a Claude
// subscription OAuth token.
func subscriptionAuth(ctx context.Context, inbound *http.Request) bool {
	if creds := proxy.CredentialsFromContext(ctx); creds != nil {
		return creds.OAuth
	}
	if raw, found := strings.CutPrefix(inbound.Header.Get("authorization"), "Bearer "); found {
		return strings.HasPrefix(strings.TrimSpace(raw), subscriptionTokenPrefix)
	}
	return false
}

// mergeBeta appends token to a comma-separated anthropic-beta value if absent,
// preserving any existing (model-capability-filtered) tokens.
func mergeBeta(existing, token string) string {
	if existing == "" {
		return token
	}
	for _, p := range strings.Split(existing, ",") {
		if strings.TrimSpace(p) == token {
			return existing
		}
	}
	return existing + "," + token
}

func (c *Client) Proxy(ctx context.Context, decision router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(prep.Body))
	if err != nil {
		return fmt.Errorf("build upstream request: %w", err)
	}
	upstream.Header.Set("content-type", "application/json")
	c.setAuth(ctx, upstream, r)
	for k, vs := range prep.Headers {
		upstream.Header[http.CanonicalHeaderKey(k)] = vs
	}
	applyOAuthBeta(ctx, upstream, r)
	if v := r.Header.Get("accept"); v != "" {
		upstream.Header.Set("accept", v)
	}

	t := timing.TimingFrom(ctx)
	t.StampUpstreamRequest()
	resp, err := c.http.Do(upstream)
	if err != nil {
		return fmt.Errorf("upstream call: %w", err)
	}
	defer resp.Body.Close()
	t.StampUpstreamHeaders()
	// Surface subscription rate-limit headroom (anthropic-ratelimit-unified-*) to
	// the proxy's usage observer. Done for every response, including 429s where
	// the headroom signal matters most.
	providers.ObserveUpstreamHeaders(ctx, resp.Header)

	if resp.StatusCode >= 400 {
		bufBody, totalRead, drainErr := httputil.ReadCapped(resp.Body, providers.MaxBufferedErrorBytes)
		if len(bufBody) > 0 {
			t.StampUpstreamFirstByte()
		}
		if drainErr == nil {
			t.StampUpstreamEOF()
		}
		httputil.LogUpstreamStatus(
			"Upstream Anthropic returned error status",
			resp.StatusCode,
			"routed_model", decision.Model,
			"body_preview", httputil.PreviewBytes(bufBody),
			"body_total_bytes", totalRead,
		)
		errHeaders := http.Header{}
		providers.CopyUpstreamHeaders(httputil.HeaderCapture{H: errHeaders}, resp)
		return &providers.UpstreamErrorResponse{
			Status:  resp.StatusCode,
			Headers: errHeaders,
			Body:    bufBody,
		}
	}

	providers.CopyUpstreamHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)

	// Anthropic ping keepalives reset the byte-idle watchdog, so also arm
	// output-progress + throughput watchdogs — a ping-alive/zero-output stream
	// (prod: sonnet-5 stuck at 0 output, no failover) otherwise never aborts.
	if arm, ok := w.(providers.OutputProgressArmer); ok {
		outMark, outStop := httputil.StartIdleWatchdogCause(ctx, cancel, c.outputStallTimeout(), httputil.ErrUpstreamOutputStall)
		tpWindow, tpMinElapsed, tpMinDeltas := c.throughputParams()
		tpMark, tpStop := httputil.StartThroughputWatchdog(ctx, cancel, tpWindow, tpMinElapsed, tpMinDeltas, httputil.ErrUpstreamSlowThroughput)
		combined := func() {
			outMark()
			tpMark()
		}
		if arm.ArmOutputProgress(combined) {
			defer outStop()
			defer tpStop()
		} else {
			outStop()
			tpStop()
		}
	}

	return httputil.StreamBody(ctx, cancel, c.idleTimeout(), resp.Body, resp.StatusCode, w, t)
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
	applyOAuthBeta(ctx, upstream, r)
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
		return httputil.WritePassthroughError(w, resp, nil, nil, "Upstream Anthropic returned error status (passthrough)", "path", r.URL.Path)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

var _ providers.Client = (*Client)(nil)
