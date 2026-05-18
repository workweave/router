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
	DeepInfraBaseURL = "https://api.deepinfra.com/v1/openai"
	// BedrockMantleBaseURLTemplate is the AWS Bedrock OpenAI-compatible
	// "bedrock-mantle" surface. The {region} placeholder is substituted at
	// boot from AWS_REGION via BedrockMantleBaseURL. AWS recommends this
	// endpoint over the model-native bedrock-runtime/InvokeModel surface
	// for OpenAI-compat upstreams; auth is a static bearer token (the
	// long-term Bedrock API key) rather than SigV4.
	BedrockMantleBaseURLTemplate = "https://bedrock-mantle.{region}.api.aws/v1"
)

// BedrockMantleBaseURL returns the bedrock-mantle base URL for the given AWS
// region (e.g. "us-east-1"). Empty region falls back to us-east-1.
func BedrockMantleBaseURL(region string) string {
	if region == "" {
		region = "us-east-1"
	}
	return strings.Replace(BedrockMantleBaseURLTemplate, "{region}", region, 1)
}

// Client is the generic OpenAI Chat Completions adapter. The optional
// modelIDMap rewrites the request body's top-level "model" field before
// forwarding — needed for upstreams (DeepInfra HF-form IDs, Bedrock dot-form
// IDs) whose canonical model strings differ from the OpenRouter-form slugs
// the router uses as public IDs.
type Client struct {
	apiKey      string
	baseURL     string
	http        *http.Client
	modelIDMap  map[string]string // empty = pass `decision.Model` through unchanged
}

// NewClient constructs an OpenAI-compatible client that forwards the
// `decision.Model` value as-is to the upstream.
func NewClient(apiKey, baseURL string) *Client {
	return NewClientWithModelIDMap(apiKey, baseURL, nil)
}

// NewClientWithModelIDMap is like NewClient but rewrites the request body's
// top-level "model" field according to modelIDMap before forwarding. Keys are
// router-internal model IDs (the slash-form public IDs); values are the
// upstream-format IDs the provider expects. Models not in the map pass through
// unchanged. Pass nil for no rewriting.
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
	body := prep.Body
	if upstreamModel, ok := c.modelIDMap[decision.Model]; ok {
		rewritten, err := rewriteModelField(body, upstreamModel)
		if err != nil {
			return fmt.Errorf("rewrite model field for %q → %q: %w", decision.Model, upstreamModel, err)
		}
		body = rewritten
	}
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
			"Upstream OpenAI-compatible provider returned error status",
			resp.StatusCode,
			"base_url", c.baseURL,
			"routed_model", decision.Model,
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

// rewriteModelField replaces the top-level "model" field of an OpenAI Chat
// Completions request body with newModel, preserving every other field
// verbatim. Used by providers (DeepInfra HF-form IDs, Bedrock dot-form IDs)
// whose canonical model strings differ from the router's public slash-form
// slugs.
func rewriteModelField(body []byte, newModel string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse request body: %w", err)
	}
	encoded, err := json.Marshal(newModel)
	if err != nil {
		return nil, fmt.Errorf("encode upstream model id: %w", err)
	}
	m["model"] = encoded
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("re-encode request body: %w", err)
	}
	return out, nil
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
