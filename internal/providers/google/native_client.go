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
	"workweave/router/internal/observability/otel"
	"workweave/router/internal/providers"
	"workweave/router/internal/providers/httputil"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
	"workweave/router/internal/translate"
)

// NativeBaseURL is Google's native Generative Language base URL. Endpoints sit
// under /v1beta/models/{model}:generateContent — distinct from the OpenAI-compat
// surface under /v1beta/openai.
const NativeBaseURL = "https://generativelanguage.googleapis.com"

// NativeClient is the providers.Client adapter for Gemini's native REST surface.
// The native API returns and accepts the opaque thought_signature field that
// multi-turn tool use against Gemini 3.x preview models requires; the
// OpenAI-compat surface does not. Auth is via x-goog-api-key.
type NativeClient struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

// NewNativeClient returns a NativeClient; baseURL defaults to NativeBaseURL when empty.
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

// Proxy posts the prepared body to :generateContent, or
// :streamGenerateContent?alt=sse when prep.Headers[GeminiStreamHintHeader] is
// "true". The synthetic hint header is stripped before forwarding upstream.
func (c *NativeClient) Proxy(ctx context.Context, decision router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
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

	return httputil.StreamBody(resp.Body, resp.StatusCode, w, t)
}

// Passthrough forwards to the native surface, rewriting the inbound /v1/
// prefix to /v1beta/ since the native API exposes /v1beta/models for discovery.
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

// logUpstreamStatus emits Error for 4xx/5xx and Warn for 429s, surfacing a
// body preview so non-2xx upstream responses don't blackhole into a generic
// "upstream call failed" string at the client.
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
