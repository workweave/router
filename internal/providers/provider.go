// Package providers defines the upstream LLM client interface, sentinel errors, and shared wire helpers.
package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"workweave/router/internal/router"
)

const (
	ProviderAnthropic  = "anthropic"
	ProviderOpenAI     = "openai"
	ProviderGoogle     = "google"
	ProviderOpenRouter = "openrouter"
	ProviderFireworks  = "fireworks"
	ProviderDeepInfra  = "deepinfra"
	ProviderBedrock    = "bedrock"
)

// APIKeyEnvVars maps provider name to the env var providing its deployment-level upstream API key.
// Bedrock uses AWS-issued long-term Bedrock API keys (static bearer tokens), not SigV4 access keys.
var APIKeyEnvVars = map[string]string{
	ProviderAnthropic:  "ANTHROPIC_API_KEY",
	ProviderOpenAI:     "OPENAI_API_KEY",
	ProviderGoogle:     "GOOGLE_API_KEY",
	ProviderOpenRouter: "OPENROUTER_API_KEY",
	ProviderFireworks:  "FIREWORKS_API_KEY",
	ProviderDeepInfra:  "DEEPINFRA_API_KEY",
	ProviderBedrock:    "AWS_BEARER_TOKEN_BEDROCK",
}

// APIKeyEnvVar returns the env-var name for the given provider, or empty
// when the provider is unknown.
func APIKeyEnvVar(provider string) string {
	return APIKeyEnvVars[provider]
}

// CacheTTL is the best-effort upstream prompt-cache lifetime per provider —
// roughly how long a cached prefix survives between turns of the same session.
// Anthropic sells a 1h extended cache, which is what pinSessionTTL is sized to;
// the OpenAI-compatible OSS providers cache on a best-effort, minutes-scale
// window with no documented TTL guarantee, so a pin can long outlive the cache
// it was meant to keep warm. The planner reads this to stop crediting a stale
// pin a cache-read discount it no longer earns.
var CacheTTL = map[string]time.Duration{
	ProviderAnthropic:  time.Hour,
	ProviderOpenAI:     5 * time.Minute,
	ProviderGoogle:     5 * time.Minute,
	ProviderOpenRouter: 5 * time.Minute,
	ProviderFireworks:  5 * time.Minute,
	ProviderDeepInfra:  5 * time.Minute,
	ProviderBedrock:    5 * time.Minute,
}

// DefaultCacheTTL is the conservative fallback cache lifetime for providers
// absent from CacheTTL.
const DefaultCacheTTL = 5 * time.Minute

// CacheTTLFor returns the best-effort prompt-cache lifetime for a provider,
// falling back to DefaultCacheTTL for unknown providers.
func CacheTTLFor(provider string) time.Duration {
	if ttl, ok := CacheTTL[provider]; ok {
		return ttl
	}
	return DefaultCacheTTL
}

// HopByHopHeaders are stripped from upstream responses per RFC 7230 §6.1.
var HopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

// CopyUpstreamHeaders copies non-hop-by-hop headers from resp into w.
func CopyUpstreamHeaders(w http.ResponseWriter, resp *http.Response) {
	dynamicHop := make(map[string]struct{})
	for _, v := range resp.Header.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				dynamicHop[http.CanonicalHeaderKey(name)] = struct{}{}
			}
		}
	}
	for k, vs := range resp.Header {
		canon := http.CanonicalHeaderKey(k)
		if _, hop := HopByHopHeaders[canon]; hop {
			continue
		}
		if _, hop := dynamicHop[canon]; hop {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
}

// ErrNotImplemented is returned by adapters that don't implement a method.
// The HTTP layer maps it to 501.
var ErrNotImplemented = errors.New("provider: not implemented")

// UpstreamStatusError is returned by Client.Proxy and Client.Passthrough after
// the upstream non-2xx response body has already been written to the client.
// Handlers detecting c.Writer.Written() must NOT write their own JSON envelope.
type UpstreamStatusError struct {
	Status int
}

func (e *UpstreamStatusError) Error() string {
	return fmt.Sprintf("upstream returned status %d", e.Status)
}

// UpstreamErrorResponse is returned by adapters that buffer the upstream
// non-2xx response instead of streaming it through. The proxy decides
// whether to retry on a different provider or flush the buffered response
// to the client. Body is capped at MaxBufferedErrorBytes.
type UpstreamErrorResponse struct {
	Status  int
	Headers http.Header
	Body    []byte
}

func (e *UpstreamErrorResponse) Error() string {
	return fmt.Sprintf("upstream returned status %d (buffered)", e.Status)
}

// MaxBufferedErrorBytes caps the upstream error body buffered by adapters
// that support failover. Beyond this the body is truncated and the rest
// of the upstream stream is drained without retention.
const MaxBufferedErrorBytes = 64 * 1024

// IsRetryableStatus reports whether an upstream HTTP status is worth
// retrying on a different provider. Covers transient upstream-side faults
// (5xx + 408 timeout + 429 rate-limit). 4xx ≠ 408/429 are the client's
// fault and won't be fixed by a different upstream.
func IsRetryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, // 408
		http.StatusTooManyRequests: // 429
		return true
	}
	return status >= 500 && status <= 599
}

// IsRetryable reports whether err represents an upstream failure that is
// safe to retry on a different provider — that is, no response bytes have
// been written to the client. True for transport-level errors from a
// provider adapter and for *UpstreamErrorResponse with a retryable status.
// False for *UpstreamStatusError (bytes already flushed) and nil.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	// Client-side cancellation and per-request deadlines are owned by the
	// caller, not the upstream. Retrying on a different binding would
	// either fire after the client is gone or use a budget that has
	// already elapsed; either way it's wasted upstream load.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var buffered *UpstreamErrorResponse
	if errors.As(err, &buffered) {
		return IsRetryableStatus(buffered.Status)
	}
	var flushed *UpstreamStatusError
	if errors.As(err, &flushed) {
		return false
	}
	// Anything else (transport error, build error) is treated as retryable;
	// the per-attempt guard in proxy.dispatchWithFallback confirms no bytes
	// were written before letting the retry happen.
	return true
}

// PreparedRequest holds the encoded target-format request body and format-specific header overrides.
type PreparedRequest struct {
	Body    []byte
	Headers http.Header
	// Stats records translation-time mutations applied to the body, for
	// observability. Zero-value when no mutation fired; populated by the
	// translate package as a side effect of Prepare*. The proxy reads these
	// after dispatch and folds them into the ProxyMessages-complete log so
	// per-PR mitigation impact can be measured in production traffic.
	Stats RequestMutationStats
}

// RequestMutationStats reports translation-time mitigations the router
// applied to the upstream request body. Surfaced in the ProxyMessages-
// complete log with keys:
//   - cc_only_tools_stripped
//   - gemini_reminder_injected
type RequestMutationStats struct {
	// CCOnlyToolsStripped is the count of Claude-Code-only tools removed
	// from the request before dispatching to a non-Anthropic upstream. See
	// translate/claudecode_tool_filter.go (router PR #277).
	CCOnlyToolsStripped int
	// GeminiReminderInjected is true when the Gemini 3.x tool-use reminder
	// (geminiToolUseReminder) was appended to systemInstruction for this
	// request. See translate/system_reminder.go (router PR #276).
	GeminiReminderInjected bool
}

type Client interface {
	// Proxy forwards prep.Body verbatim to the upstream and streams the response into w.
	Proxy(ctx context.Context, decision router.Decision, prep PreparedRequest, w http.ResponseWriter, r *http.Request) error

	// Passthrough forwards an inbound request to the same path on the upstream with no model rewriting.
	Passthrough(ctx context.Context, prep PreparedRequest, w http.ResponseWriter, r *http.Request) error
}
