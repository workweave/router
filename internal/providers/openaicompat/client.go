// Package openaicompat is a generic providers.Client adapter for upstreams that speak OpenAI Chat Completions.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
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
)

const (
	DefaultBaseURL   = "https://openrouter.ai/api/v1"
	FireworksBaseURL = "https://api.fireworks.ai/inference/v1"
	// DeepInfraBaseURL is DeepInfra's OpenAI-compatible surface. DeepInfra uses
	// HuggingFace-form model IDs; pair with NewClientWithModelIDMap to rewrite
	// the router's public slash-form slugs on the wire.
	DeepInfraBaseURL = "https://api.deepinfra.com/v1/openai"
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

// rewriteModelField rewrites the body's top-level "model" field according to
// modelIDMap. Returns the input unchanged when the map is empty, the body
// isn't a JSON object, or the model isn't mapped.
func rewriteModelField(body []byte, modelIDMap map[string]string) []byte {
	if len(modelIDMap) == 0 || len(body) == 0 {
		return body
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}
	modelRaw, ok := raw["model"]
	if !ok {
		return body
	}
	var model string
	if err := json.Unmarshal(modelRaw, &model); err != nil {
		return body
	}
	upstream, ok := modelIDMap[model]
	if !ok {
		return body
	}
	newModel, err := json.Marshal(upstream)
	if err != nil {
		return body
	}
	raw["model"] = newModel
	out, err := json.Marshal(raw)
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
	return httputil.StreamBody(resp.Body, resp.StatusCode, w, t)
}

// readCapped reads up to limit bytes from r into a buffer, then drains the
// rest without retention. Returns the buffered prefix, total bytes read,
// and any read error (io.EOF mapped to nil).
func readCapped(r io.Reader, limit int) ([]byte, int64, error) {
	prefix, err := io.ReadAll(io.LimitReader(r, int64(limit)))
	totalRead := int64(len(prefix))
	if err != nil {
		return prefix, totalRead, err
	}
	rest, drainErr := io.Copy(io.Discard, r)
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
func (c headerCapture) WriteHeader(int)            {}

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
