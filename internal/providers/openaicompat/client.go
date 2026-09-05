// Package openaicompat is a generic providers.Client adapter for upstreams that speak OpenAI Chat Completions.
package openaicompat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"weave-os/router/internal/observability"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/providers/httputil"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/router"
	"weave-os/router/internal/timing"
)

const (
	DefaultBaseURL   = "https://openrouter.ai/api/v1"
	FireworksBaseURL = "https://api.fireworks.ai/inference/v1"
	// MakoraBaseURL serves the OSS catalog at higher throughput than commodity
	// providers; pair with NewClientWithModelIDMap to rewrite slugs to Makora's
	// upstream IDs.
	MakoraBaseURL = "https://inference.makora.com/v1"
	// MiniMaxGlobalBaseURL is MiniMax's global OpenAI-compatible endpoint.
	MiniMaxGlobalBaseURL = "https://api.minimax.io/v1"
	// MiniMaxCNBaseURL is MiniMax's mainland-China OpenAI-compatible endpoint.
	MiniMaxCNBaseURL = "https://api.minimaxi.com/v1"
	// TogetherBaseURL serves the OSS pool (DeepSeek, GLM, MiniMax, Qwen, Kimi)
	// and is fastest on artificialanalysis.ai for several routed models; pair
	// with NewClientWithModelIDMap to rewrite slugs to Together's "Org/Model" IDs.
	TogetherBaseURL = "https://api.together.xyz/v1"
	// XAIBaseURL is SpaceXAI's OpenAI-compatible Chat Completions surface.
	XAIBaseURL = "https://api.x.ai/v1"
	// MetaBaseURL is Meta's Model API OpenAI-compatible Chat Completions surface.
	MetaBaseURL = "https://api.meta.ai/v1"
	// WaferBaseURL is Wafer Serverless' OpenAI-compatible surface.
	WaferBaseURL = "https://pass.wafer.ai/v1"
)

// MiniMaxBaseURL returns the regional MiniMax OpenAI-compatible endpoint.
// The global endpoint is the safe default for unset or unrecognized regions.
func MiniMaxBaseURL(region string) string {
	switch strings.ToLower(strings.TrimSpace(region)) {
	case "cn", "china":
		return MiniMaxCNBaseURL
	default:
		return MiniMaxGlobalBaseURL
	}
}

// grokResponseHeaderTimeout is the time-to-first-byte guard for Grok models;
// Snowflake Cortex prefill can push first-byte past the default 30s. Streaming
// inactivity stays bounded by StreamBody's idle watchdog. Tunable via
// ROUTER_GROK_HEADER_TIMEOUT_SECONDS.
var grokResponseHeaderTimeout = httputil.TimeoutFromEnv("ROUTER_GROK_HEADER_TIMEOUT_SECONDS", 90*time.Second)

// BedrockMantleBaseURLTemplate is the OpenAI-compatible bedrock-mantle endpoint
// template. Substitute the region via BedrockMantleBaseURL.
const BedrockMantleBaseURLTemplate = "https://bedrock-mantle.%s.api.aws/v1"

// BedrockMantleBaseURL returns the bedrock-mantle URL for the given region.
// AWS recommends bedrock-mantle over the model-native bedrock-runtime surface
// for OpenAI-compat traffic; auth is a long-term Bedrock API key, not SigV4.
func BedrockMantleBaseURL(region string) string {
	if region == "" {
		region = "us-east-1"
	}
	return fmt.Sprintf(BedrockMantleBaseURLTemplate, region)
}

type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client
	// grokHTTP carries the wider time-to-first-byte guard used for Grok
	// models; see grokResponseHeaderTimeout.
	grokHTTP *http.Client
	// modelIDMap rewrites the request body's "model" field before sending, when
	// the router's public slug differs from the upstream's canonical ID
	// (Bedrock dot-form, Makora/Together HuggingFace-form). Nil/empty = no rewrite.
	modelIDMap map[string]string
	// sseIdleTimeout overrides httputil.DefaultSSEIdleTimeout when > 0; tests
	// set it small so the output-stall watchdog fires before this one.
	sseIdleTimeout time.Duration
	// outputStall overrides httputil.DefaultOutputStallTimeout when > 0; used
	// by tests to trip output-stall without waiting out the real budget.
	outputStall time.Duration
	// throughputWindow/MinElapsed/MinDeltas override the minimum-throughput
	// watchdog budgets when set; used by tests to trip slow-throughput without
	// waiting out the real warmup.
	throughputWindow     time.Duration
	throughputMinElapsed time.Duration
	throughputMinDeltas  int
	throughputOverride   bool
	// defaultHeaders are set on every upstream request before prep.Headers
	// apply, so per-request values override them.
	defaultHeaders http.Header
	// protectedHeaders are set after prep.Headers / inbound headers apply, so
	// provider-mandated values cannot be overridden.
	protectedHeaders http.Header
	// versionMemo resolves gateway base URLs that mount chat/completions one
	// "/v1" below where the stored base URL points.
	versionMemo providers.GatewayVersionMemo
}

func NewClient(apiKey, baseURL string) *Client {
	return NewClientWithModelIDMap(apiKey, baseURL, nil)
}

// NewClientWithModelIDMap builds a client that rewrites the body's top-level
// "model" field before forwarding when the requested model has a mapping.
// Pass nil to disable rewriting.
func NewClientWithModelIDMap(apiKey, baseURL string, modelIDMap map[string]string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return newClient(apiKey, baseURL, modelIDMap)
}

// NewGatewayClient builds a client for a customer-supplied OpenAI-spec endpoint.
// Unlike NewClient, no default base URL is applied: an unconfigured gateway must
// fail rather than silently dispatch the tenant's token to OpenRouter.
func NewGatewayClient(apiKey, baseURL string) *Client {
	return newClient(apiKey, baseURL, nil)
}

func newClient(apiKey, baseURL string, modelIDMap map[string]string) *Client {
	return &Client{
		apiKey:     apiKey,
		baseURL:    strings.TrimRight(baseURL, "/"),
		http:       httputil.NewClient(httputil.NewTransport(5*time.Second, 5*time.Second)),
		grokHTTP:   httputil.NewClient(httputil.NewTransportWithResponseHeaderTimeout(5*time.Second, 5*time.Second, grokResponseHeaderTimeout)),
		modelIDMap: modelIDMap,
	}
}

// httpFor picks the HTTP client for a routed model: Grok models get the
// wider time-to-first-byte guard, everything else the default transport.
func (c *Client) httpFor(model string) *http.Client {
	if strings.HasPrefix(model, "grok") && c.grokHTTP != nil {
		return c.grokHTTP
	}
	return c.http
}

// WithDefaultHeaders returns c with headers set on every upstream request.
// Prepared and inbound per-request headers can override these values.
func (c *Client) WithDefaultHeaders(h http.Header) *Client {
	c.defaultHeaders = h.Clone()
	return c
}

// WithProtectedHeaders returns c with headers that cannot be overridden by
// prepared or inbound per-request headers.
func (c *Client) WithProtectedHeaders(h http.Header) *Client {
	c.protectedHeaders = h.Clone()
	return c
}

// applyDefaultHeaders sets c.defaultHeaders on req before callers layer their
// own headers on top.
func (c *Client) applyDefaultHeaders(req *http.Request) {
	for k, vs := range c.defaultHeaders {
		req.Header[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
	}
}

// applyProtectedHeaders restores c.protectedHeaders after callers layer their
// own headers.
func (c *Client) applyProtectedHeaders(req *http.Request) {
	for k, vs := range c.protectedHeaders {
		req.Header[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
	}
}

// NewClientWithHeaderTimeouts is NewClient with injected header-timeout
// values so tests can exercise transport selection without real-latency waits.
func NewClientWithHeaderTimeouts(apiKey, baseURL string, defaultTimeout, grokTimeout time.Duration) *Client {
	c := NewClient(apiKey, baseURL)
	c.http = httputil.NewClient(httputil.NewTransportWithResponseHeaderTimeout(5*time.Second, 5*time.Second, defaultTimeout))
	c.grokHTTP = httputil.NewClient(httputil.NewTransportWithResponseHeaderTimeout(5*time.Second, 5*time.Second, grokTimeout))
	return c
}

// NewClientWithStallTimeouts is NewClient with injected byte-idle and
// output-stall watchdog budgets. Exists so a test can drive the
// output-progress watchdog with a small budget while keeping the byte-idle
// watchdog large enough that it isn't what fires.
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

// NewClientWithThroughputGuard is NewClient with injected minimum-throughput
// watchdog budgets, so a test can drive a slow-throughput trip with a tiny
// warmup/window while keeping the idle and output-stall watchdogs large enough
// that they aren't what fire.
func NewClientWithThroughputGuard(apiKey, baseURL string, window, minElapsed time.Duration, minDeltas int) *Client {
	c := NewClient(apiKey, baseURL)
	c.throughputOverride = true
	c.throughputWindow = window
	c.throughputMinElapsed = minElapsed
	c.throughputMinDeltas = minDeltas
	return c
}

// rewriteModelField rewrites the body's top-level "model" field according to
// modelIDMap. Returns the input unchanged when the map is empty, the body
// isn't a JSON object, or the model isn't mapped.
func rewriteModelField(body []byte, modelIDMap map[string]string) []byte {
	if len(modelIDMap) == 0 || len(body) == 0 {
		return body
	}
	model := gjson.GetBytes(body, "model").String()
	upstream, ok := modelIDMap[model]
	if !ok {
		return body
	}
	out, err := sjson.SetBytes(body, "model", upstream)
	if err != nil {
		return body
	}
	return out
}

// effectiveBaseURL resolves this request's base URL, folding an Azure resource
// endpoint onto /openai/v1 and a Snowflake Cortex REST root onto
// /api/v2/cortex/v1 — the OpenAI-spec surfaces they actually serve.
func (c *Client) effectiveBaseURL(ctx context.Context) string {
	base := providers.NormalizeAzureOpenAIBaseURL(proxy.EffectiveBaseURL(ctx, c.baseURL))
	return providers.NormalizeSnowflakeCortexOpenAIBaseURL(base)
}

// setAuth sets the Authorization header, preferring BYOK credentials over the deployment-level key.
func (c *Client) setAuth(ctx context.Context, upstream *http.Request) {
	if creds := proxy.CredentialsFromContext(ctx); creds != nil {
		upstream.Header.Set("Authorization", "Bearer "+string(creds.APIKey))
		return
	}
	if c.apiKey != "" {
		upstream.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
}

func (c *Client) Proxy(ctx context.Context, decision router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	body := rewriteModelField(prep.Body, c.modelIDMap)
	// Applied after the catalog map so a BYOK endpoint's own naming wins.
	body = proxy.ApplyModelAlias(ctx, body, decision.Model)
	baseURL := c.effectiveBaseURL(ctx)

	// EndpointResponses is the Responses surface: reasoning models reject a tool
	// turn on chat/completions, and gateways that mount /v1/responses (Snowflake
	// Cortex) serve it there. The proxy re-emits onto chat/completions when the
	// gateway answers that it has no such surface.
	suffix := "/chat/completions"
	if prep.Endpoint == providers.EndpointResponses {
		suffix = "/responses"
	}

	// 404s are buffered before reaching w, so a missing "/v1" segment can be retried.
	// A non-404 on the versioned path is the real error and is memoized; only a
	// second 404 falls back to the probe so a genuine model-not-found is preserved.
	urls := c.versionMemo.URLs(baseURL, suffix)
	firstErr := c.proxyTo(ctx, cancel, urls[0], baseURL, body, decision, prep, w, r)
	if len(urls) == 1 || !providers.IsUpstreamModelNotFound(firstErr) {
		return firstErr
	}
	err := c.proxyTo(ctx, cancel, urls[1], baseURL, body, decision, prep, w, r)
	if err == nil || !providers.IsUpstreamModelNotFound(err) {
		c.versionMemo.Learn(baseURL)
		return err
	}
	return firstErr
}

func (c *Client) proxyTo(ctx context.Context, cancel context.CancelCauseFunc, url, baseURL string, body []byte, decision router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build upstream request: %w", err)
	}
	c.applyDefaultHeaders(upstream)
	upstream.Header.Set("Content-Type", "application/json")
	for k, vs := range prep.Headers {
		upstream.Header[http.CanonicalHeaderKey(k)] = vs
	}
	c.applyProtectedHeaders(upstream)
	c.setAuth(ctx, upstream)
	proxy.ApplyWIFTokenType(ctx, upstream)
	proxy.ApplyIdentityHeader(ctx, upstream)
	proxy.ApplyForwardedClientHeaders(ctx, upstream, r.Header)
	if v := r.Header.Get("Accept"); v != "" {
		upstream.Header.Set("Accept", v)
	}

	t := timing.TimingFrom(ctx)
	t.StampUpstreamRequest()
	resp, err := c.httpFor(decision.Model).Do(upstream)
	if err != nil {
		return fmt.Errorf("upstream call: %w", err)
	}
	defer resp.Body.Close()
	t.StampUpstreamHeaders()

	if resp.StatusCode >= 400 {
		// Buffer the error — do NOT touch w; the failover loop decides whether
		// to retry or flush this buffer to the client.
		bufBody, totalRead, drainErr := httputil.ReadCapped(resp.Body, providers.MaxBufferedErrorBytes)
		if len(bufBody) > 0 {
			t.StampUpstreamFirstByte()
		}
		if drainErr == nil {
			t.StampUpstreamEOF()
		}
		httputil.LogUpstreamStatus(
			ctx,
			"Upstream OpenAI-compatible provider returned error status",
			resp.StatusCode,
			"base_url", baseURL,
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

	// Output-progress watchdog: StreamBody's byte-idle watchdog resets on ANY
	// byte, so keepalive/empty-delta frames with zero real output ride to the
	// request cap (2026-06-19 DeepInfra incident). This one is marked only on
	// output-bearing deltas by the SSE translator (via ArmOutputProgress) and
	// trips ErrUpstreamOutputStall (retryable). Non-streaming/no-hook writers
	// stay byte-idle-guarded only.
	//
	// A second watchdog shares the same mark: minimum-throughput. It catches a
	// clean 200 that keeps dribbling output at a crawl (2026-06-25
	// deepseek-v4-flash ~132s dribble) by counting deltas over a rolling window
	// and tripping ErrUpstreamSlowThroughput once warmup passes.
	if arm, ok := w.(providers.OutputProgressArmer); ok {
		outMark, outStop := httputil.StartIdleWatchdogCause(ctx, cancel, c.outputStallTimeout(), httputil.ErrUpstreamOutputStall)
		tpWindow, tpMinElapsed, tpMinDeltas := c.throughputParams()
		tpMark, tpStop := httputil.StartThroughputWatchdog(ctx, cancel, tpWindow, tpMinElapsed, tpMinDeltas, httputil.ErrUpstreamSlowThroughput)
		combined := func() {
			outMark()
			tpMark()
		}
		if arm.ArmOutputProgress(timing.FirstOutputMark(ctx, combined)) {
			defer outStop()
			defer tpStop()
		} else {
			outStop()
			tpStop()
		}
	}

	streamErr := httputil.StreamBody(ctx, cancel, c.idleTimeout(), resp.Body, resp.StatusCode, w, t)
	if errors.Is(streamErr, httputil.ErrUpstreamIdleTimeout) || errors.Is(streamErr, httputil.ErrUpstreamOutputStall) || errors.Is(streamErr, httputil.ErrUpstreamSlowThroughput) {
		logStreamStall(ctx, decision.Model, baseURL, streamErr)
	}
	return streamErr
}

// logStreamStall reports a watchdog trip at ERROR after upstream returned
// 200 + headers then stalled for the full budget. Both stall kinds are
// retryable (dispatchWithFallback re-attempts); this is the paper trail.
func logStreamStall(ctx context.Context, model, baseURL string, cause error) {
	stallKind := "byte_idle"
	switch {
	case errors.Is(cause, httputil.ErrUpstreamOutputStall):
		stallKind = "output_idle"
	case errors.Is(cause, httputil.ErrUpstreamSlowThroughput):
		stallKind = "slow_throughput"
	}
	observability.FromContext(ctx).Error("Upstream OpenAI-compatible stream stalled mid-response; aborting for retry",
		"model", model,
		"base_url", baseURL,
		"stall_kind", stallKind,
	)
}

// Passthrough strips the inbound /v1 prefix to avoid double-prefixing with the configured baseURL.
func (c *Client) Passthrough(ctx context.Context, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	suffix := strings.TrimPrefix(r.URL.Path, "/v1")
	baseURL := c.effectiveBaseURL(ctx)
	url := baseURL + suffix
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}

	upstream, err := http.NewRequestWithContext(ctx, r.Method, url, bytes.NewReader(prep.Body))
	if err != nil {
		return fmt.Errorf("build upstream passthrough request: %w", err)
	}
	c.applyDefaultHeaders(upstream)
	if ct := r.Header.Get("Content-Type"); ct != "" {
		upstream.Header.Set("Content-Type", ct)
	}
	for k, vs := range prep.Headers {
		upstream.Header[http.CanonicalHeaderKey(k)] = vs
	}
	c.applyProtectedHeaders(upstream)
	c.setAuth(ctx, upstream)
	proxy.ApplyWIFTokenType(ctx, upstream)
	proxy.ApplyForwardedClientHeaders(ctx, upstream, r.Header)
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
	if resp.StatusCode >= 400 {
		return httputil.WritePassthroughError(r.Context(), w, resp, nil, nil, "Upstream OpenAI-compatible provider returned error status (passthrough)", "base_url", baseURL, "path", r.URL.Path)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

var _ providers.Client = (*Client)(nil)
