// Package providers defines the upstream LLM client interface, sentinel errors, and shared wire helpers.
package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"workweave/router/internal/router"
)

// UpstreamHeaderObserver records subscription rate-limit headroom (see
// internal/proxy/usage) without coupling adapters to the observer. Ctx lets it
// check the resolved credential so only responses on the caller's own
// subscription are recorded (not e.g. a handover summarizer's deployment-key
// call). Invoked right after the upstream responds; must be cheap, non-blocking.
type UpstreamHeaderObserver func(context.Context, http.Header)

type upstreamHeaderObserverKey struct{}

// WithUpstreamHeaderObserver returns ctx carrying obs; a nil obs leaves ctx unchanged.
func WithUpstreamHeaderObserver(ctx context.Context, obs UpstreamHeaderObserver) context.Context {
	if obs == nil {
		return ctx
	}
	return context.WithValue(ctx, upstreamHeaderObserverKey{}, obs)
}

// ObserveUpstreamHeaders invokes the context's UpstreamHeaderObserver with ctx
// and h, if one is set. Provider adapters call this after receiving an upstream
// response.
func ObserveUpstreamHeaders(ctx context.Context, h http.Header) {
	if obs, ok := ctx.Value(upstreamHeaderObserverKey{}).(UpstreamHeaderObserver); ok && obs != nil {
		obs(ctx, h)
	}
}

// Adding a provider is a THREE-map edit: the Provider* constant here, its
// APIKeyEnvVars entry, and its ProviderFamilies entry. Omitting the family
// entry makes dispatch fall through to ErrProviderNotConfigured — a silent 502
// even though the provider looked "enabled" at boot. ValidateDispatchable and
// the table test catch this at boot instead of in production.
const (
	ProviderAnthropic  = "anthropic"
	ProviderOpenAI     = "openai"
	ProviderGoogle     = "google"
	ProviderOpenRouter = "openrouter"
	ProviderFireworks  = "fireworks"
	ProviderDeepInfra  = "deepinfra"
	ProviderBedrock    = "bedrock"
	ProviderMakora     = "makora"
	ProviderTogether   = "together"
)

// TranslationFamily is the wire-format family a provider speaks; the proxy
// dispatches cross-format translation off this instead of enumerated
// provider-name lists, so a new OpenAI-compatible provider routes correctly
// as soon as it gets a ProviderFamilies entry.
type TranslationFamily int

const (
	// FamilyUnknown is the zero value (no ProviderFamilies entry).
	// ValidateDispatchable panics at boot if a registered provider maps to it.
	FamilyUnknown TranslationFamily = iota
	// FamilyAnthropic speaks the Anthropic Messages wire format natively.
	FamilyAnthropic
	// FamilyOpenAICompat speaks the OpenAI Chat Completions wire format
	// (OpenAI itself plus every OpenAI-compatible upstream: OpenRouter,
	// Fireworks, DeepInfra, Bedrock's OpenAI-compat surface, Makora, Together).
	FamilyOpenAICompat
	// FamilyGemini speaks the Google Generative Language (Gemini) wire format.
	FamilyGemini
)

// ProviderFamilies is the single source of truth for cross-format dispatch;
// keep it covering EVERY Provider* constant (see the three-map note above).
var ProviderFamilies = map[string]TranslationFamily{
	ProviderAnthropic:  FamilyAnthropic,
	ProviderOpenAI:     FamilyOpenAICompat,
	ProviderGoogle:     FamilyGemini,
	ProviderOpenRouter: FamilyOpenAICompat,
	ProviderFireworks:  FamilyOpenAICompat,
	ProviderDeepInfra:  FamilyOpenAICompat,
	ProviderBedrock:    FamilyOpenAICompat,
	ProviderMakora:     FamilyOpenAICompat,
	ProviderTogether:   FamilyOpenAICompat,
}

// FamilyFor returns the translation family for a provider, or FamilyUnknown
// when the provider has no ProviderFamilies entry.
func FamilyFor(provider string) TranslationFamily {
	return ProviderFamilies[provider]
}

// IsOpenAICompat reports whether the provider speaks the OpenAI Chat
// Completions wire format.
func IsOpenAICompat(provider string) bool {
	return FamilyFor(provider) == FamilyOpenAICompat
}

// AllProviders returns every known Provider* constant (every ProviderFamilies
// key), sorted for deterministic iteration and display order.
func AllProviders() []string {
	out := make([]string, 0, len(ProviderFamilies))
	for p := range ProviderFamilies {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ValidateDispatchable reports an error if any registered provider is missing
// from ProviderFamilies (would silently 502 at request time). Called at boot;
// the composition root panics on error so this fails loudly, not in prod.
func ValidateDispatchable(registered []string) error {
	missing := make([]string, 0)
	for _, p := range registered {
		if FamilyFor(p) == FamilyUnknown {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("providers missing a ProviderFamilies entry (add them to internal/providers/provider.go): %s", strings.Join(missing, ", "))
}

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
	ProviderMakora:     "MAKORA_API_KEY",
	ProviderTogether:   "TOGETHER_API_KEY",
}

// APIKeyEnvVar returns the env-var name for the given provider, or empty
// when the provider is unknown.
func APIKeyEnvVar(provider string) string {
	return APIKeyEnvVars[provider]
}

// CacheTTL is the best-effort upstream prompt-cache lifetime per provider.
// Anthropic's 1h extended cache is what pinSessionTTL is sized to; OSS
// OpenAI-compatible providers cache on an undocumented minutes-scale window,
// so a pin can outlive the cache — the planner uses this to stop crediting a
// stale pin a cache-read discount it no longer earns.
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

// UpstreamStatusError is returned after a non-2xx upstream body has already
// been written to the client. Handlers seeing c.Writer.Written() must NOT
// write their own JSON envelope.
type UpstreamStatusError struct {
	Status int
}

func (e *UpstreamStatusError) Error() string {
	return fmt.Sprintf("upstream returned status %d", e.Status)
}

// UpstreamErrorResponse is returned by adapters that buffer a non-2xx
// response instead of streaming it, so the proxy can retry on a different
// provider or flush it to the client. Body capped at MaxBufferedErrorBytes.
type UpstreamErrorResponse struct {
	Status  int
	Headers http.Header
	Body    []byte
}

func (e *UpstreamErrorResponse) Error() string {
	return fmt.Sprintf("upstream returned status %d (buffered)", e.Status)
}

// MaxBufferedErrorBytes caps the buffered upstream error body; beyond this
// it's truncated and the rest of the stream is drained without retention.
const MaxBufferedErrorBytes = 64 * 1024

// ErrUpstreamIdleTimeout: SSE inactivity watchdog fired — upstream returned
// headers then stopped producing bytes for the full idle budget. Upstream-owned
// stall, not caller cancellation. Defined here (not httputil, which owns the
// watchdog) so IsRetryable can classify it without an import cycle; httputil
// re-exports it as httputil.ErrUpstreamIdleTimeout.
var ErrUpstreamIdleTimeout = errors.New("upstream sse idle timeout")

// ErrUpstreamOutputStall: output-progress watchdog fired — stream stayed alive
// on non-output frames (reasoning deltas, keepalives) but produced zero
// output-bearing content for the full budget. Root cause of the 2026-06-16
// gpt-5.x incident (a /v1/responses stream sat at zero output tokens until the
// 600s cap). Upstream-owned and retryable, like ErrUpstreamIdleTimeout;
// defined here for the same import-cycle reason and re-exported by httputil.
var ErrUpstreamOutputStall = errors.New("upstream sse output stall")

// ErrUpstreamSlowThroughput: minimum-throughput watchdog fired — upstream IS
// producing output, just too slowly (2026-06-25: deepseek-v4-flash sustained
// ~13 events/s, a clean 200 riding toward the 600s cap with no other watchdog
// tripping). Upstream-owned and retryable; defined here for the same
// import-cycle reason as the other stall sentinels.
var ErrUpstreamSlowThroughput = errors.New("upstream sse slow throughput")

// IsRetryableStatus reports whether an upstream status is worth retrying on
// a different provider: 5xx, 408, and 429. Other 4xx are the client's fault
// and won't be fixed by a different upstream.
func IsRetryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, // 408
		http.StatusTooManyRequests: // 429
		return true
	}
	return status >= 500 && status <= 599
}

// IsRetryable reports whether err is safe to retry on a different provider,
// i.e. no response bytes reached the client. False for *UpstreamStatusError
// (bytes already flushed) and nil.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if isResponseHeaderTimeout(err) {
		return true
	}
	// All three stall sentinels are upstream-owned even though the watchdog
	// surfaces them by canceling the request context (which may also chain
	// context.Canceled) — so they must be checked before the cancellation
	// guard below.
	if errors.Is(err, ErrUpstreamIdleTimeout) || errors.Is(err, ErrUpstreamOutputStall) || errors.Is(err, ErrUpstreamSlowThroughput) {
		return true
	}
	// Caller-side cancellation/deadlines aren't the upstream's fault; a retry
	// would fire after the client is gone or reuse an elapsed budget.
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
	// Anything else (transport/build error) is retryable; dispatchWithFallback
	// confirms no bytes were written before actually retrying.
	return true
}

func isResponseHeaderTimeout(err error) bool {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) || urlErr.Err == nil {
		return false
	}
	return strings.Contains(urlErr.Err.Error(), "timeout awaiting response headers")
}

// IsUpstreamModelNotFound reports whether err is a buffered upstream 404,
// meaning the chosen provider doesn't serve the model (stale id, renamed
// binding, no active endpoint). Retrying the SAME binding is futile but a
// DIFFERENT one may carry the model, so this gates cross-binding failover —
// distinct from IsRetryable, which covers same-provider transient faults.
func IsUpstreamModelNotFound(err error) bool {
	var buffered *UpstreamErrorResponse
	if errors.As(err, &buffered) {
		return buffered.Status == http.StatusNotFound
	}
	return false
}

// PreparedRequest holds the encoded target-format request body and format-specific header overrides.
// Endpoint selects which upstream path a provider client POSTs to. Zero value
// is chat/completions; EndpointResponses routes to `/v1/responses`, required
// for reasoning models (gpt-5.x) that reject reasoning_effort + tools on
// chat/completions.
type Endpoint int

const (
	EndpointChatCompletions Endpoint = iota
	EndpointResponses
)

type PreparedRequest struct {
	Body    []byte
	Headers http.Header
	// Endpoint selects the upstream surface (zero value = chat/completions).
	Endpoint Endpoint
	// Stats records translation-time mutations applied to the body (populated
	// by translate.Prepare*), folded into the ProxyMessages-complete log so
	// per-PR mitigation impact can be measured in production traffic.
	Stats RequestMutationStats
}

// RequestMutationStats reports translation-time mitigations the router
// applied to the upstream request body. Surfaced in the ProxyMessages-
// complete log with keys:
//   - cc_only_tools_stripped
//   - gemini_reminder_injected
//   - gemini_validated_tool_mode
type RequestMutationStats struct {
	// CCOnlyToolsStripped counts Claude-Code-only tools removed before
	// dispatching to a non-Anthropic upstream. See claudecode_tool_filter.go.
	CCOnlyToolsStripped int
	// GeminiReminderInjected is true when the Gemini 3.x tool-use reminder was
	// appended to systemInstruction. See translate/system_reminder.go.
	GeminiReminderInjected bool
	// GeminiValidatedToolMode is true when functionCallingConfig.mode=VALIDATED
	// was set (Gemini 3.x, tools present, no forced tool_choice). Such requests
	// can 400 with a generic INVALID_ARGUMENT when Gemini can't compile the
	// tool schema; the proxy uses this to decide if an AUTO-mode retry is worth
	// attempting. See translate/emit_gemini.go.
	GeminiValidatedToolMode bool
}

// OutputProgressArmer is implemented by a streaming writer that can
// distinguish output-bearing frames from keepalive/reasoning frames, letting
// an adapter wire the output-progress watchdog (see ErrUpstreamOutputStall).
// ArmOutputProgress installs mark (called on each output-bearing frame) and
// reports whether it armed — false for a non-streaming writer, which is
// byte-idle-guarded only.
type OutputProgressArmer interface {
	ArmOutputProgress(mark func()) (armed bool)
}

type Client interface {
	// Proxy forwards prep.Body verbatim to the upstream and streams the response into w.
	Proxy(ctx context.Context, decision router.Decision, prep PreparedRequest, w http.ResponseWriter, r *http.Request) error

	// Passthrough forwards an inbound request to the same path on the upstream with no model rewriting.
	Passthrough(ctx context.Context, prep PreparedRequest, w http.ResponseWriter, r *http.Request) error
}
