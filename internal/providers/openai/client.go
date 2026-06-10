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

// responseHeaderTimeout guards time-to-first-byte for OpenAI upstreams. It is
// raised above the 30s default because the Responses API (gpt-5.x reasoning)
// can take well over 30s to emit its first streamed event under high effort;
// the default would false-trip "http2: timeout awaiting response headers" on a
// healthy model. Once the stream is flowing, inter-event gaps are bounded by
// StreamBody's idle watchdog, so this generous header budget cannot reintroduce
// an unbounded hang. Applies to both /v1/chat/completions and /v1/responses.
const responseHeaderTimeout = 120 * time.Second

type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client
	// sseIdleTimeout, when > 0, overrides the per-endpoint idle-progress
	// threshold (httputil.DefaultSSEIdleTimeout for chat/completions,
	// httputil.DefaultResponsesSSEIdleTimeout for /v1/responses). Production
	// always uses the defaults via NewClient; tests inject a small value to
	// exercise the mid-stream stall watchdog without waiting out the real
	// threshold (mirrors the header-timeout injection below).
	sseIdleTimeout time.Duration
}

func NewClient(apiKey, baseURL string) *Client {
	return NewClientWithResponseHeaderTimeout(apiKey, baseURL, responseHeaderTimeout)
}

// NewClientWithResponseHeaderTimeout is NewClient with a caller-chosen
// time-to-first-byte guard. Production uses the 120s default via NewClient;
// the override exists so a test can inject a small timeout and exercise the
// bounded-stall behavior (a stalled upstream surfaces an error, not a hang —
// the #331 belt-and-suspenders) without waiting the full default.
func NewClientWithResponseHeaderTimeout(apiKey, baseURL string, headerTimeout time.Duration) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		apiKey:  apiKey,
		baseURL: baseURL,
		http:    &http.Client{Transport: httputil.NewTransportWithResponseHeaderTimeout(5*time.Second, 5*time.Second, headerTimeout)},
	}
}

// NewClientWithTimeouts is NewClientWithResponseHeaderTimeout with an
// additional injected SSE idle-progress threshold; see Client.sseIdleTimeout.
func NewClientWithTimeouts(apiKey, baseURL string, headerTimeout, sseIdleTimeout time.Duration) *Client {
	c := NewClientWithResponseHeaderTimeout(apiKey, baseURL, headerTimeout)
	c.sseIdleTimeout = sseIdleTimeout
	return c
}

// idleTimeoutFor picks the idle-progress watchdog threshold for the upstream
// endpoint. /v1/responses gets the more generous reasoning budget — see
// httputil.DefaultResponsesSSEIdleTimeout.
func (c *Client) idleTimeoutFor(endpoint providers.Endpoint) time.Duration {
	if c.sseIdleTimeout > 0 {
		return c.sseIdleTimeout
	}
	if endpoint == providers.EndpointResponses {
		return httputil.DefaultResponsesSSEIdleTimeout
	}
	return httputil.DefaultSSEIdleTimeout
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
	// Match the bearer prefix case-insensitively to mirror the router auth
	// middleware's extractBearer; otherwise `authorization: bearer rk_...`
	// (lowercased by some clients) bypasses this guard and the router key
	// crosses the trust boundary to OpenAI.
	const bearerPrefix = "Bearer "
	if len(v) > len(bearerPrefix) && strings.EqualFold(v[:len(bearerPrefix)], bearerPrefix) {
		if auth.HasAPIKeyPrefix(strings.TrimSpace(v[len(bearerPrefix):])) {
			return
		}
	}
	upstream.Header.Set("Authorization", v)
}

func (c *Client) Proxy(ctx context.Context, decision router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	path := "/v1/chat/completions"
	if prep.Endpoint == providers.EndpointResponses {
		path = "/v1/responses"
	}
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(prep.Body))
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

	idleTimeout := c.idleTimeoutFor(prep.Endpoint)
	log := observability.Get()
	log.Debug("OpenAI upstream response",
		"status", resp.StatusCode,
		"content_type", resp.Header.Get("Content-Type"),
		"transfer_encoding", resp.Header.Get("Transfer-Encoding"),
		"content_encoding", resp.Header.Get("Content-Encoding"),
		"request_id", resp.Header.Get("X-Request-Id"),
	)

	// Buffer non-2xx and surface as UpstreamErrorResponse so the dispatch
	// loop can fail over (multi-binding models) or render the upstream error
	// envelope in the inbound format (single-binding models like gpt-*).
	// Writing the upstream status straight through corrupts the SSE stream
	// when a translator's Prelude already committed `200 + message_start`
	// to the prelude buffer, and silently drops the upstream error body —
	// neither debugging signal nor failover survive.
	if resp.StatusCode >= 400 {
		// Guard the error-body read with the same idle watchdog as the
		// streaming path below: readCapped's blocking reads would otherwise
		// hang indefinitely on an upstream that returns error headers and
		// then stalls the body — the response-header timeout no longer
		// applies once headers have arrived.
		mark, stop := httputil.StartIdleWatchdog(ctx, cancel, idleTimeout)
		body := &progressReader{r: resp.Body, mark: mark}
		bufBody, totalRead, drainErr := readCapped(body, providers.MaxBufferedErrorBytes)
		stop()
		if errors.Is(context.Cause(ctx), httputil.ErrUpstreamIdleTimeout) {
			logStreamStall(decision.Model, path, idleTimeout, totalRead)
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

	// Manual stream loop for per-chunk diagnostics; non-debug takes the fast path.
	if !log.Enabled(ctx, slog.LevelDebug) {
		body := &progressReader{r: resp.Body}
		streamErr := httputil.StreamBody(ctx, cancel, idleTimeout, body, status, w, t)
		if errors.Is(streamErr, httputil.ErrUpstreamIdleTimeout) {
			logStreamStall(decision.Model, path, idleTimeout, body.n)
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
			if cause := context.Cause(ctx); errors.Is(cause, httputil.ErrUpstreamIdleTimeout) {
				logStreamStall(decision.Model, path, idleTimeout, int64(bytesRead))
				return httputil.ErrUpstreamIdleTimeout
			}
			log.Debug("OpenAI upstream read failed", "err", readErr, "bytes_read", bytesRead)
			return readErr
		}
	}
}

// progressReader counts upstream bytes and reports each successful read as
// watchdog progress. The byte count feeds the stall log's bytes_received
// field; mark (optional, nil-safe) feeds StartIdleWatchdog on paths that do
// not go through StreamBody's built-in marking. Single-goroutine use only —
// the count is read by the same goroutine after the stream loop returns.
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

// logStreamStall reports an SSE idle-watchdog trip at ERROR: the upstream
// accepted the request and returned headers, then produced zero bytes for the
// full idle budget (prod incident 2026-06-09: two /v1/responses streams sat
// at zero output tokens until the 600s request cap, burning 10 minutes of the
// customer's agent budget each). The returned ErrUpstreamIdleTimeout is
// classified retryable, so dispatchWithFallback re-attempts when nothing has
// been committed to the client; this log is the paper trail for how often
// that happens per model.
func logStreamStall(model, path string, idleTimeout time.Duration, bytesReceived int64) {
	observability.Get().Error("OpenAI upstream stream stalled mid-response; aborting for retry",
		"model", model,
		"provider", providers.ProviderOpenAI,
		"path", path,
		"idle_ms", idleTimeout.Milliseconds(),
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

// readCapped reads up to limit bytes from r into a buffer, then drains the
// rest without retention up to maxDrain to bound failover latency on a slow
// upstream returning a large error body. The connection is closed by the
// caller's defer regardless, so the unread tail is discarded by Close.
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

// headerCapture is a minimal http.ResponseWriter that captures headers only,
// used to reuse providers.CopyUpstreamHeaders against an http.Header we own.
// Write/WriteHeader are no-ops.
type headerCapture struct{ h http.Header }

func (c headerCapture) Header() http.Header       { return c.h }
func (c headerCapture) Write([]byte) (int, error) { return 0, nil }
func (c headerCapture) WriteHeader(int)           {}

// logUpstreamStatus logs non-2xx upstream responses with a body preview.
// Severity is ERROR for >=500 and >=400 except 429 (which is a routine
// rate-limit signal that callers handle via failover).
func logUpstreamStatus(msg string, status int, attrs ...any) {
	merged := append([]any{"status", status}, attrs...)
	if status >= 500 || (status >= 400 && status != http.StatusTooManyRequests) {
		observability.Get().Error(msg, merged...)
		return
	}
	observability.Get().Warn(msg, merged...)
}

var _ providers.Client = (*Client)(nil)
