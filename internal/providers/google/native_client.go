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
	"workweave/router/internal/translate"
)

// NativeBaseURL is Google's native Generative Language base URL. Native
// endpoints sit under /v1beta/models/{model}:generateContent — distinct from
// the OpenAI-compat surface (which lives under /v1beta/openai). NativeClient
// composes the per-request URL.
const NativeBaseURL = "https://generativelanguage.googleapis.com"

// NativeClient is the providers.Client adapter for Google Gemini's native
// REST surface. The native API both returns and accepts the opaque
// thought_signature field that multi-turn tool use against Gemini 3.x preview
// models requires; the OpenAI-compat surface (see Client) does not.
//
// Auth is via the x-goog-api-key request header. BYOK credentials on the
// request context take precedence over the deployment-level key.
type NativeClient struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

// NewNativeClient is pooled for sustained traffic to a single host. baseURL
// defaults to NativeBaseURL when empty.
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

// Proxy posts the prepared Gemini-native body to :generateContent (or
// :streamGenerateContent?alt=sse when the inbound request was streaming) and
// streams the response back via the provided ResponseWriter.
//
// The streaming/non-streaming choice is communicated by the caller: the
// translate.GeminiToOpenAISSETranslator wrapping w expects Gemini SSE only
// when the upstream Content-Type advertises text/event-stream. Callers select
// streaming by setting "X-Stream" on prep.Headers; we strip that synthetic
// header before forwarding.
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
	return httputil.StreamBody(resp.Body, resp.StatusCode, w, t)
}

// Passthrough forwards an inbound non-routing request (e.g. /v1/models) to the
// native surface unchanged. The native API exposes /v1beta/models for model
// discovery; the inbound /v1/... prefix is rewritten to /v1beta/.
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
	_, err = io.Copy(w, resp.Body)
	return err
}

// applyAPIKey sets x-goog-api-key from BYOK credentials when present, falling
// back to the deployment-level key. Mirrors Client.Proxy's resolution order.
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
