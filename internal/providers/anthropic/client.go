// Package anthropic is the providers.Client adapter for Anthropic's Messages API.
package anthropic

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"workweave/router/internal/observability"
	"workweave/router/internal/observability/otel"
	"workweave/router/internal/providers"
	"workweave/router/internal/providers/httputil"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
)

const DefaultBaseURL = "https://api.anthropic.com"

// UsageRecorder consumes the rate-limit headers Anthropic returns on
// every Messages response. It is declared as a consumer-side interface
// (rather than importing internal/proxy/usage) so the providers layer
// keeps its inner-ring purity: the concrete observer lives outside this
// package and is injected from the composition root.
type UsageRecorder interface {
	Record(key string, h http.Header)
}

type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client
	// usage is optional; nil means rate-limit headers are forwarded to
	// the client but not recorded for the bypass gate.
	usage UsageRecorder
	// credKeyer turns the upstream-bound credential into the opaque key
	// the recorder uses. Injected so the providers package doesn't
	// import internal/proxy/usage.
	credKeyer func(apiKey []byte) string
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

// WithUsageRecorder attaches a recorder + credential-key function so
// every successful upstream response feeds the bypass observer.
// Returns the receiver for fluent wiring from main.go.
func (c *Client) WithUsageRecorder(rec UsageRecorder, credKeyer func([]byte) string) *Client {
	c.usage = rec
	c.credKeyer = credKeyer
	return c
}

// resolveCredentialKey mirrors setAuth's precedence so the key recorded
// against an observation matches the credential actually sent upstream.
// Returns "" when no credential is resolvable or no recorder is wired.
func (c *Client) resolveCredentialKey(ctx context.Context, inbound *http.Request) string {
	if c.usage == nil || c.credKeyer == nil {
		return ""
	}
	if creds := proxy.CredentialsFromContext(ctx); creds != nil {
		return c.credKeyer(creds.APIKey)
	}
	if c.apiKey != "" {
		return c.credKeyer([]byte(c.apiKey))
	}
	if v := inbound.Header.Get("x-api-key"); v != "" {
		return c.credKeyer([]byte(v))
	}
	if v := inbound.Header.Get("authorization"); v != "" {
		return c.credKeyer([]byte(v))
	}
	return ""
}

// setAuth applies authentication to the upstream request. Precedence:
// (1) per-request BYOK credentials in ctx; (2) deployment-level API key;
// (3) passthrough of the client's own Anthropic auth headers (OAuth bearer or
// x-api-key). Router-only credentials are deliberately not forwarded.
func (c *Client) setAuth(ctx context.Context, upstream *http.Request, inbound *http.Request) {
	if creds := proxy.CredentialsFromContext(ctx); creds != nil {
		upstream.Header.Set("x-api-key", string(creds.APIKey))
		return
	}
	if c.apiKey != "" {
		upstream.Header.Set("x-api-key", c.apiKey)
		return
	}
	if v := inbound.Header.Get("authorization"); v != "" {
		upstream.Header.Set("authorization", v)
	}
	if v := inbound.Header.Get("x-api-key"); v != "" {
		upstream.Header.Set("x-api-key", v)
	}
}

func (c *Client) Proxy(ctx context.Context, decision router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(prep.Body))
	if err != nil {
		return fmt.Errorf("build upstream request: %w", err)
	}
	upstream.Header.Set("content-type", "application/json")
	c.setAuth(ctx, upstream, r)
	for k, vs := range prep.Headers {
		upstream.Header[http.CanonicalHeaderKey(k)] = vs
	}
	if v := r.Header.Get("accept"); v != "" {
		upstream.Header.Set("accept", v)
	}

	t := otel.TimingFrom(ctx)
	t.StampUpstreamRequest()
	resp, err := c.http.Do(upstream)
	if err != nil {
		return fmt.Errorf("upstream call: %w", err)
	}
	defer resp.Body.Close()
	t.StampUpstreamHeaders()
	c.recordUsage(ctx, r, resp)

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
			"Upstream Anthropic returned error status",
			resp.StatusCode,
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
	if v := r.Header.Get("accept"); v != "" {
		upstream.Header.Set("accept", v)
	}

	resp, err := c.http.Do(upstream)
	if err != nil {
		return fmt.Errorf("upstream passthrough call: %w", err)
	}
	defer resp.Body.Close()
	c.recordUsage(ctx, r, resp)

	providers.CopyUpstreamHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	if resp.StatusCode >= 400 {
		var snip [1024]byte
		n, _ := io.ReadFull(resp.Body, snip[:])
		_, snipWriteErr := w.Write(snip[:n])
		rest, copyErr := io.Copy(w, resp.Body)
		logUpstreamStatus(
			"Upstream Anthropic returned error status (passthrough)",
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

// recordUsage observes the unified rate-limit headers off resp and
// forwards them to the recorder. No-op when no recorder is wired or
// no credential can be resolved (in which case the bypass gate's
// cold-start path applies to the next request).
func (c *Client) recordUsage(ctx context.Context, inbound *http.Request, resp *http.Response) {
	if c.usage == nil {
		return
	}
	key := c.resolveCredentialKey(ctx, inbound)
	if key == "" {
		return
	}
	c.usage.Record(key, resp.Header)
}

func logUpstreamStatus(msg string, status int, attrs ...any) {
	merged := append([]any{"status", status}, attrs...)
	if status >= 500 || (status >= 400 && status != http.StatusTooManyRequests) {
		observability.Get().Error(msg, merged...)
		return
	}
	observability.Get().Warn(msg, merged...)
}

var _ providers.Client = (*Client)(nil)
