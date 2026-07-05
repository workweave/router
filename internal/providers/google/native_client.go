package google

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"workweave/router/internal/observability"
	"workweave/router/internal/providers"
	"workweave/router/internal/providers/httputil"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
	"workweave/router/internal/timing"
	"workweave/router/internal/translate"
)

// NativeBaseURL is Google's native Generative Language base URL.
const NativeBaseURL = "https://generativelanguage.googleapis.com"

// NativeClient is the providers.Client adapter for Gemini's native REST surface.
// The native API preserves thought_signature for multi-turn tool use with Gemini 3.x preview models.
type NativeClient struct {
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

// NewNativeClient returns a NativeClient using NativeBaseURL when baseURL is empty.
func NewNativeClient(apiKey, baseURL string) *NativeClient {
	if baseURL == "" {
		baseURL = NativeBaseURL
	}
	return &NativeClient{
		apiKey:  apiKey,
		baseURL: baseURL,
		http:    &http.Client{Transport: httputil.NewTransport(5*time.Second, 5*time.Second)},
	}
}

// NewNativeClientWithStallTimeouts is NewNativeClient with injected byte-idle
// and output-stall watchdog budgets. Exists so a test can drive the
// output-progress watchdog with a small budget while keeping the byte-idle
// watchdog large enough that it isn't what fires.
func NewNativeClientWithStallTimeouts(apiKey, baseURL string, sseIdleTimeout, outputStall time.Duration) *NativeClient {
	c := NewNativeClient(apiKey, baseURL)
	c.sseIdleTimeout = sseIdleTimeout
	c.outputStall = outputStall
	return c
}

// idleTimeout returns the byte-idle watchdog budget: the injected test override
// when set, else httputil.DefaultSSEIdleTimeout.
func (c *NativeClient) idleTimeout() time.Duration {
	if c.sseIdleTimeout > 0 {
		return c.sseIdleTimeout
	}
	return httputil.DefaultSSEIdleTimeout
}

// outputStallTimeout returns the output-progress watchdog budget: the injected
// test override when set, else httputil.DefaultOutputStallTimeout.
func (c *NativeClient) outputStallTimeout() time.Duration {
	if c.outputStall > 0 {
		return c.outputStall
	}
	return httputil.DefaultOutputStallTimeout
}

// Proxy posts to :generateContent or :streamGenerateContent?alt=sse depending on the Gemini stream hint header.
func (c *NativeClient) Proxy(ctx context.Context, decision router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	stream := prep.Headers.Get(translate.GeminiStreamHintHeader) == "true"
	prep.Headers.Del(translate.GeminiStreamHintHeader)

	method := ":generateContent"
	query := ""
	if stream {
		method = ":streamGenerateContent"
		query = "?alt=sse"
	}
	url := c.baseURL + "/v1beta/models/" + decision.Model + method + query

	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(prep.Body))
	if err != nil {
		return fmt.Errorf("build upstream request: %w", err)
	}
	upstream.Header.Set("Content-Type", "application/json")
	c.applyAPIKey(ctx, upstream)
	for k, vs := range prep.Headers {
		upstream.Header[http.CanonicalHeaderKey(k)] = vs
	}
	if stream {
		upstream.Header.Set("Accept", "text/event-stream")
	}

	t := timing.TimingFrom(ctx)
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
			"Upstream Google returned error status",
			resp.StatusCode,
			"routed_model", decision.Model,
			"streaming", stream,
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

	// Output-progress watchdog: StreamBody's byte-idle watchdog resets on ANY
	// upstream byte, so a stream that stays byte-alive (SSE keepalive comments or
	// empty/role-only frames) while producing zero output rides to the request
	// cap. This second watchdog measures time-since-last-OUTPUT; its mark is fed
	// by the GeminiToOpenAISSETranslator only on output-bearing events (text,
	// tool-call args, terminal finish). On trip it cancels ctx with
	// ErrUpstreamOutputStall (retryable; fails over while uncommitted). Only the
	// translator can tell output from keepalive, so it is wired via
	// ArmOutputProgress; a non-streaming writer returns armed=false and is
	// byte-idle-guarded only.
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

// Passthrough rewrites inbound /v1/ paths to /v1beta/ for the native API surface.
func (c *NativeClient) Passthrough(ctx context.Context, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	suffix := r.URL.Path
	if rest, ok := strings.CutPrefix(suffix, "/v1/"); ok {
		suffix = "/v1beta/" + rest
	} else if !strings.HasPrefix(suffix, "/v1beta") {
		suffix = "/v1beta" + suffix
	}
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
	c.applyAPIKey(ctx, upstream)
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
			"Upstream Google returned error status (passthrough)",
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

// logUpstreamStatus logs non-2xx upstream responses with a body preview.
func logUpstreamStatus(msg string, status int, attrs ...any) {
	merged := append([]any{"status", status}, attrs...)
	if status >= 500 || (status >= 400 && status != http.StatusTooManyRequests) {
		observability.Get().Error(msg, merged...)
		return
	}
	observability.Get().Warn(msg, merged...)
}

// applyAPIKey sets x-goog-api-key, preferring BYOK credentials over the deployment-level key.
func (c *NativeClient) applyAPIKey(ctx context.Context, req *http.Request) {
	if creds := proxy.CredentialsFromContext(ctx); creds != nil && len(creds.APIKey) > 0 {
		req.Header.Set("x-goog-api-key", string(creds.APIKey))
		return
	}
	if c.apiKey != "" {
		req.Header.Set("x-goog-api-key", c.apiKey)
	}
}

var _ providers.Client = (*NativeClient)(nil)
