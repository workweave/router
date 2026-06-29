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
	"workweave/router/internal/observability"
	"workweave/router/internal/observability/otel"
	"workweave/router/internal/providers"
	"workweave/router/internal/providers/httputil"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
)

const (
	DefaultBaseURL   = "https://openrouter.ai/api/v1"
	FireworksBaseURL = "https://api.fireworks.ai/inference/v1"
	// DeepInfraBaseURL is DeepInfra's OpenAI-compatible surface. DeepInfra uses
	// HuggingFace-form model IDs; pair with NewClientWithModelIDMap to rewrite
	// the router's public slash-form slugs on the wire.
	DeepInfraBaseURL = "https://api.deepinfra.com/v1/openai"
	// MakoraBaseURL is Makora's OpenAI-compatible surface. Makora is an
	// agent-optimized inference platform serving DeepSeek V4 (and other OSS
	// models) at notably higher throughput than the commodity providers; pair
	// with NewClientWithModelIDMap to rewrite the router's public slash-form
	// slugs to Makora's upstream IDs on the wire.
	MakoraBaseURL = "https://inference.makora.com/v1"
	// TogetherBaseURL is Together AI's OpenAI-compatible surface. Together
	// serves the OSS pool (DeepSeek, GLM, MiniMax, Qwen, Kimi, …) and is the
	// fastest provider on artificialanalysis.ai for several of the models we
	// route; pair with NewClientWithModelIDMap to rewrite the router's public
	// slash-form slugs to Together's "Org/Model" upstream IDs on the wire.
	TogetherBaseURL = "https://api.together.xyz/v1"
)

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
	// modelIDMap rewrites the request body's "model" field before sending.
	// Empty map (or nil) = no rewrite. Used when the router's public slash-form
	// slug differs from the upstream's canonical ID (Bedrock dot-form,
	// DeepInfra HuggingFace-form).
	modelIDMap map[string]string
	// sseIdleTimeout, when > 0, overrides httputil.DefaultSSEIdleTimeout for the
	// byte-idle watchdog. Production uses the default; tests inject a small value
	// so the output-stall watchdog can be exercised without the byte-idle one
	// firing first.
	sseIdleTimeout time.Duration
	// outputStall, when > 0, overrides httputil.DefaultOutputStallTimeout for the
	// output-progress watchdog. Production uses the default via NewClient; tests
	// inject a small value to drive the output-stall trip without waiting out the
	// real budget.
	outputStall time.Duration
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
	return &Client{
		apiKey:     apiKey,
		baseURL:    strings.TrimRight(baseURL, "/"),
		http:       &http.Client{Transport: httputil.NewTransport(5*time.Second, 5*time.Second)},
		modelIDMap: modelIDMap,
	}
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
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build upstream request: %w", err)
	}
	upstream.Header.Set("Content-Type", "application/json")
	for k, vs := range prep.Headers {
		upstream.Header[http.CanonicalHeaderKey(k)] = vs
	}
	c.setAuth(ctx, upstream)
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

	if resp.StatusCode >= 400 {
		// Buffer the upstream error response — do NOT touch w. The proxy's
		// failover loop decides whether to retry on the next binding or
		// flush this buffer to the client.
		bufBody, totalRead, drainErr := readCapped(resp.Body, providers.MaxBufferedErrorBytes)
		if len(bufBody) > 0 {
			t.StampUpstreamFirstByte()
		}
		if drainErr == nil {
			t.StampUpstreamEOF()
		}
		logUpstreamStatus(
			"Upstream OpenAI-compatible provider returned error status",
			resp.StatusCode,
			"base_url", c.baseURL,
			"routed_model", decision.Model,
			"body_preview", previewBytes(bufBody),
			"body_total_bytes", totalRead,
		)
		errHeaders := http.Header{}
		providers.CopyUpstreamHeaders(headerCapture{errHeaders}, resp)
		return &providers.UpstreamErrorResponse{
			Status:  resp.StatusCode,
			Headers: errHeaders,
			Body:    bufBody,
		}
	}

	providers.CopyUpstreamHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)

	// Output-progress watchdog. StreamBody's byte-idle watchdog below resets on
	// ANY upstream byte, so a stream that stays byte-alive with SSE keepalive
	// comments or empty/role-only delta frames while producing zero output rides
	// to the 600s request cap (2026-06-19 DeepInfra incident). This second
	// watchdog measures time-since-last-OUTPUT: its mark is fed by the
	// OpenAI→Anthropic SSE translator only on output-bearing deltas (text,
	// reasoning, tool-call args, terminal finish). On trip it cancels ctx with
	// ErrUpstreamOutputStall (retryable; fails over while the preludeBuffer is
	// still uncommitted). Only the translator can tell output frames from
	// keepalives, so it is wired via ArmOutputProgress; a non-streaming client
	// (or a writer without the hook) returns armed=false and is byte-idle-guarded
	// only.
	if arm, ok := w.(providers.OutputProgressArmer); ok {
		outMark, outStop := httputil.StartIdleWatchdogCause(ctx, cancel, c.outputStallTimeout(), httputil.ErrUpstreamOutputStall)
		if arm.ArmOutputProgress(outMark) {
			defer outStop()
		} else {
			outStop()
		}
	}

	streamErr := httputil.StreamBody(ctx, cancel, c.idleTimeout(), resp.Body, resp.StatusCode, w, t)
	if errors.Is(streamErr, httputil.ErrUpstreamIdleTimeout) || errors.Is(streamErr, httputil.ErrUpstreamOutputStall) {
		logStreamStall(decision.Model, c.baseURL, streamErr)
	}
	return streamErr
}

// logStreamStall reports a watchdog trip at ERROR: the upstream returned 200 +
// headers, then stalled for the full budget. byte_idle = zero bytes for the
// idle budget; output_idle = stream stayed byte-alive on keepalive/empty frames
// but produced zero output content for the output-stall budget (the 2026-06-19
// DeepInfra mode). Both classify retryable, so dispatchWithFallback re-attempts
// when nothing reached the client; this log is the per-model paper trail.
func logStreamStall(model, baseURL string, cause error) {
	stallKind := "byte_idle"
	if errors.Is(cause, httputil.ErrUpstreamOutputStall) {
		stallKind = "output_idle"
	}
	observability.Get().Error("Upstream OpenAI-compatible stream stalled mid-response; aborting for retry",
		"model", model,
		"base_url", baseURL,
		"stall_kind", stallKind,
	)
}

// readCapped reads up to limit bytes from r into a buffer, then drains the
// rest without retention up to maxDrain to bound failover latency on a
// slow upstream returning a large error body. The connection is closed by
// the caller's defer regardless, so the unread tail is discarded by Close.
// Returns the buffered prefix, total bytes read, and any read error
// (io.EOF mapped to nil).
func readCapped(r io.Reader, limit int) ([]byte, int64, error) {
	prefix, err := io.ReadAll(io.LimitReader(r, int64(limit)))
	totalRead := int64(len(prefix))
	if err != nil {
		return prefix, totalRead, err
	}
	const maxDrain = 1 << 20 // 1 MiB
	rest, drainErr := io.Copy(io.Discard, io.LimitReader(r, maxDrain))
	totalRead += rest
	return prefix, totalRead, drainErr
}

// previewBytes returns the first 1KB of body as a string for logging.
func previewBytes(body []byte) string {
	const previewLimit = 1024
	if len(body) > previewLimit {
		return string(body[:previewLimit])
	}
	return string(body)
}

// headerCapture is a minimal http.ResponseWriter that captures headers
// only, used to reuse providers.CopyUpstreamHeaders against an http.Header
// we own. Write/WriteHeader are no-ops.
type headerCapture struct{ h http.Header }

func (c headerCapture) Header() http.Header       { return c.h }
func (c headerCapture) Write([]byte) (int, error) { return 0, nil }
func (c headerCapture) WriteHeader(int)           {}

// Passthrough strips the inbound /v1 prefix to avoid double-prefixing with the configured baseURL.
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
	for k, vs := range prep.Headers {
		upstream.Header[http.CanonicalHeaderKey(k)] = vs
	}
	c.setAuth(ctx, upstream)
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
		var snip [1024]byte
		n, _ := io.ReadFull(resp.Body, snip[:])
		_, snipWriteErr := w.Write(snip[:n])
		rest, copyErr := io.Copy(w, resp.Body)
		logUpstreamStatus(
			"Upstream OpenAI-compatible provider returned error status (passthrough)",
			resp.StatusCode,
			"base_url", c.baseURL,
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

// logUpstreamStatus logs non-2xx upstream responses with a body preview.
func logUpstreamStatus(msg string, status int, attrs ...any) {
	merged := append([]any{"status", status}, attrs...)
	if status >= 500 || (status >= 400 && status != http.StatusTooManyRequests) {
		observability.Get().Error(msg, merged...)
		return
	}
	observability.Get().Warn(msg, merged...)
}

var _ providers.Client = (*Client)(nil)
