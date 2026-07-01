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

// UpstreamHeaderObserver receives the call's context and an upstream response's
// headers so the proxy can record subscription rate-limit headroom (see
// internal/proxy/usage) without coupling provider adapters to the observer. The
// context lets the observer check the call's resolved credential, so it records
// only responses actually served on the caller's subscription (not, e.g., the
// handover summarizer's deployment-key call on the same request). Provider
// clients invoke it (when present on the context) right after the upstream
// responds; it must be cheap and non-blocking.
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

// Adding a provider is a THREE-map edit that must stay in lockstep: the
// Provider* constant here, its APIKeyEnvVars entry (deployment-key wiring), and
// its ProviderFamilies entry (translation-dispatch family). Omit the family
// entry and an inbound request routed to the new provider falls through every
// cross-format dispatch switch to the ErrProviderNotConfigured default → a
// silent 502 in prod even though the provider looked "enabled" at boot. The
// boot-time ValidateDispatchable guard and the providers-package table test both
// exist to make that omission fail loudly rather than in production; keep all
// three maps covering the same key set.
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

// TranslationFamily is the wire-format family a provider speaks, which selects
// the cross-format translation/dispatch path in the proxy. It is the structural
// replacement for the provider-name lists that were duplicated across the
// dispatch switches: dispatch keys off the family, not off an enumerated set of
// names, so a newly-added OpenAI-compatible provider is routed correctly the
// moment it gets a ProviderFamilies entry.
type TranslationFamily int

const (
	// FamilyUnknown is the zero value: a provider with no ProviderFamilies
	// entry. It must never reach the request path (ValidateDispatchable panics
	// the process at boot if a registered provider maps to it).
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

// ProviderFamilies maps every Provider* constant to the wire-format family it
// speaks. It is the single source of truth for cross-format dispatch; keep it
// covering EVERY Provider* constant (see the const block's three-map note).
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

// ValidateDispatchable reports an error if any registered provider maps to
// FamilyUnknown — i.e. is missing from ProviderFamilies and would fall through
// every cross-format dispatch switch to ErrProviderNotConfigured at request
// time. The composition root calls this right after the provider map is built
// and panics on error so the misconfiguration aborts the process at boot rather
// than surfacing as a silent 502 in production.
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

// ErrUpstreamIdleTimeout is the sentinel cause set on a request context when
// a provider adapter's SSE inactivity watchdog fires: the upstream accepted
// the request and returned headers, then stopped producing bytes for the full
// idle budget. It marks the stall as upstream-owned, as opposed to
// caller-initiated cancellation. Defined here (rather than in httputil, which
// owns the watchdog) so IsRetryable can classify it explicitly without an
// import cycle — httputil imports providers and re-exports this value as
// httputil.ErrUpstreamIdleTimeout.
var ErrUpstreamIdleTimeout = errors.New("upstream sse idle timeout")

// ErrUpstreamOutputStall is the sentinel cause set on a request context when a
// provider adapter's OUTPUT-progress watchdog fires: the upstream returned
// headers and keeps the stream alive (event frames, reasoning-summary deltas,
// keepalives — so ErrUpstreamIdleTimeout never trips) yet produces no
// output-bearing content (assistant text / tool-call args / a terminal
// envelope) for the full output-stall budget. This is the gpt-5.x failure mode
// behind the 2026-06-16 incident: a /v1/responses stream sat at zero output
// tokens, dribbling non-output bytes, until the 600s request cap. Like
// ErrUpstreamIdleTimeout it is upstream-owned and retryable. Defined here (not
// in httputil) so IsRetryable can classify it without the import cycle;
// httputil re-exports it as httputil.ErrUpstreamOutputStall.
var ErrUpstreamOutputStall = errors.New("upstream sse output stall")

// ErrUpstreamSlowThroughput is the sentinel cause set when a provider adapter's
// minimum-throughput watchdog fires: the upstream IS producing output (so
// neither ErrUpstreamIdleTimeout nor ErrUpstreamOutputStall trips — the mark is
// fed steadily), but at a sustained rate so low that the turn would dribble for
// minutes and trip none of the other watchdogs while the client (Claude Code)
// appears frozen. This is the 2026-06-25 failure mode: a deepseek/deepseek-v4-flash
// stream emitted ~1774 output deltas over ~132s (~13 events/s) — a clean,
// steadily-flowing 200 that no existing guard classified as retryable, riding
// toward the 600s request cap. Like the other stall sentinels it is
// upstream-owned and retryable. Defined here (not in httputil) so IsRetryable
// can classify it without the import cycle; httputil re-exports it as
// httputil.ErrUpstreamSlowThroughput.
var ErrUpstreamSlowThroughput = errors.New("upstream sse slow throughput")

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
	if isResponseHeaderTimeout(err) {
		return true
	}
	// An SSE idle-timeout stall is upstream-owned even though the watchdog
	// surfaces it by canceling the request context: the upstream returned
	// headers and then went silent. A retry on the same or a different
	// binding can still serve the turn, so this must be classified before
	// the caller-cancellation guard below (the underlying transport error
	// in the chain may also carry context.Canceled from the watchdog's
	// cancel call).
	if errors.Is(err, ErrUpstreamIdleTimeout) {
		return true
	}
	// An output-progress stall is likewise upstream-owned: the stream stayed
	// byte-alive but produced no output for the full budget. Classified before
	// the caller-cancellation guard for the same reason as ErrUpstreamIdleTimeout
	// — the watchdog surfaces it via the request context's cancel cause.
	if errors.Is(err, ErrUpstreamOutputStall) {
		return true
	}
	// A sustained sub-throughput stall is upstream-owned: the stream produced
	// output the whole time, just far too slowly to be usable. Classified
	// before the caller-cancellation guard for the same reason as the other
	// stall sentinels — the watchdog surfaces it via the request context's
	// cancel cause, which may also chain context.Canceled.
	if errors.Is(err, ErrUpstreamSlowThroughput) {
		return true
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

func isResponseHeaderTimeout(err error) bool {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) || urlErr.Err == nil {
		return false
	}
	return strings.Contains(urlErr.Err.Error(), "timeout awaiting response headers")
}

// IsUpstreamModelNotFound reports whether err is a buffered upstream 404.
// In a routing dispatch a 404 means the chosen provider does not serve the
// requested model — a stale/wrong upstream id (e.g. a Bedrock binding the
// gateway renamed) or a provider with no active endpoints for it. Retrying
// the SAME binding is futile, but a DIFFERENT provider binding may carry
// the model, so this gates cross-binding failover (not same-binding retry).
// It is deliberately distinct from IsRetryable, which covers transient
// faults worth re-hitting the same provider for.
func IsUpstreamModelNotFound(err error) bool {
	var buffered *UpstreamErrorResponse
	if errors.As(err, &buffered) {
		return buffered.Status == http.StatusNotFound
	}
	return false
}

// PreparedRequest holds the encoded target-format request body and format-specific header overrides.
// Endpoint selects which upstream path a provider client POSTs to. The zero
// value is the default chat/completions surface; EndpointResponses routes to
// the OpenAI Responses API (`/v1/responses`), required for reasoning models
// (gpt-5.x) that reject reasoning_effort + tools on chat/completions.
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
//   - gemini_validated_tool_mode
type RequestMutationStats struct {
	// CCOnlyToolsStripped is the count of Claude-Code-only tools removed
	// from the request before dispatching to a non-Anthropic upstream. See
	// translate/claudecode_tool_filter.go (router PR #277).
	CCOnlyToolsStripped int
	// GeminiReminderInjected is true when the Gemini 3.x tool-use reminder
	// (geminiToolUseReminder) was appended to systemInstruction for this
	// request. See translate/system_reminder.go (router PR #276).
	GeminiReminderInjected bool
	// GeminiValidatedToolMode is true when the Gemini emit path set
	// functionCallingConfig.mode=VALIDATED for this request (Gemini 3.x, tools
	// present, no forced tool_choice). Such a request can 400 with a generic
	// INVALID_ARGUMENT when Gemini cannot compile a tool schema into its
	// decode-time grammar; the proxy uses this to decide whether an AUTO-mode
	// retry is worth attempting. See translate/emit_gemini.go.
	GeminiValidatedToolMode bool
}

// OutputProgressArmer is implemented by a streaming response writer (the
// format translator) that can distinguish output-bearing frames from
// keepalive/reasoning frames. A provider adapter type-asserts its writer to
// this interface to wire the output-progress watchdog (see ErrUpstreamOutputStall):
// ArmOutputProgress installs mark, called on every output-bearing frame, and
// reports whether the watchdog was armed (false for a non-streaming writer,
// which is byte-idle-guarded only). Defining it here keeps the contract shared
// across adapters and translator impls so a signature change fails to compile
// at every call site rather than silently falling through to the no-watchdog path.
type OutputProgressArmer interface {
	ArmOutputProgress(mark func()) (armed bool)
}

type Client interface {
	// Proxy forwards prep.Body verbatim to the upstream and streams the response into w.
	Proxy(ctx context.Context, decision router.Decision, prep PreparedRequest, w http.ResponseWriter, r *http.Request) error

	// Passthrough forwards an inbound request to the same path on the upstream with no model rewriting.
	Passthrough(ctx context.Context, prep PreparedRequest, w http.ResponseWriter, r *http.Request) error
}
