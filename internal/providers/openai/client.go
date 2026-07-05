// Package openai is the providers.Client adapter for OpenAI's Chat Completions API.
package openai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"workweave/router/internal/observability"
	"workweave/router/internal/providers"
	"workweave/router/internal/providers/httputil"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
	"workweave/router/internal/timing"
)

const DefaultBaseURL = "https://api.openai.com"

// Codex (ChatGPT) subscription backend. A ChatGPT plan authenticates only
// against this base URL over the Responses API — never api.openai.com — and
// requires ChatGPT-Account-ID paired with the OAuth bearer (401/403 without
// it). Mirrors what the Codex CLI sends (codex_cli_rs).
const (
	chatGPTCodexBaseURL   = "https://chatgpt.com/backend-api/codex"
	codexResponsesPath    = "/responses"
	codexAccountIDHeader  = "ChatGPT-Account-ID"
	codexOpenAIBetaHeader = "OpenAI-Beta"
	codexOpenAIBetaValue  = "responses=experimental"
	codexOriginatorHeader = "originator"
	codexOriginatorValue  = "codex_cli_rs"
	codexUserAgentHeader  = "User-Agent"
	codexUserAgentValue   = "codex_cli_rs"
)

// codexSubscriptionCreds returns the resolved credential when it's a Codex
// (ChatGPT) subscription bearer (OAuth token with a paired account id), else
// nil. Such a turn must dispatch to the Codex backend, not api.openai.com.
func codexSubscriptionCreds(ctx context.Context) *proxy.Credentials {
	creds := proxy.CredentialsFromContext(ctx)
	if creds != nil && creds.OAuth && len(creds.AccountID) > 0 {
		return creds
	}
	return nil
}

// responseHeaderTimeout is raised above the 30s default because the Responses
// API (gpt-5.x reasoning) can take well over 30s to emit its first event under
// high effort, false-tripping the default. StreamBody's idle watchdog still
// bounds inter-event gaps once streaming starts, so this can't reintroduce an
// unbounded hang.
const responseHeaderTimeout = 120 * time.Second

type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client
	// sseIdleTimeout, when > 0, overrides the per-endpoint idle-progress
	// threshold. Tests inject a small value to exercise the stall watchdog
	// without waiting out the real threshold.
	sseIdleTimeout time.Duration
	// outputStall, when > 0, overrides the /v1/responses output-progress
	// watchdog budget for tests, same reason as sseIdleTimeout.
	outputStall time.Duration
	// codexBaseURL is the Codex (ChatGPT) subscription backend base URL;
	// tests override it to point at an httptest server.
	codexBaseURL string
}

func NewClient(apiKey, baseURL string) *Client {
	return NewClientWithResponseHeaderTimeout(apiKey, baseURL, responseHeaderTimeout)
}

// NewClientWithResponseHeaderTimeout is NewClient with a caller-chosen
// time-to-first-byte guard, so tests can exercise bounded-stall behavior
// (#331) without waiting out the 120s default.
func NewClientWithResponseHeaderTimeout(apiKey, baseURL string, headerTimeout time.Duration) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		apiKey:       apiKey,
		baseURL:      baseURL,
		codexBaseURL: chatGPTCodexBaseURL,
		http:         &http.Client{Transport: httputil.NewTransportWithResponseHeaderTimeout(5*time.Second, 5*time.Second, headerTimeout)},
	}
}

// NewClientWithTimeouts is NewClientWithResponseHeaderTimeout with an
// additional injected SSE idle-progress threshold; see Client.sseIdleTimeout.
func NewClientWithTimeouts(apiKey, baseURL string, headerTimeout, sseIdleTimeout time.Duration) *Client {
	c := NewClientWithResponseHeaderTimeout(apiKey, baseURL, headerTimeout)
	c.sseIdleTimeout = sseIdleTimeout
	return c
}

// NewClientWithStallTimeouts additionally injects the /v1/responses
// output-stall threshold (Client.outputStall) for tests.
func NewClientWithStallTimeouts(apiKey, baseURL string, headerTimeout, sseIdleTimeout, outputStall time.Duration) *Client {
	c := NewClientWithTimeouts(apiKey, baseURL, headerTimeout, sseIdleTimeout)
	c.outputStall = outputStall
	return c
}

// idleTimeoutFor picks the idle-progress threshold; /v1/responses gets the
// more generous reasoning budget.
func (c *Client) idleTimeoutFor(endpoint providers.Endpoint) time.Duration {
	if c.sseIdleTimeout > 0 {
		return c.sseIdleTimeout
	}
	if endpoint == providers.EndpointResponses {
		return httputil.DefaultResponsesSSEIdleTimeout
	}
	return httputil.DefaultSSEIdleTimeout
}

// outputStallTimeout picks the /v1/responses output-progress watchdog budget:
// the injected test override when set, else httputil.DefaultResponsesOutputStallTimeout.
func (c *Client) outputStallTimeout() time.Duration {
	if c.outputStall > 0 {
		return c.outputStall
	}
	return httputil.DefaultResponsesOutputStallTimeout
}

// stallBudgetFor returns the budget the watchdog identified by cause used, so
// logStreamStall reports the threshold that actually fired.
func (c *Client) stallBudgetFor(endpoint providers.Endpoint, cause error) time.Duration {
	if errors.Is(cause, httputil.ErrUpstreamOutputStall) {
		return c.outputStallTimeout()
	}
	return c.idleTimeoutFor(endpoint)
}

// setAuth applies authentication to the upstream request. Precedence:
// (1) per-request BYOK credentials in ctx; (2) deployment-level API key;
// (3) passthrough of the inbound auth header (Codex plan flow).
//
// The passthrough tier strips `Authorization: Bearer rk_...` — the router
// auth middleware accepts the same header for router-key auth, so we must not
// relay a router credential to OpenAI. Mirrors proxy.ExtractClientCredentials's
// !HasAPIKeyPrefix guard.
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
	// Skip forwarding only if the Bearer token is a router-issued key; any
	// other shape is forwarded as-is (upstream 401s on invalid creds). See
	// httputil.SanitizeInboundAuthHeader for the shared scrub guard.
	if v = httputil.SanitizeInboundAuthHeader(v); v == "" {
		return
	}
	upstream.Header.Set("Authorization", v)
}

func (c *Client) Proxy(ctx context.Context, decision router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	// Gate Codex dispatch on EndpointResponses too, so a chat-completions body
	// that happens to resolve a Codex credential never hits the Codex
	// /responses endpoint (Responses schema only).
	codexCreds := codexSubscriptionCreds(ctx)
	useCodex := codexCreds != nil && prep.Endpoint == providers.EndpointResponses
	baseURL := c.baseURL
	path := "/v1/chat/completions"
	if prep.Endpoint == providers.EndpointResponses {
		path = "/v1/responses"
	}
	if useCodex {
		baseURL = c.codexBaseURL
		path = codexResponsesPath
	}
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(prep.Body))
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
	// Set after the prep.Headers copy so these can't be clobbered.
	if useCodex {
		upstream.Header.Set(codexAccountIDHeader, string(codexCreds.AccountID))
		upstream.Header.Set(codexOpenAIBetaHeader, codexOpenAIBetaValue)
		upstream.Header.Set(codexOriginatorHeader, codexOriginatorValue)
		upstream.Header.Set(codexUserAgentHeader, codexUserAgentValue)
	}

	t := timing.TimingFrom(ctx)
	t.StampUpstreamRequest()
	resp, err := c.http.Do(upstream)
	if err != nil {
		return fmt.Errorf("upstream call: %w", err)
	}
	defer resp.Body.Close()
	t.StampUpstreamHeaders()
	// Surface Codex rate-limit headroom (x-codex-*) even on 429s, where it matters most.
	providers.ObserveUpstreamHeaders(ctx, resp.Header)

	idleTimeout := c.idleTimeoutFor(prep.Endpoint)
	log := observability.Get()
	log.Debug("OpenAI upstream response",
		"status", resp.StatusCode,
		"content_type", resp.Header.Get("Content-Type"),
		"transfer_encoding", resp.Header.Get("Transfer-Encoding"),
		"content_encoding", resp.Header.Get("Content-Encoding"),
		"request_id", resp.Header.Get("X-Request-Id"),
	)

	// Buffer non-2xx as UpstreamErrorResponse so the dispatch loop can fail
	// over or render the error in the inbound format. Writing the status
	// straight through would corrupt the SSE stream if a translator's Prelude
	// already committed `200 + message_start`, and would drop the error body.
	if resp.StatusCode >= 400 {
		// Same idle watchdog as the streaming path: readCapped's blocking
		// reads would otherwise hang on an upstream that returns error
		// headers then stalls the body (header timeout no longer applies).
		mark, stop := httputil.StartIdleWatchdog(ctx, cancel, idleTimeout)
		body := &progressReader{r: resp.Body, mark: mark}
		bufBody, totalRead, drainErr := readCapped(body, providers.MaxBufferedErrorBytes)
		stop()
		if errors.Is(context.Cause(ctx), httputil.ErrUpstreamIdleTimeout) {
			logStreamStall(decision.Model, path, idleTimeout, totalRead, httputil.ErrUpstreamIdleTimeout)
		}
		if len(bufBody) > 0 {
			t.StampUpstreamFirstByte()
		}
		if drainErr == nil {
			t.StampUpstreamEOF()
		}
		logUpstreamStatus(
			"Upstream OpenAI returned error status",
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
	status := resp.StatusCode

	// Output-progress watchdog (Responses only): the byte-idle watchdog below
	// resets on ANY byte, so a stream alive on reasoning/keepalive frames but
	// producing zero output rides to the 600s cap (2026-06-16 incident). This
	// one measures time-since-last-OUTPUT, fed by the translator only on
	// output-bearing events, and trips ErrUpstreamOutputStall (retryable).
	// Wired via ArmOutputProgress since only the translator can tell output
	// frames from reasoning/keepalive frames.
	if prep.Endpoint == providers.EndpointResponses {
		if arm, ok := w.(providers.OutputProgressArmer); ok {
			outMark, outStop := httputil.StartIdleWatchdogCause(ctx, cancel, c.outputStallTimeout(), httputil.ErrUpstreamOutputStall)
			if arm.ArmOutputProgress(outMark) {
				defer outStop()
			} else {
				outStop()
			}
		}
	}

	// Manual stream loop for per-chunk diagnostics; non-debug takes the fast path.
	if !log.Enabled(ctx, slog.LevelDebug) {
		body := &progressReader{r: resp.Body}
		streamErr := httputil.StreamBody(ctx, cancel, idleTimeout, body, status, w, t)
		if errors.Is(streamErr, httputil.ErrUpstreamIdleTimeout) || errors.Is(streamErr, httputil.ErrUpstreamOutputStall) {
			logStreamStall(decision.Model, path, c.stallBudgetFor(prep.Endpoint, streamErr), body.n, streamErr)
		}
		return streamErr
	}

	mark, stop := httputil.StartIdleWatchdog(ctx, cancel, idleTimeout)
	defer stop()

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, httputil.FlushChunk)
	bytesRead := 0
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			mark()
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
			if cause := context.Cause(ctx); errors.Is(cause, httputil.ErrUpstreamIdleTimeout) || errors.Is(cause, httputil.ErrUpstreamOutputStall) {
				logStreamStall(decision.Model, path, c.stallBudgetFor(prep.Endpoint, cause), int64(bytesRead), cause)
				return cause
			}
			log.Debug("OpenAI upstream read failed", "err", readErr, "bytes_read", bytesRead)
			return readErr
		}
	}
}

// progressReader counts upstream bytes for the stall log's bytes_received
// field and reports each read to mark (optional, nil-safe) for watchdog
// paths outside StreamBody's built-in marking. Single-goroutine use only.
type progressReader struct {
	r    io.Reader
	mark func()
	n    int64
}

func (p *progressReader) Read(buf []byte) (n int, err error) {
	n, err = p.r.Read(buf)
	if n > 0 {
		p.n += int64(n)
		if p.mark != nil {
			p.mark()
		}
	}
	return n, err
}

// logStreamStall reports a watchdog trip at ERROR, distinguishing two modes
// via stall_kind: byte_idle (ErrUpstreamIdleTimeout, zero bytes — 2026-06-09
// incident) vs output_idle (ErrUpstreamOutputStall, bytes flowing but zero
// output — 2026-06-16 incident). Both are retryable; this is the per-model
// paper trail for how often each happens.
func logStreamStall(model, path string, budget time.Duration, bytesReceived int64, cause error) {
	stallKind := "byte_idle"
	if errors.Is(cause, httputil.ErrUpstreamOutputStall) {
		stallKind = "output_idle"
	}
	observability.Get().Error("OpenAI upstream stream stalled mid-response; aborting for retry",
		"model", model,
		"provider", providers.ProviderOpenAI,
		"path", path,
		"stall_kind", stallKind,
		"budget_ms", budget.Milliseconds(),
		"bytes_received", bytesReceived,
	)
}

func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

func (c *Client) Passthrough(ctx context.Context, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	// Codex subscriptions are served only via the routed Responses dispatch
	// (Proxy), never here — no Codex backend switch needed.
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
	if resp.StatusCode >= 400 {
		var snip [1024]byte
		n, _ := io.ReadFull(resp.Body, snip[:])
		_, snipWriteErr := w.Write(snip[:n])
		rest, copyErr := io.Copy(w, resp.Body)
		logUpstreamStatus(
			"Upstream OpenAI returned error status (passthrough)",
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

// readCapped reads up to limit bytes into a buffer, then drains the rest
// (up to maxDrain, discarded) to bound failover latency on a large error body.
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

// headerCapture is a minimal http.ResponseWriter used to reuse
// providers.CopyUpstreamHeaders against an http.Header we own.
type headerCapture struct{ h http.Header }

func (c headerCapture) Header() http.Header       { return c.h }
func (c headerCapture) Write([]byte) (int, error) { return 0, nil }
func (c headerCapture) WriteHeader(int)           {}

// logUpstreamStatus logs non-2xx responses at ERROR, except 429 (routine
// rate-limit signal handled via failover), logged at WARN.
func logUpstreamStatus(msg string, status int, attrs ...any) {
	merged := append([]any{"status", status}, attrs...)
	if status >= 500 || (status >= 400 && status != http.StatusTooManyRequests) {
		observability.Get().Error(msg, merged...)
		return
	}
	observability.Get().Warn(msg, merged...)
}

var _ providers.Client = (*Client)(nil)
