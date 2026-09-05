// Package providers defines the upstream LLM client interface, sentinel errors, and shared wire helpers.
package providers

import (
	"context"
	"errors"
	"fmt"
	"github.com/tidwall/gjson"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"weave-os/router/internal/router"
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
	ProviderBedrock    = "bedrock"
	ProviderMakora     = "makora"
	ProviderMiniMax    = "minimax"
	ProviderTogether   = "together"
	ProviderXAI        = "xai"
	// ProviderMeta is Meta's Model API (api.meta.ai), OpenAI-compatible Chat Completions surface.
	ProviderMeta = "meta"
	// ProviderWafer is Wafer Serverless' OpenAI-compatible surface; see
	// ProviderWaferAnthropic for the Anthropic-spec surface (shared WAFER_API_KEY).
	ProviderWafer = "wafer"
	// ProviderWaferAnthropic is Wafer Serverless' Anthropic-spec Messages surface
	// (pass.wafer.ai/v1/messages, bearer auth); shares WAFER_API_KEY with ProviderWafer.
	ProviderWaferAnthropic = "wafer_anthropic"
	// ProviderAnthropicGateway is an Anthropic-spec enterprise gateway using
	// Bearer auth; its endpoint is per-tenant with no deployment default.
	ProviderAnthropicGateway = "anthropic_gateway"
	// ProviderOpenAIGateway is the OpenAI-Chat-Completions-spec counterpart
	// to ProviderAnthropicGateway: a per-tenant endpoint, bearer auth, no
	// deployment default. Serves model classes the Anthropic spec cannot carry.
	ProviderOpenAIGateway = "openai_gateway"
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
	// FamilyAnthropic speaks the Anthropic Messages wire format natively
	// (Anthropic itself plus Anthropic-compatible gateways such as Wafer's).
	FamilyAnthropic
	// FamilyOpenAICompat speaks the OpenAI Chat Completions wire format
	// (OpenAI itself plus every OpenAI-compatible upstream: OpenRouter,
	// Fireworks, Bedrock's OpenAI-compat surface, Makora, MiniMax, Together,
	// XAI, Wafer).
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
	ProviderBedrock:    FamilyOpenAICompat,
	ProviderMakora:     FamilyOpenAICompat,
	ProviderMiniMax:    FamilyOpenAICompat,
	ProviderTogether:   FamilyOpenAICompat,
	ProviderXAI:        FamilyOpenAICompat,
	ProviderMeta:       FamilyOpenAICompat,
	ProviderWafer:      FamilyOpenAICompat,

	ProviderWaferAnthropic:   FamilyAnthropic,
	ProviderAnthropicGateway: FamilyAnthropic,
	ProviderOpenAIGateway:    FamilyOpenAICompat,
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

// IsGateway reports whether the provider is a customer-hosted gateway rather
// than a vendor API. A gateway serves only the models its key's aliases name,
// so routing treats it as the installation's exclusive upstream.
func IsGateway(provider string) bool {
	switch provider {
	case ProviderAnthropicGateway, ProviderOpenAIGateway:
		return true
	default:
		return false
	}
}

// SupportsAnthropicServerTools reports whether the provider natively executes
// Anthropic's server-side tools (web_search_*, web_fetch_*). Speaking the
// Anthropic wire format is not the same: gateways relay to function-tool-only
// backends and reject a server tool with a 400.
func SupportsAnthropicServerTools(provider string) bool {
	return FamilyFor(provider) == FamilyAnthropic && !IsGateway(provider)
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
	ProviderBedrock:    "AWS_BEARER_TOKEN_BEDROCK",
	ProviderMakora:     "MAKORA_API_KEY",
	ProviderMiniMax:    "MINIMAX_API_KEY",
	ProviderTogether:   "TOGETHER_API_KEY",
	ProviderXAI:        "XAI_API_KEY",
	ProviderMeta:       "META_API_KEY",
	// Wafer's two surfaces share a single account key.
	ProviderWafer:          "WAFER_API_KEY",
	ProviderWaferAnthropic: "WAFER_API_KEY",
	// Pairs with ANTHROPIC_GATEWAY_BASE_URL, the endpoint the token is scoped to.
	ProviderAnthropicGateway: "ANTHROPIC_GATEWAY_TOKEN",
	// Pairs with OPENAI_GATEWAY_BASE_URL, likewise.
	ProviderOpenAIGateway: "OPENAI_GATEWAY_TOKEN",
}

// APIKeyEnvVar returns the env-var name for the given provider, or empty
// when the provider is unknown.
func APIKeyEnvVar(provider string) string {
	return APIKeyEnvVars[provider]
}

// baseURLRequiredProviders have no vendor endpoint to default to, so a
// credential without a base URL is undispatchable.
var baseURLRequiredProviders = map[string]struct{}{
	ProviderAnthropicGateway: {},
	ProviderOpenAIGateway:    {},
}

// RequiresBaseURL reports whether a BYOK credential for this provider must
// carry its own endpoint.
func RequiresBaseURL(provider string) bool {
	_, ok := baseURLRequiredProviders[provider]
	return ok
}

// CacheTTL is the best-effort upstream prompt-cache lifetime per provider.
// Anthropic's 1h extended cache is what pinSessionTTL is sized to; OSS
// OpenAI-compatible providers cache on an undocumented minutes-scale window,
// so a pin can outlive the cache — the planner uses this to stop crediting a
// stale pin a cache-read discount it no longer earns.
var CacheTTL = map[string]time.Duration{
	ProviderAnthropic:      time.Hour,
	ProviderOpenAI:         5 * time.Minute,
	ProviderGoogle:         5 * time.Minute,
	ProviderOpenRouter:     5 * time.Minute,
	ProviderFireworks:      5 * time.Minute,
	ProviderBedrock:        5 * time.Minute,
	ProviderXAI:            5 * time.Minute,
	ProviderMeta:           5 * time.Minute,
	ProviderWafer:          5 * time.Minute,
	ProviderWaferAnthropic: 5 * time.Minute,
	// A gateway publishes no prompt-cache lifetime of its own, so it keeps the
	// conservative window rather than inheriting Anthropic's 1h extended cache.
	ProviderAnthropicGateway: 5 * time.Minute,
	ProviderOpenAIGateway:    5 * time.Minute,
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

// IsUpstreamProviderBillingBlocked reports whether err is a buffered 402
// (provider refuses the model on this account). Like 404, it is fatal for
// the binding but not the model, so it gates cross-binding failover only.
func IsUpstreamProviderBillingBlocked(err error) bool {
	var buffered *UpstreamErrorResponse
	if errors.As(err, &buffered) {
		return buffered.Status == http.StatusPaymentRequired
	}
	return false
}

// capabilityRejectionPhrases are prose 400 bodies meaning the model cannot
// serve this request shape. Substring-matching is the only signal; keep phrases
// narrow — a loose match fires on ordinary validation errors.
var capabilityRejectionPhrases = []string{
	"not a multimodal model",
	"does not support image",
	"image input is not supported",
	"image input is unsupported",
	"does not support vision",
	"multimodal input is not supported",
}

// IsUpstreamCapabilityRejection reports whether err is a buffered upstream 400
// stating the model cannot handle a modality the request carries. Deliberately
// not a cross-binding signal — the same model rejects identically elsewhere.
func IsUpstreamCapabilityRejection(err error) bool {
	var buffered *UpstreamErrorResponse
	if !errors.As(err, &buffered) || buffered.Status != http.StatusBadRequest {
		return false
	}
	body := strings.ToLower(string(buffered.Body))
	for _, phrase := range capabilityRejectionPhrases {
		if strings.Contains(body, phrase) {
			return true
		}
	}
	return false
}

// schemaRejectionPhrases are prose 400 bodies from provider grammar/schema
// compilation. Unlike capability rejections (a model property, same phrasing on
// any binding), a schema rejection is compiler-specific — a sibling or baseline
// binding CAN accept the same schemas. Keep phrases narrow: a loose match
// rescues a genuinely malformed request onto another provider, masking the bug.
var schemaRejectionPhrases = []string{
	// Fireworks grammar-compiler conflict across tool schemas.
	"conflict in schema definitions",
	// Generic grammar/structured-output rejection phrasings.
	"failed to compile grammar",
	"could not compile grammar",
	"invalid tool schema",
	"invalid function schema",
	"schema is not representable",
	"invalid input schema",
}

// IsUpstreamSchemaRejection reports whether err is a buffered upstream 400 from
// the provider's tool-schema/grammar compilation — the class behind the
// Fireworks "Conflict in schema definitions" dead turns. Cross-binding
// retryable (a different provider's compiler accepts the same schemas); not a
// same-binding retry signal (identical re-POST 400s identically).
func IsUpstreamSchemaRejection(err error) bool {
	var buffered *UpstreamErrorResponse
	if !errors.As(err, &buffered) || buffered.Status != http.StatusBadRequest {
		return false
	}
	body := strings.ToLower(string(buffered.Body))
	for _, phrase := range schemaRejectionPhrases {
		if strings.Contains(body, phrase) {
			return true
		}
	}
	return false
}

// unknownFieldPhrases are the verdicts meaning the upstream's request schema
// has no such field at all, as opposed to disliking its contents.
var unknownFieldPhrases = []string{
	"extra inputs are not permitted",
	"extra inputs not permitted",
	"extra fields not permitted",
	"unknown field",
	"unrecognized field",
	"unexpected field",
	"additional properties are not allowed",
}

// IsUpstreamOutputConfigFormatRejection reports whether err is a buffered 400
// rejecting output_config.format as an unknown field — licensing a one-shot retry.
// A schema-contents complaint names the same field but is caller-fixable and must
// not match (e.g. additionalProperties must be explicitly set to false).
func IsUpstreamOutputConfigFormatRejection(err error) bool {
	var buffered *UpstreamErrorResponse
	if !errors.As(err, &buffered) || buffered.Status != http.StatusBadRequest {
		return false
	}
	body := strings.ToLower(string(buffered.Body))
	if !strings.Contains(body, "output_config") || !strings.Contains(body, "format") {
		return false
	}
	for _, phrase := range unknownFieldPhrases {
		if strings.Contains(body, phrase) {
			return true
		}
	}
	return false
}

// IsAnthropicFastModeQuotaRejection reports whether err is a buffered 429
// refusing a fast-tier request for lack of fast-mode allocation (Anthropic
// phrases it as a rate limit of N "fast mode input tokens"), as opposed to an
// ordinary rate limit — licensing a one-shot retry at standard speed.
func IsAnthropicFastModeQuotaRejection(err error) bool {
	var buffered *UpstreamErrorResponse
	if !errors.As(err, &buffered) || buffered.Status != http.StatusTooManyRequests {
		return false
	}
	return strings.Contains(strings.ToLower(string(buffered.Body)), "fast mode")
}

// IsUpstreamPromptCacheKeyRejection reports whether err is a buffered 400
// that rejects prompt_cache_key as an unknown field — some gateways trail the
// spec and 400 bodies naming it — licensing a one-shot hint-stripped retry.
func IsUpstreamPromptCacheKeyRejection(err error) bool {
	var buffered *UpstreamErrorResponse
	if !errors.As(err, &buffered) || buffered.Status != http.StatusBadRequest {
		return false
	}
	body := strings.ToLower(string(buffered.Body))
	if !strings.Contains(body, "prompt_cache_key") {
		return false
	}
	for _, phrase := range unknownFieldPhrases {
		if strings.Contains(body, phrase) {
			return true
		}
	}
	return false
}

// responsesUnsupportedPhrases are prose bodies meaning the gateway does not
// serve /v1/responses at all (as opposed to rejecting this particular body).
// Snowflake Cortex gates the surface per account and answers 400/403 rather
// than 404, so status alone can't classify it.
var responsesUnsupportedPhrases = []string{
	"responses rest api not enabled",
	"responses api not enabled",
	"not allowed to access this endpoint",
	"unknown path",
}

// IsUpstreamResponsesUnsupported reports whether err means the upstream has no
// usable Responses API, so the caller should re-emit the turn onto
// chat/completions. A 404 covers gateways that never mount the path; the prose
// phrases cover gateways that mount it but leave it disabled per account.
func IsUpstreamResponsesUnsupported(err error) bool {
	var buffered *UpstreamErrorResponse
	if !errors.As(err, &buffered) {
		return false
	}
	if buffered.Status == http.StatusNotFound {
		return true
	}
	if buffered.Status != http.StatusBadRequest && buffered.Status != http.StatusForbidden {
		return false
	}
	body := strings.ToLower(string(buffered.Body))
	for _, phrase := range responsesUnsupportedPhrases {
		if strings.Contains(body, phrase) {
			return true
		}
	}
	return false
}

// UpstreamErrorBodyMessage extracts a provider's error message from a buffered
// non-2xx body for diagnostics: prefers {"error":{"message":...}}, then
// top-level "message", then truncated raw body. Returns "" for non-buffered or
// empty bodies. Capped at 1 KiB.
func UpstreamErrorBodyMessage(err error) string {
	const maxBodyLogBytes = 1024
	var buffered *UpstreamErrorResponse
	if !errors.As(err, &buffered) || len(buffered.Body) == 0 {
		return ""
	}
	body := buffered.Body
	if len(body) > maxBodyLogBytes {
		body = body[:maxBodyLogBytes]
	}
	if msg := gjson.GetBytes(body, "error.message"); msg.Exists() {
		return msg.String()
	}
	if msg := gjson.GetBytes(body, "message"); msg.Exists() {
		return msg.String()
	}
	return strings.TrimSpace(string(body))
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
	// ServerToolsStripped counts native server tools (web_search_*, web_fetch_*)
	// removed before emitting to a non-Anthropic upstream. See websearch.StripServerTools.
	ServerToolsStripped int
	// GeminiReminderInjected is true when the Gemini 3.x tool-use reminder was
	// appended to systemInstruction. See translate/system_reminder.go.
	GeminiReminderInjected bool
	// GeminiValidatedToolMode is true when functionCallingConfig.mode=VALIDATED
	// was set (Gemini 3.x, tools present, no forced tool_choice). Such requests
	// can 400 with a generic INVALID_ARGUMENT when Gemini can't compile the
	// tool schema; the proxy uses this to decide if an AUTO-mode retry is worth
	// attempting. See translate/emit_gemini.go.
	GeminiValidatedToolMode bool
	// Transformations carries stable, structured request transformation
	// outcomes. Aggregate fields above remain during the migration for existing
	// dashboards and callers.
	Transformations []RequestTransformation
}

// TransformationAction identifies how a request semantic was handled.
type TransformationAction string

const (
	TransformationPreserved TransformationAction = "preserved"
	TransformationRewritten TransformationAction = "rewritten"
	TransformationRejected  TransformationAction = "rejected"
	TransformationDropped   TransformationAction = "dropped"
)

// TransformationSeverity identifies whether a transformation changes request
// semantics. Paths and reasons are intended only for logs/traces, never metric
// labels.
type TransformationSeverity string

const (
	TransformationInfo  TransformationSeverity = "info"
	TransformationWarn  TransformationSeverity = "warning"
	TransformationError TransformationSeverity = "error"
)

// RequestTransformation is one structured request-conversion outcome. Code
// must be a stable low-cardinality identifier.
type RequestTransformation struct {
	Code     string
	Action   TransformationAction
	Severity TransformationSeverity
	Path     string
	Reason   string
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
