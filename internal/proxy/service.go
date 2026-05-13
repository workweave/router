package proxy

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"
	"workweave/router/internal/observability/otel"
	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/cache"
	"workweave/router/internal/router/capability"
	"workweave/router/internal/router/handover"
	"workweave/router/internal/router/planner"
	"workweave/router/internal/router/pricing"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
	"github.com/hashicorp/golang-lru/v2/expirable"
)

// Service orchestrates routing decisions and provider dispatch.
type Service struct {
	router               router.Router
	providers            map[string]providers.Client
	emitter              *otel.Emitter
	embedOnlyUserMessage bool
	// semanticCache short-circuits non-streaming requests on a cosine-similarity hit.
	semanticCache *cache.Cache
	// pinStore persists session-sticky routing decisions across instance restarts.
	// Nil when the feature flag is off; the orchestrator then runs the scorer
	// every turn and persists nothing.
	pinStore sessionpin.Store
	// pinCache absorbs the hot path; 30s TTL is short enough that pinned_until
	// in the pin store remains source of truth for validity.
	pinCache *expirable.LRU[string, sessionpin.Pin]
	// pinWriteSem bounds concurrent async pin-upsert goroutines. Writes drop
	// (non-blocking) when full so a slow Postgres can't accumulate goroutines.
	pinWriteSem chan struct{}
	// usageWriteSem bounds concurrent async last-turn-usage writeback
	// goroutines, with the same drop-on-full semantics as pinWriteSem.
	usageWriteSem chan struct{}
	// hardPinExplore gates the Explore sub-agent hard-pin.
	hardPinExplore bool
	// hardPinProvider/hardPinModel route compaction (and, when hardPinExplore is
	// on, Explore sub-agent turns). Derived at boot from the cheapest available
	// model; overridable via ROUTER_HARD_PIN_PROVIDER / ROUTER_HARD_PIN_MODEL.
	hardPinProvider string
	hardPinModel    string
	// hardPinResolver, when set, overrides the boot-time hardPin{Provider,Model}
	// per-request. Used in byokOnly (managed) deployments where the cheapest
	// model among ALL registered providers is the wrong choice — the
	// installation may only BYOK a subset, and a hard-pin to a provider they
	// can't authenticate to would 401 at dispatch. The resolver is given the
	// request's enabled-providers set (BYOK + client-creds intersection) and
	// returns (provider, model, ok). ok=false signals no eligible provider for
	// this request — the orchestrator surfaces ErrClusterUnavailable rather
	// than silently falling back.
	hardPinResolver func(enabled map[string]struct{}) (provider, model string, ok bool)
	// tierClampResolver enforces the requested-model tier ceiling: returns
	// the cheapest deployed model whose tier ≤ ceiling, whose provider is
	// in the request's enabled set, and which is NOT in the request's
	// excluded-models denylist. Nil disables the clamp. See
	// WithTierClampResolver and turnloop.go's clampToCeiling for the call
	// sites.
	tierClampResolver func(enabled, excluded map[string]struct{}, ceiling capability.Tier) (provider, model string, ok bool)
	// telemetry is an optional repository for persisting per-request telemetry.
	telemetry TelemetryRepository
	// byokOnly disables deployment-level credential fallback. When true, a
	// provider is only eligible if BYOK or client-supplied credentials are
	// present. Set on Weave-managed deployments so customer requests never
	// silently consume the platform's API key budget.
	byokOnly bool
	// excludedModelsOverride, when non-nil, replaces the per-installation
	// exclusion list on every request. Set from ROUTER_EXCLUDED_MODELS at
	// boot so headless self-hosters can pin the list in their manifest.
	excludedModelsOverride map[string]struct{}
	// deploymentKeyedProviders is the subset of registered providers whose
	// upstream API key is actually configured at the deployment level (env
	// var). It's the "default eligible" set used by enabledProvidersForRequest
	// in non-BYOK-only mode. When nil, all registered providers are treated
	// as deployment-keyed (legacy behavior preserved for tests).
	deploymentKeyedProviders map[string]struct{}
	// planner parameterizes the Prism-style EV policy that decides
	// stay-vs-switch per turn. Constructed once at boot from env.
	planner planner.EVConfig
	// plannerEnabled is the kill switch (ROUTER_PLANNER_ENABLED). When
	// false, the orchestrator falls back to first-decision-wins behavior.
	plannerEnabled bool
	// summarizer produces a bounded-cost handover summary on switch
	// turns. May be nil — the orchestrator then falls straight to
	// handover.TrimLastN on switch.
	summarizer handover.Summarizer
	// availableModels is the boot-time set of model names whose providers
	// are registered. Read by the planner to decide whether a pin's model
	// is still routable; if not, switch regardless of EV.
	availableModels map[string]struct{}
	// defaultBaselineModel is the cost-comparison baseline used when the
	// inbound RequestedModel has no pricing entry (typical of generic
	// "weave-router"-style custom names IDE clients send). Empty means no
	// substitution — savings markers and pricing fields stay empty for
	// unknown models, preserving prior behavior.
	defaultBaselineModel string
}

// pinSessionTTL is the sliding TTL on pinned_until. Mirrors Anthropic's
// prompt-cache TTL on Sonnet/Haiku/Opus 4.5+ so the pin lifecycle tracks
// the cache it's keeping warm.
const pinSessionTTL = time.Hour

// APIKeyIDContextKey is the request-context key for the authenticated api_key_id.
type APIKeyIDContextKey struct{}

// ExternalIDContextKey is the request-context key for the installation's external_id.
type ExternalIDContextKey struct{}

// CredentialsContextKey is the request-context key for resolved per-request credentials.
type CredentialsContextKey struct{}

// InstallationExcludedModelsContextKey is the request-context key for the
// authed installation's model exclusion list. Carried as []string so the
// proxy can build the request-time filter without re-reading the DB.
type InstallationExcludedModelsContextKey struct{}

// installationExcludedModelsFromContext returns the per-installation exclusion
// list stashed on ctx by the auth middleware, or nil when none is present.

// routingMarkerFor builds the upfront "brand → model · reason" snippet emitted
// at the start of every cross-format streamed response.
func routingMarkerFor(res turnLoopResult) string {
	decision := res.Decision
	if decision.Model == "" {
		return ""
	}
	parts := []string{"✦ **Weave Router** → " + decision.Model}
	if decision.Provider != "" {
		parts[0] += " (" + decision.Provider + ")"
	}
	if reason := routingReasonShort(res); reason != "" {
		parts = append(parts, "reason: "+reason)
	}
	if note := clampNote(res); note != "" {
		parts = append(parts, note)
	}
	return strings.Join(parts, " · ") + "\n\n"
}

// clampNote surfaces the tier-ceiling clamp to the caller when it fired:
// names the runner-up model the scorer actually preferred and points at
// the action that would unlock it (request a higher-tier model). Empty
// string when no clamp occurred.
func clampNote(res turnLoopResult) string {
	if !res.TierClamped || res.PreClampModel == "" {
		return ""
	}
	upsell := upsellModelFor(res.RequestedTier)
	if upsell == "" {
		return fmt.Sprintf("second-choice pick (would have used %s — capped to your requested %s tier)", res.PreClampModel, res.RequestedTier.String())
	}
	return fmt.Sprintf("second-choice pick (would have used %s — capped to your requested %s tier; request %s to unlock higher-tier picks)", res.PreClampModel, res.RequestedTier.String(), upsell)
}

// upsellModelFor returns the conventional next-tier-up model name to
// suggest in the clamp note. High tier has no upsell. Unknown returns
// empty so the marker falls back to the no-upsell wording.
func upsellModelFor(t capability.Tier) string {
	switch t {
	case capability.TierLow:
		return "claude-sonnet-4-5"
	case capability.TierMid:
		return "claude-opus-4-7"
	default:
		return ""
	}
}

// closingMarkerFor returns a callback that formats a "saved $X vs <baseline>"
// line from observed usage. Returns "" when routed == baseline, pricing is
// missing, or savings are non-positive / below the flicker floor.
//
// When requestedModel and baselineModel differ, the inbound model name had
// no pricing entry and the baseline was substituted in; the marker labels
// the comparison "(configured baseline)" so users see the savings are an
// attribution rather than against a literal model they requested.
func closingMarkerFor(decision router.Decision, requestedModel, baselineModel string) func(translate.Usage) string {
	return func(u translate.Usage) string {
		if decision.Model == "" || baselineModel == "" {
			return ""
		}
		if decision.Model == baselineModel {
			return ""
		}
		routed, ok1 := pricing.For(decision.Model)
		baseline, ok2 := pricing.For(baselineModel)
		if !ok1 || !ok2 {
			return ""
		}
		savings := closingMarkerSavingsUSD(u, routed, baseline)
		if savings < 0.0001 {
			return ""
		}
		label := baselineModel
		if requestedModel != baselineModel {
			label = baselineModel + " (configured baseline)"
		}
		return fmt.Sprintf("✦ saved $%.4f vs %s (%s in / %s out)",
			savings, label,
			formatTokenCount(u.InputTokens), formatTokenCount(u.OutputTokens))
	}
}

// formatTokenCount renders a token count as raw / "Nk" / "N.NM".
func formatTokenCount(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 1_000_000:
		return strconv.Itoa(n/1000) + "k"
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
}

// closingMarkerSavingsUSD = requested-model cost − routed-model cost for the
// same usage shape.
func closingMarkerSavingsUSD(u translate.Usage, routed, requested pricing.Pricing) float64 {
	nonCached := u.InputTokens - u.CacheReadTokens
	if nonCached < 0 {
		nonCached = 0
	}
	routedCost := costForUsage(nonCached, u.CacheReadTokens, u.OutputTokens, routed)
	requestedCost := costForUsage(nonCached, u.CacheReadTokens, u.OutputTokens, requested)
	return requestedCost - routedCost
}

// costForUsage prices (nonCached, cacheRead, output) tokens against p.
func costForUsage(nonCachedInput, cacheReadInput, output int, p pricing.Pricing) float64 {
	cacheMult := p.EffectiveCacheReadMultiplier()
	inputCost := float64(nonCachedInput)*p.InputUSDPer1M +
		float64(cacheReadInput)*p.InputUSDPer1M*cacheMult
	outputCost := float64(output) * p.OutputUSDPer1M
	return (inputCost + outputCost) / 1e6
}

// routingReasonShort returns a human-readable reason for the marker, falling
// back to the orchestrator path when the planner didn't run.
func routingReasonShort(res turnLoopResult) string {
	if res.PlannerDecision.Reason != "" {
		return humanReasonFromPlanner(res.PlannerDecision.Reason)
	}
	if res.HardPinned {
		return "hard pin (compaction / sub-agent)"
	}
	if res.StickyHit {
		return "tool-result follow-up"
	}
	return "top scorer"
}

// humanReasonFromPlanner maps planner.Reason* codes to marker prose. Unknown
// codes pass through verbatim so new reasons surface visibly.
func humanReasonFromPlanner(code string) string {
	switch code {
	case planner.ReasonEVPositive:
		return "switched to save on cache reads"
	case planner.ReasonEVNegative:
		return "stayed: cache reuse beats the switch"
	case planner.ReasonSameModel:
		return "scorer matches the pin"
	case planner.ReasonNoPin:
		return "top scorer"
	case planner.ReasonNoPriorUsage:
		return "no cache stats yet"
	case planner.ReasonPinModelMissing:
		return "pin model no longer available"
	case planner.ReasonPricingMissing:
		return "missing pricing for a candidate"
	case planner.ReasonTierUpgrade:
		return "model tier upgrade"
	default:
		return code
	}
}

func installationExcludedModelsFromContext(ctx context.Context) []string {
	v := ctx.Value(InstallationExcludedModelsContextKey{})
	if v == nil {
		return nil
	}
	out, _ := v.([]string)
	return out
}

// excludedModelsForRequest returns the model exclusion set to apply for this
// request. The env-driven override wins; otherwise the installation list (if
// any) is converted to a set. Returns nil when neither is configured.
func (s *Service) excludedModelsForRequest(ctx context.Context) map[string]struct{} {
	if s.excludedModelsOverride != nil {
		return s.excludedModelsOverride
	}
	excluded := installationExcludedModelsFromContext(ctx)
	if len(excluded) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(excluded))
	for _, m := range excluded {
		out[m] = struct{}{}
	}
	return out
}

// CredentialsFromContext returns the resolved credentials stashed on ctx,
// or nil when none are present (plan-based auth path).
func CredentialsFromContext(ctx context.Context) *Credentials {
	v := ctx.Value(CredentialsContextKey{})
	if v == nil {
		return nil
	}
	creds, _ := v.(*Credentials)
	return creds
}

// DefaultPlannerThresholdUSD is the minimum positive EV (over the
// remaining-turn horizon) required to switch off a pinned model. Small
// enough that genuine arbitrage triggers a switch; large enough that
// near-tie noise doesn't flap decisions.
const DefaultPlannerThresholdUSD = 0.001

// DefaultPlannerExpectedRemainingTurns is the horizon used to amortize
// per-turn savings. Matches observed agentic-loop tail length.
const DefaultPlannerExpectedRemainingTurns = 3

// DefaultPlannerTierUpgradeEnabled turns on the tier guard so a trivial
// first turn can't pin a Low-tier model for the rest of the session.
// See internal/router/capability.
const DefaultPlannerTierUpgradeEnabled = true

// session-pin feature flag is off. The planner runs by default with the
// conservative EVConfig above; callers tune it via WithPlanner /
// WithPlannerEnabled / WithSummarizer / WithAvailableModels.
func NewService(r router.Router, providerMap map[string]providers.Client, emitter *otel.Emitter, embedOnlyUserMessage bool, semanticCache *cache.Cache, pinStore sessionpin.Store, hardPinExplore bool, hardPinProvider, hardPinModel string, telemetry TelemetryRepository) *Service {
	var pinCache *expirable.LRU[string, sessionpin.Pin]
	var pinWriteSem chan struct{}
	var usageWriteSem chan struct{}
	if pinStore != nil {
		pinCache = expirable.NewLRU[string, sessionpin.Pin](10000, nil, 30*time.Second)
		pinWriteSem = make(chan struct{}, 64)
		usageWriteSem = make(chan struct{}, 64)
	}
	return &Service{
		router:               r,
		providers:            providerMap,
		emitter:              emitter,
		embedOnlyUserMessage: embedOnlyUserMessage,
		semanticCache:        semanticCache,
		pinStore:             pinStore,
		pinCache:             pinCache,
		pinWriteSem:          pinWriteSem,
		usageWriteSem:        usageWriteSem,
		hardPinExplore:       hardPinExplore,
		hardPinProvider:      hardPinProvider,
		hardPinModel:         hardPinModel,
		telemetry:            telemetry,
		planner: planner.EVConfig{
			ThresholdUSD:           DefaultPlannerThresholdUSD,
			ExpectedRemainingTurns: DefaultPlannerExpectedRemainingTurns,
			TierUpgradeEnabled:     DefaultPlannerTierUpgradeEnabled,
		},
		plannerEnabled: true,
	}
}

// WithPlanner overrides the EV-policy configuration. ThresholdUSD is
// assigned verbatim — zero and negative values are legitimate operator-
// chosen settings (the planner switches when expectedSavings -
// evictionCost > threshold; the test plan documents -1 as the force-
// switch knob). ExpectedRemainingTurns falls back to the default on
// non-positive values because amortizing savings over <= 0 turns has no
// meaningful interpretation.
func (s *Service) WithPlanner(cfg planner.EVConfig) *Service {
	s.planner.ThresholdUSD = cfg.ThresholdUSD
	if cfg.ExpectedRemainingTurns > 0 {
		s.planner.ExpectedRemainingTurns = cfg.ExpectedRemainingTurns
	}
	s.planner.TierUpgradeEnabled = cfg.TierUpgradeEnabled
	return s
}

// WithPlannerEnabled is the kill switch (ROUTER_PLANNER_ENABLED). When
// false, the orchestrator preserves the "first decision wins" behavior
// (Tier-1/2 pin lookup, scorer fallback, no EV math).
func (s *Service) WithPlannerEnabled(enabled bool) *Service {
	s.plannerEnabled = enabled
	return s
}

// WithSummarizer installs the cheap-model summarizer used to bound
// handover cost on switch turns. nil disables the synchronous summary
// step; the orchestrator then trims-last-N on every switch.
func (s *Service) WithSummarizer(sz handover.Summarizer) *Service {
	s.summarizer = sz
	return s
}

// WithAvailableModels installs the boot-time set of model names whose
// providers are registered. The planner consults this set so a pin
// whose model is no longer routable forces a switch regardless of EV.
// Passing nil clears the set (every model is considered available).
func (s *Service) WithAvailableModels(models map[string]struct{}) *Service {
	if models == nil {
		s.availableModels = nil
		return s
	}
	copied := make(map[string]struct{}, len(models))
	for m := range models {
		copied[m] = struct{}{}
	}
	s.availableModels = copied
	return s
}

// WithHardPinResolver installs a per-request hard-pin resolver. The
// resolver is consulted on compaction/probe/Explore turns when set; nil
// preserves the boot-time hardPin{Provider,Model} for every request
// (the selfhosted default). The resolver receives the request's enabled
// providers set; ok=false signals the request has no eligible provider
// and the turnloop should surface ErrClusterUnavailable rather than
// dispatching to a provider the request can't authenticate to.
func (s *Service) WithHardPinResolver(resolver func(enabled map[string]struct{}) (provider, model string, ok bool)) *Service {
	s.hardPinResolver = resolver
	return s
}

// WithDefaultBaselineModel installs the cost-comparison fallback used
// when the inbound RequestedModel has no pricing entry. Empty disables
// substitution. See [Service.defaultBaselineModel] for rationale.
func (s *Service) WithDefaultBaselineModel(model string) *Service {
	s.defaultBaselineModel = model
	return s
}

// WithTierClampResolver installs the resolver used by the tier-ceiling
// guard. Given a request's enabled-providers set and a tier ceiling, it
// returns the cheapest registry-deployed model whose tier is ≤ ceiling
// and whose provider is enabled. ok=false means no in-ceiling model is
// routable for this request — the orchestrator preserves the original
// (unclamped) decision rather than failing the turn. Nil resolver
// disables clamping (preserves pre-tier-ceiling behavior).
func (s *Service) WithTierClampResolver(resolver func(enabled, excluded map[string]struct{}, ceiling capability.Tier) (provider, model string, ok bool)) *Service {
	s.tierClampResolver = resolver
	return s
}

// baselineFor returns requested if it has a pricing entry; otherwise the
// configured defaultBaselineModel (which may itself be ""). Used by every
// cost/savings lookup so unknown client model names (e.g. "weave-router")
// still attribute savings to a real baseline.
func (s *Service) baselineFor(requested string) string {
	if requested != "" {
		if _, ok := pricing.For(requested); ok {
			return requested
		}
	}
	return s.defaultBaselineModel
}

// WithByokOnly enables BYOK-only credential resolution: providers without
// caller-supplied credentials are ineligible, even if a deployment-level env
// key is registered.
func (s *Service) WithByokOnly(byokOnly bool) *Service {
	s.byokOnly = byokOnly
	return s
}

// WithExcludedModelsOverride pins the per-request model exclusion list to a
// deployment-wide set, ignoring per-installation DB state. Pass nil or an
// empty slice to clear the override (per-installation DB state then takes
// over). Used for headless self-hosted deployments that pin config in env.
func (s *Service) WithExcludedModelsOverride(models []string) *Service {
	if len(models) == 0 {
		s.excludedModelsOverride = nil
		return s
	}
	set := make(map[string]struct{}, len(models))
	for _, m := range models {
		set[m] = struct{}{}
	}
	s.excludedModelsOverride = set
	return s
}

// HasExcludedModelsOverride reports whether ROUTER_EXCLUDED_MODELS (or an
// equivalent override) is in effect. Admin handlers use this to render the
// UI read-only and to reject mutating PUTs.
func (s *Service) HasExcludedModelsOverride() bool {
	return s.excludedModelsOverride != nil
}

// ExcludedModelsOverride returns a sorted copy of the override list, or nil
// when no override is active. Surfaced via the admin GET endpoint so the UI
// can show the operator which models the env var pins off.
func (s *Service) ExcludedModelsOverride() []string {
	if s.excludedModelsOverride == nil {
		return nil
	}
	out := make([]string, 0, len(s.excludedModelsOverride))
	for m := range s.excludedModelsOverride {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// usageRequired reports whether per-request token usage must be captured.
// Both OTel export and DB telemetry persistence need it; without either, we
// can skip the SSE include_usage flag + extractor wiring.
//
// This used to be gated on s.emitter alone, which meant local deployments
// without OTEL_EXPORTER_OTLP_ENDPOINT silently persisted every telemetry
// row with input/output_tokens = 0 (and therefore $0 cost). Telemetry
// persistence is the source of truth for the dashboard's cost panel — it
// can't depend on OTel being configured.
func (s *Service) usageRequired() bool {
	return s.emitter != nil || s.telemetry != nil
}

// WithDeploymentKeyedProviders restricts the "default eligible" set for
// non-BYOK-only requests to providers whose deployment env key is actually
// set. Without this, every registered provider is eligible by default —
// which is wrong when we register a provider with an empty key purely so
// BYOK keys can route to it. Passing nil restores the legacy "all
// registered providers eligible" behavior.
func (s *Service) WithDeploymentKeyedProviders(set map[string]struct{}) *Service {
	if set == nil {
		s.deploymentKeyedProviders = nil
		return s
	}
	copied := make(map[string]struct{}, len(set))
	for p := range set {
		copied[p] = struct{}{}
	}
	s.deploymentKeyedProviders = copied
	return s
}

// MetricsSummary returns aggregated cost/token totals for the given installation and time window.
func (s *Service) MetricsSummary(ctx context.Context, installationID string, from, to time.Time) (TelemetrySummary, error) {
	if s.telemetry == nil {
		return TelemetrySummary{}, nil
	}
	return s.telemetry.GetTelemetrySummary(ctx, installationID, from, to)
}

// MetricsTimeseries returns per-bucket cost rows for the cost savings chart.
func (s *Service) MetricsTimeseries(ctx context.Context, installationID string, from, to time.Time, granularity string) ([]TelemetryBucket, error) {
	if s.telemetry == nil {
		return nil, nil
	}
	return s.telemetry.GetTelemetryTimeseries(ctx, installationID, from, to, granularity)
}

// MetricsSummaryAll aggregates totals across every installation. Admin-only.
func (s *Service) MetricsSummaryAll(ctx context.Context, from, to time.Time) (TelemetrySummary, error) {
	if s.telemetry == nil {
		return TelemetrySummary{}, nil
	}
	return s.telemetry.GetTelemetrySummaryAll(ctx, from, to)
}

// MetricsTimeseriesAll returns per-bucket cost rows across every installation.
func (s *Service) MetricsTimeseriesAll(ctx context.Context, from, to time.Time, granularity string) ([]TelemetryBucket, error) {
	if s.telemetry == nil {
		return nil, nil
	}
	return s.telemetry.GetTelemetryTimeseriesAll(ctx, from, to, granularity)
}

// MetricsRows returns individual telemetry rows for an installation in [from, to).
func (s *Service) MetricsRows(ctx context.Context, installationID string, from, to time.Time, limit int32) ([]TelemetryRow, error) {
	if s.telemetry == nil {
		return nil, nil
	}
	return s.telemetry.GetTelemetryRows(ctx, installationID, from, to, limit)
}

// MetricsRowsAll returns individual telemetry rows across every installation.
func (s *Service) MetricsRowsAll(ctx context.Context, from, to time.Time, limit int32) ([]TelemetryRow, error) {
	if s.telemetry == nil {
		return nil, nil
	}
	return s.telemetry.GetTelemetryRowsAll(ctx, from, to, limit)
}

// ErrProviderNotConfigured is returned when a routing decision selects a
// provider that is not present in the registry.
var ErrProviderNotConfigured = errors.New("provider not configured")

// semanticCacheMaxBodyBytes caps how large a response the cache will store;
// larger bodies stream through but skip the Store call to bound peak memory.
const semanticCacheMaxBodyBytes = 1 << 20

// headersToSkipOnHit lists response headers the cache must NOT replay.
// request-id ties to a specific upstream call; x-router-* are set fresh from
// the live decision so the client sees current routing, not stale.
var headersToSkipOnHit = map[string]struct{}{
	"Request-Id":        {},
	"X-Request-Id":      {},
	"X-Router-Decision": {},
	"X-Router-Provider": {},
	"X-Router-Model":    {},
	"X-Router-Cache":    {},
}

// cloneCacheHeaders snapshots a header set for storage, dropping transient
// identifiers that must not survive replay.
func cloneCacheHeaders(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vs := range h {
		if _, skip := headersToSkipOnHit[http.CanonicalHeaderKey(k)]; skip {
			continue
		}
		copied := make([]string, len(vs))
		copy(copied, vs)
		out[k] = copied
	}
	return out
}

// writeCachedResponse emits a stored CachedResponse. x-router-* headers come
// from the live decision so the client sees an accurate routing trace.
func (s *Service) writeCachedResponse(w http.ResponseWriter, resp cache.CachedResponse, decision router.Decision) {
	for k, vs := range resp.Headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("x-router-decision", decision.Reason)
	w.Header().Set("x-router-provider", decision.Provider)
	w.Header().Set("x-router-model", decision.Model)
	w.Header().Set("x-router-cache", "hit")
	if resp.StatusCode != 0 && resp.StatusCode != http.StatusOK {
		w.WriteHeader(resp.StatusCode)
	}
	_, _ = w.Write(resp.Body)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// EmbedOnlyUserMessageContextKey is the context key for the per-request embed flag override.
type EmbedOnlyUserMessageContextKey struct{}

// embedOnlyUserMessageOverride reads the per-request embed flag from ctx.
func embedOnlyUserMessageOverride(ctx context.Context) (bool, bool) {
	v, ok := ctx.Value(EmbedOnlyUserMessageContextKey{}).(bool)
	return v, ok
}

// ResolveEmbedOnlyUserMessage reports the effective embed-only-user flag for
// ctx, applying the per-request override (if set) on top of the service
// default. Exposed so handlers outside this package (e.g. /v1/route) can use
// the same resolution as ProxyMessages and stay in sync with customer-visible
// routing behavior.
func (s *Service) ResolveEmbedOnlyUserMessage(ctx context.Context) bool {
	flag := s.embedOnlyUserMessage
	if v, ok := embedOnlyUserMessageOverride(ctx); ok {
		flag = v
	}
	return flag
}

func (s *Service) provider(name string) (providers.Client, error) {
	p, ok := s.providers[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrProviderNotConfigured, name)
	}
	return p, nil
}

// Route exposes the underlying router for callers that need a decision
// without dispatching (e.g. admin endpoints).
func (s *Service) Route(ctx context.Context, req router.Request) (router.Decision, error) {
	return s.router.Route(ctx, req)
}

// PassthroughToProvider forwards a non-routing request to the default
// (Anthropic) provider for metadata endpoints (count_tokens, models).
func (s *Service) PassthroughToProvider(ctx context.Context, body []byte, w http.ResponseWriter, r *http.Request) error {
	return s.PassthroughToNamedProvider(ctx, providers.ProviderAnthropic, body, w, r)
}

// PassthroughToNamedProvider forwards a non-routing request to a specific
// provider. No model rewriting, no routing decision. Anthropic targets get
// the body scrubbed via envelope parsing; others receive it verbatim.
func (s *Service) PassthroughToNamedProvider(ctx context.Context, providerName string, body []byte, w http.ResponseWriter, r *http.Request) error {
	log := observability.Get()
	p, err := s.provider(providerName)
	if err != nil {
		return err
	}

	var prep providers.PreparedRequest
	if providerName == providers.ProviderAnthropic && len(body) > 0 {
		env, parseErr := translate.ParseAnthropic(body)
		if parseErr == nil {
			prep, err = env.PrepareAnthropicPassthrough(r.Header)
			if err != nil {
				return fmt.Errorf("prepare passthrough: %w", err)
			}
		} else {
			prep = providers.PreparedRequest{Body: body, Headers: translate.AnthropicPassthroughHeaders(r.Header)}
		}
	} else if providerName == providers.ProviderAnthropic {
		prep = providers.PreparedRequest{Body: body, Headers: translate.AnthropicPassthroughHeaders(r.Header)}
	} else {
		prep = providers.PreparedRequest{Body: body, Headers: make(http.Header)}
	}

	proxyStart := time.Now()
	proxyErr := p.Passthrough(ctx, prep, w, r)
	proxyMs := time.Since(proxyStart).Milliseconds()
	log.Info("PassthroughToProvider complete", "provider", providerName, "path", r.URL.Path, "method", r.Method, "proxy_ms", proxyMs, "proxy_err", proxyErr)
	return proxyErr
}

// ProxyMessages routes a raw Anthropic-Messages request body and streams the
// upstream response back. The routing decision is reflected in x-router-* headers.
func (s *Service) ProxyMessages(ctx context.Context, body []byte, w http.ResponseWriter, r *http.Request) error {
	log := observability.Get()
	requestStart := time.Now()
	requestID := uuid.New().String()
	buf := otel.NewBuffer(s.emitter)
	ctx = buf.WithContext(ctx)

	env, parseErr := translate.ParseAnthropic(body)
	if parseErr != nil {
		log.Error("Failed to parse Anthropic request", "err", parseErr)
		return fmt.Errorf("parse request: %w", parseErr)
	}

	embedFlag := s.embedOnlyUserMessage
	if v, ok := embedOnlyUserMessageOverride(ctx); ok {
		embedFlag = v
	}
	feats := env.RoutingFeatures(embedFlag)
	promptText := feats.PromptText
	embedInput := "concatenated_stream"
	if embedFlag && feats.OnlyUserMessageText != "" {
		promptText = feats.OnlyUserMessageText
		embedInput = "only_user_message"
	}

	apiKeyID, _ := ctx.Value(APIKeyIDContextKey{}).(string)
	externalID, _ := ctx.Value(ExternalIDContextKey{}).(string)
	installationID := installationIDFromContext(ctx)
	clientID := ClientIdentityFrom(ctx)
	bypassEval := hasEvalOverrideHeader(r)

	// Anthropic packs sub-agent identity into metadata.user_id; the
	// x-weave-subagent-type header is for non-Anthropic ingress only.
	routeStart := time.Now()
	routeRes, routeErr := s.runTurnLoop(ctx, env, feats, apiKeyID, installationID, "", r.Header, router.Request{
		RequestedModel:       feats.Model,
		EstimatedInputTokens: feats.Tokens,
		HasTools:             feats.HasTools,
		PromptText:           promptText,
		EnabledProviders:     s.enabledProvidersForRequest(ctx, providers.ProviderAnthropic, r.Header),
		ExcludedModels:       s.excludedModelsForRequest(ctx),
	})
	if routeErr != nil {
		log.Error("Routing failed", "err", routeErr, "route_ms", time.Since(routeStart).Milliseconds(), "requested_model", feats.Model, "estimated_input_tokens", feats.Tokens)
		return routeErr
	}
	decision := routeRes.Decision
	tt := routeRes.TurnType
	stickyHit := routeRes.StickyHit
	pinTier := routeRes.PinTier
	pinAgeSec := routeRes.PinAgeSec
	routeMs := time.Since(routeStart).Milliseconds()
	s.logPlannerOutcome(routeRes)

	// Semantic-cache eligibility: configured, non-streaming, decision has
	// metadata, externalID present, not eval traffic (eval bypasses to keep
	// per-prompt accuracy attribution clean).
	cacheEligible := s.semanticCache != nil && !env.Stream() && decision.Metadata != nil && externalID != "" && !bypassEval
	if cacheEligible {
		if resp, hit := s.semanticCache.Lookup(externalID, cache.FormatAnthropic, decision.Metadata.Embedding, decision.Metadata.ClusterIDs); hit {
			s.writeCachedResponse(w, resp, decision)
			otel.Record(ctx, otel.Span{
				Name:  "router.cache_hit",
				Start: requestStart,
				End:   time.Now(),
				Attrs: otel.NewAttrBuilder(7).
					String("request_id", requestID).
					String("external_id", externalID).
					String("decision.model", decision.Model).
					String("decision.provider", decision.Provider).
					Bool("cache.hit", true).
					String("cache.format", string(cache.FormatAnthropic)).
					Int64("latency.total_ms", time.Since(requestStart).Milliseconds()).
					Build(),
			})
			otel.Flush(ctx)
			log.Info("ProxyMessages cache hit", "requested_model", feats.Model, "baseline_model", s.baselineFor(feats.Model), "decision_model", decision.Model, "decision_provider", decision.Provider, "external_id", externalID, "total_ms", time.Since(requestStart).Milliseconds())
			return nil
		}
	}

	w.Header().Set("x-router-decision", decision.Reason)
	w.Header().Set("x-router-provider", decision.Provider)
	w.Header().Set("x-router-model", decision.Model)

	p, provErr := s.provider(decision.Provider)
	if provErr != nil {
		return provErr
	}

	reqPricing := otel.Lookup(s.baselineFor(feats.Model))
	actPricing := otel.Lookup(decision.Model)
	decisionBuilder := otel.NewAttrBuilder(40).
		String("request_id", requestID).
		String("external_id", externalID).
		String("router_user_id", auth.UserIDFrom(ctx)).
		String("client.device_id", clientID.DeviceID).
		String("client.account_id", clientID.AccountID).
		String("client.session_id", clientID.SessionID).
		String("client.user_agent", clientID.UserAgent).
		String("client.app", clientID.ClientApp).
		String("requested.model", feats.Model).
		String("decision.model", decision.Model).
		String("decision.provider", decision.Provider).
		String("decision.reason", decision.Reason).
		Bool("routing.sticky_hit", stickyHit).
		Bool("routing.session_pin_hit", pinTier == "in_proc" || pinTier == "postgres").
		String("routing.session_pin_tier", pinTier).
		Int64("routing.session_pin_age_s", pinAgeSec).
		String("routing.turn_type", string(tt)).
		String("routing.embed_input", embedInput).
		Int64("routing.estimated_input_tokens", int64(feats.Tokens)).
		IntSlice("routing.cluster_ids", clusterIDsFromDecision(decision)).
		Float64("pricing.requested_input_per_1m", reqPricing.InputUSDPer1M).
		Float64("pricing.requested_output_per_1m", reqPricing.OutputUSDPer1M).
		Float64("pricing.actual_input_per_1m", actPricing.InputUSDPer1M).
		Float64("pricing.actual_output_per_1m", actPricing.OutputUSDPer1M).
		Int64("latency.route_ms", routeMs)
	applyPlannerAttrs(decisionBuilder, routeRes)
	otel.Record(ctx, otel.Span{
		Name:  "router.decision",
		Start: requestStart,
		End:   time.Now(),
		Attrs: decisionBuilder.Build(),
	})
	otel.Flush(ctx)

	opts := translate.EmitOptions{
		TargetModel:        decision.Model,
		Capabilities:       router.Lookup(decision.Model),
		IncludeStreamUsage: s.usageRequired(),
	}

	// BYOK takes precedence; inbound headers supply fallback credentials.
	ctx = resolveAndInjectCredentials(ctx, decision.Provider, r.Header)

	// Mirror post-translation wire bytes into a buffer for post-Proxy storage.
	var captureW *captureWriter
	var sink http.ResponseWriter = w
	if cacheEligible {
		captureW = newCaptureWriter(w, semanticCacheMaxBodyBytes)
		sink = captureW
	}

	proxyStart := time.Now()
	var proxyErr error
	crossFormat := false
	var extractor *otel.UsageExtractor

	switch decision.Provider {
	case providers.ProviderAnthropic:
		prep, emitErr := env.PrepareAnthropic(r.Header, opts)
		if emitErr != nil {
			log.Error("Failed to emit Anthropic body", "err", emitErr)
			return fmt.Errorf("emit body: %w", emitErr)
		}
		proxyWriter := sink
		if s.usageRequired() {
			extractor = otel.NewUsageExtractor(sink, decision.Provider)
			proxyWriter = extractor
		}
		proxyErr = p.Proxy(ctx, decision, prep, proxyWriter, r)
	case providers.ProviderOpenAI, providers.ProviderOpenRouter, providers.ProviderFireworks:
		crossFormat = true
		prep, emitErr := env.PrepareOpenAI(r.Header, opts)
		if emitErr != nil {
			log.Error("Failed to translate Anthropic request to OpenAI format", "err", emitErr, "decision_provider", decision.Provider)
			return fmt.Errorf("translate anthropic request: %w", emitErr)
		}
		var usage otel.UsageSink
		if s.usageRequired() {
			extractor = otel.NewUsageExtractor(nil, decision.Provider)
			usage = extractor
		}
		translator := translate.NewAnthropicSSETranslator(sink, decision.Model, usage).
			WithRoutingMarker(routingMarkerFor(routeRes)).
			WithClosingMarker(closingMarkerFor(decision, feats.Model, s.baselineFor(feats.Model)))
		proxyErr = p.Proxy(ctx, decision, prep, translator, r)
		proxyErr = finalizeAfterProxy(proxyErr, translator.Finalize)
	case providers.ProviderGoogle:
		crossFormat = true
		prep, emitErr := env.PrepareGemini(r.Header, opts)
		if emitErr != nil {
			log.Error("Failed to translate Anthropic request to Gemini format", "err", emitErr)
			return fmt.Errorf("translate anthropic request to gemini: %w", emitErr)
		}
		var usage otel.UsageSink
		if s.usageRequired() {
			extractor = otel.NewUsageExtractor(nil, decision.Provider)
			usage = extractor
		}
		// SSE chain: Gemini → OpenAI → Anthropic.
		anthropicTr := translate.NewAnthropicSSETranslator(sink, decision.Model, usage).
			WithRoutingMarker(routingMarkerFor(routeRes)).
			WithClosingMarker(closingMarkerFor(decision, feats.Model, s.baselineFor(feats.Model)))
		geminiTr := translate.NewGeminiToOpenAISSETranslator(anthropicTr, decision.Model, nil)
		proxyErr = p.Proxy(ctx, decision, prep, geminiTr, r)
		proxyErr = finalizeAfterProxy(proxyErr, geminiTr.Finalize)
		proxyErr = finalizeAfterProxy(proxyErr, anthropicTr.Finalize)
	default:
		return fmt.Errorf("%w: %s (no translation path defined for inbound Anthropic Messages)", ErrProviderNotConfigured, decision.Provider)
	}

	// Cache store: only on success when body fits. Any top-p cluster id
	// works for storage since LRU.Lookup scans all of them.
	if cacheEligible && proxyErr == nil && captureW != nil {
		if body, status, ok := captureW.captured(); ok && status == http.StatusOK {
			storeResp := cache.CachedResponse{
				StatusCode: status,
				Headers:    cloneCacheHeaders(w.Header()),
				Body:       body,
			}
			s.semanticCache.Store(externalID, cache.FormatAnthropic, decision.Metadata.Embedding, decision.Metadata.ClusterIDs[0], storeResp)
		}
	}

	proxyMs := time.Since(proxyStart).Milliseconds()

	in, out := extractor.Tokens()
	cacheCreation, cacheRead := extractor.CacheTokens()
	upstreamBuilder := otel.NewAttrBuilder(40).
		String("request_id", requestID).
		String("external_id", externalID).
		String("router_user_id", auth.UserIDFrom(ctx)).
		String("client.device_id", clientID.DeviceID).
		String("client.account_id", clientID.AccountID).
		String("client.session_id", clientID.SessionID).
		String("client.user_agent", clientID.UserAgent).
		String("client.app", clientID.ClientApp).
		Int64("usage.input_tokens", int64(in)).
		Int64("usage.output_tokens", int64(out)).
		Int64("usage.cache_creation_input_tokens", int64(cacheCreation)).
		Int64("usage.cache_read_input_tokens", int64(cacheRead)).
		Float64("cost.requested_input_usd", float64(in)/1_000_000*reqPricing.InputUSDPer1M).
		Float64("cost.requested_output_usd", float64(out)/1_000_000*reqPricing.OutputUSDPer1M).
		Float64("cost.actual_input_usd", float64(in)/1_000_000*actPricing.InputUSDPer1M).
		Float64("cost.actual_output_usd", float64(out)/1_000_000*actPricing.OutputUSDPer1M).
		Int64("latency.upstream_ms", proxyMs).
		Int64("latency.total_ms", time.Since(requestStart).Milliseconds()).
		Int64("upstream.status_code", int64(upstreamStatus(proxyErr))).
		Bool("routing.cross_format", crossFormat)
	applyPlannerAttrs(upstreamBuilder, routeRes)
	addTimingAttrs(ctx, upstreamBuilder)

	// Span attrs and telemetry row share the same source; bundle keeps them symmetric.
	obs := buildObservationContext(ctx, decision)
	obs.applySpanAttrs(upstreamBuilder)

	otel.Record(ctx, otel.Span{
		Name:  "router.upstream",
		Start: proxyStart,
		End:   time.Now(),
		Attrs: upstreamBuilder.Build(),
	})
	otel.Flush(ctx)

	// Persist last-turn usage to the pin row so the next turn's planner
	// has cache-hit evidence. Off the request path; drops on saturation.
	s.recordTurnUsage(routeRes, in, out, cacheCreation, cacheRead)

	if installationID != uuid.Nil {
		s.fireTelemetry(InsertTelemetryParams{
			InstallationID:         installationID.String(),
			RequestID:              requestID,
			SpanType:               "router.upstream",
			TraceID:                requestID,
			Timestamp:              requestStart,
			RequestedModel:         feats.Model,
			DecisionModel:          decision.Model,
			DecisionProvider:       decision.Provider,
			DecisionReason:         decision.Reason,
			EstimatedInputTokens:   int32(feats.Tokens),
			StickyHit:              stickyHit,
			EmbedInput:             embedInput,
			InputTokens:            int32(in),
			OutputTokens:           int32(out),
			RequestedInputCostUSD:  float64(in) / 1_000_000 * reqPricing.InputUSDPer1M,
			RequestedOutputCostUSD: float64(out) / 1_000_000 * reqPricing.OutputUSDPer1M,
			ActualInputCostUSD:     float64(in) / 1_000_000 * actPricing.InputUSDPer1M,
			ActualOutputCostUSD:    float64(out) / 1_000_000 * actPricing.OutputUSDPer1M,
			RouteLatencyMs:         routeMs,
			UpstreamLatencyMs:      proxyMs,
			TotalLatencyMs:         time.Since(requestStart).Milliseconds(),
			CrossFormat:            crossFormat,
			UpstreamStatusCode:     int32(upstreamStatus(proxyErr)),
			ClusterIDs:             obs.ClusterIDs,
			CandidateModels:        obs.CandidateModels,
			ChosenScore:            obs.ChosenScore,
			ClusterRouterVersion:   obs.ClusterRouterVersion,
			TTFTMs:                 obs.TTFTMs,
			DeviceID:               clientID.DeviceID,
			SessionID:              clientID.SessionID,
		})
	}

	log.Info("ProxyMessages complete", "requested_model", feats.Model, "baseline_model", s.baselineFor(feats.Model), "decision_model", decision.Model, "decision_provider", decision.Provider, "decision_reason", decision.Reason, "requested_tier", routeRes.RequestedTier.String(), "decision_tier", capability.TierFor(decision.Model).String(), "tier_clamped", routeRes.TierClamped, "pre_clamp_model", routeRes.PreClampModel, "estimated_input_tokens", feats.Tokens, "has_tools", feats.HasTools, "message_count", feats.MessageCount, "last_kind", feats.LastKind, "last_preview", feats.LastPreview, "embed_input", embedInput, "cross_format", crossFormat, "sticky_hit", stickyHit, "route_ms", routeMs, "proxy_ms", proxyMs, "proxy_err", proxyErr, "upstream_status", upstreamStatus(proxyErr))
	return proxyErr
}

// sessionPinCacheKey produces the in-proc LRU key for a (session_key, role)
// pair. Hex-encoded for the string-keyed LRU; role suffix preserves the
// schema's role dimension for non-default roles.
func sessionPinCacheKey(key [sessionpin.SessionKeyLen]byte, role string) string {
	return hex.EncodeToString(key[:]) + ":" + role
}

// applyPlannerAttrs stamps planner and handover attributes onto a span
// attribute builder. Safe to call when the planner didn't run; uses
// "skipped" for the outcome and zero values for the EV fields so the
// schema stays uniform across hard-pin / tool-result / planner-disabled
// paths.
func applyPlannerAttrs(b *otel.AttrBuilder, res turnLoopResult) *otel.AttrBuilder {
	outcome := plannerOutcomeAttr(res)
	b.String("planner.outcome", outcome).
		String("planner.reason", res.PlannerDecision.Reason).
		Float64("planner.expected_savings_usd", res.PlannerDecision.ExpectedSavingsUSD).
		Float64("planner.eviction_cost_usd", res.PlannerDecision.EvictionCostUSD).
		Float64("planner.threshold_usd", res.PlannerDecision.ThresholdUSD).
		String("planner.pin_model", res.PinModel).
		String("planner.fresh_model", res.Fresh.Model).
		String("planner.chosen_model", res.Decision.Model).
		Bool("handover.invoked", res.Handover.Invoked).
		Int64("handover.latency_ms", res.Handover.LatencyMS).
		Int64("handover.summary_tokens", int64(res.Handover.SummaryTokens)).
		Bool("handover.fallback_to_trim", res.Handover.FallbackToTrim)
	return b
}

// plannerOutcomeAttr maps the planner's typed outcome to the string used
// in OTel attrs. "skipped" covers every path where Decide was not
// invoked (hard-pin, tool-result short-circuit, planner-disabled,
// pin-store-disabled).
func plannerOutcomeAttr(res turnLoopResult) string {
	if res.PlannerDecision.Reason == "" {
		return "skipped"
	}
	switch res.PlannerDecision.Outcome {
	case planner.OutcomeStay:
		return "stay"
	case planner.OutcomeSwitch:
		return "switch"
	default:
		return "skipped"
	}
}

// logPlannerOutcome emits a single structured log line summarizing the
// planner's verdict. Switch turns are Info (rare, decision-affecting);
// stay turns are Debug so the steady-state hot path stays quiet.
func (s *Service) logPlannerOutcome(res turnLoopResult) {
	if res.PlannerDecision.Reason == "" {
		return
	}
	log := observability.Get()
	if res.PlannerDecision.Outcome == planner.OutcomeSwitch {
		log.Info("router switched models",
			"from", res.PinModel,
			"to", res.Decision.Model,
			"reason", res.PlannerDecision.Reason,
			"expected_savings_usd", res.PlannerDecision.ExpectedSavingsUSD,
			"eviction_cost_usd", res.PlannerDecision.EvictionCostUSD,
			"threshold_usd", res.PlannerDecision.ThresholdUSD,
			"handover_invoked", res.Handover.Invoked,
			"handover_fallback_to_trim", res.Handover.FallbackToTrim,
			"handover_latency_ms", res.Handover.LatencyMS,
		)
		return
	}
	log.Debug("router stayed on pinned model",
		"model", res.Decision.Model,
		"reason", res.PlannerDecision.Reason,
		"expected_savings_usd", res.PlannerDecision.ExpectedSavingsUSD,
		"eviction_cost_usd", res.PlannerDecision.EvictionCostUSD,
		"threshold_usd", res.PlannerDecision.ThresholdUSD,
	)
}

// recordTurnUsage writes the upstream's observed input/output/cache
// tokens back to the session pin row. The planner reads these on the
// next turn to weigh switch EV against eviction cost.
//
// Bounded async; drops on saturation. Guarded against hard-pin turns
// (no pin row to update) and missing session keys (e.g. pin store
// disabled). Uses context.Background() — per CLAUDE.md, deferred DB
// writes must outlive the request ctx.
func (s *Service) recordTurnUsage(res turnLoopResult, in, out, cacheCreation, cacheRead int) {
	if s.pinStore == nil || res.HardPinned {
		return
	}
	var zeroKey [sessionpin.SessionKeyLen]byte
	if res.SessionKey == zeroKey {
		return
	}
	if in == 0 && out == 0 && cacheCreation == 0 && cacheRead == 0 {
		return
	}
	usage := sessionpin.Usage{
		InputTokens:       in,
		CachedReadTokens:  cacheRead,
		CachedWriteTokens: cacheCreation,
		OutputTokens:      out,
		EndedAt:           time.Now(),
	}
	key := res.SessionKey
	role := res.PinRole
	if role == "" {
		role = sessionpin.DefaultRole
	}

	// Keep the in-proc LRU coherent with the DB writeback. Without this,
	// loadPin's Tier-1 hit serves a stale pin with zero usage and the
	// planner returns ReasonNoPriorUsage forever (the 30s LRU TTL keeps
	// resetting under typical agentic turn cadence), which silently
	// disables EV-based switching for all active sessions.
	if s.pinCache != nil {
		pinCacheKey := sessionPinCacheKey(key, role)
		if pin, ok := s.pinCache.Get(pinCacheKey); ok {
			pin.LastInputTokens = usage.InputTokens
			pin.LastCachedReadTokens = usage.CachedReadTokens
			pin.LastCachedWriteTokens = usage.CachedWriteTokens
			pin.LastOutputTokens = usage.OutputTokens
			pin.LastTurnEndedAt = usage.EndedAt
			s.pinCache.Add(pinCacheKey, pin)
		}
	}

	select {
	case s.usageWriteSem <- struct{}{}:
		go func() {
			defer func() { <-s.usageWriteSem }()
			if err := s.pinStore.UpdateUsage(context.Background(), key, role, usage); err != nil {
				observability.Get().Debug("session pin usage writeback failed", "err", err)
			}
		}()
	default:
		observability.Get().Debug("session pin usage writeback dropped: semaphore full")
	}
}

// pinDecision rehydrates a router.Decision from a stored pin. Metadata is
// nil intentionally — the embedding isn't persisted, so semantic-cache won't
// fire on a pinned turn (acceptable: the pin already short-circuits routing).
func pinDecision(p sessionpin.Pin) router.Decision {
	return router.Decision{
		Provider: p.Provider,
		Model:    p.Model,
		Reason:   p.Reason,
	}
}

// clusterIDsFromDecision returns the cluster ids on a decision, or nil for
// decisions without metadata.
func clusterIDsFromDecision(d router.Decision) []int {
	if d.Metadata == nil {
		return nil
	}
	return d.Metadata.ClusterIDs
}

// pinAge returns seconds since first_pinned_at, or zero for fresh pins.
func pinAge(p sessionpin.Pin) int64 {
	if p.FirstPinnedAt.IsZero() {
		return 0
	}
	d := time.Since(p.FirstPinnedAt)
	if d < 0 {
		return 0
	}
	return int64(d.Seconds())
}

// hasEvalOverrideHeader reports whether the request carries any eval-harness override headers.
func hasEvalOverrideHeader(r *http.Request) bool {
	if r == nil {
		return false
	}
	return r.Header.Get("x-weave-cluster-version") != "" ||
		r.Header.Get("x-weave-embed-only-user-message") != ""
}

// externalKeysFromContext reads external API keys stashed by auth middleware.
func externalKeysFromContext(ctx context.Context) []*auth.ExternalAPIKey {
	v := ctx.Value(ExternalAPIKeysContextKey{})
	if v == nil {
		return nil
	}
	keys, _ := v.([]*auth.ExternalAPIKey)
	return keys
}

// requestUsesNonDeploymentCreds reports whether the inbound request's
// provider credentials are NOT the platform's deployment-level env keys.
// The handover summarizer is wired at boot with deployment-level
// credentials; calling it on a request whose upstream call would use
// BYOK or client-supplied creds would route prior conversation context
// (preserved by the summarizer's handover instruction) through the
// platform account, violating tenant data boundaries. The orchestrator
// uses this to skip the summarizer and fall through to TrimLastN.
func (s *Service) requestUsesNonDeploymentCreds(ctx context.Context, headers http.Header) bool {
	if s.byokOnly {
		return true
	}
	if len(externalKeysFromContext(ctx)) > 0 {
		return true
	}
	for _, p := range []string{
		providers.ProviderAnthropic,
		providers.ProviderOpenAI,
		providers.ProviderGoogle,
		providers.ProviderOpenRouter,
		providers.ProviderFireworks,
	} {
		if ExtractClientCredentials(p, headers) != nil {
			return true
		}
	}
	return false
}

// enabledProvidersForRequest returns providers whose credentials are
// resolvable for this request (boot-time env key, BYOK, or client-supplied
// header). The cluster scorer intersects this set with its boot-time
// candidates so argmax never picks a model the upstream call would 401 on.
//
// surfaceProvider is the inbound wire-format's natural provider (anthropic
// for /v1/messages, openai for /v1/chat/completions, google for the Gemini
// surface). A client-supplied bearer/x-api-key header is treated as creds
// for that surface only — never as a licence to enable other OpenAI-compat
// upstreams that happen to read the same Authorization header. This matches
// the guard already in resolveAndInjectCredentials: a router-key-authed
// request must rely on BYOK; a header on such a request is for the inbound
// surface only and must not enable cross-provider routing.
func (s *Service) enabledProvidersForRequest(ctx context.Context, surfaceProvider string, headers http.Header) map[string]struct{} {
	out := make(map[string]struct{}, len(s.providers))
	// In BYOK-only mode the deployment-level env key must not make a provider
	// eligible — otherwise argmax could pick it and 401 with the platform key.
	if !s.byokOnly {
		// Prefer the explicit env-keyed set when configured (selfhosted mode
		// with BYOK-routable providers registered but unkeyed). Fall back to
		// "every registered provider" for legacy callers that don't set it.
		if s.deploymentKeyedProviders != nil {
			for p := range s.deploymentKeyedProviders {
				out[p] = struct{}{}
			}
		} else {
			for p := range s.providers {
				out[p] = struct{}{}
			}
		}
	}
	for _, k := range externalKeysFromContext(ctx) {
		// Empty plaintext (decryption produced no bytes, or a stale row was
		// written without a value) must not enroll the provider — argmax
		// would pick it and the upstream call would 401 with no auth header.
		if len(k.Plaintext) == 0 {
			continue
		}
		out[k.Provider] = struct{}{}
	}
	// Client-supplied headers are only consulted when the request is NOT
	// authed via a router key. A router-key-authed request that happens to
	// carry an inbound bearer (e.g. Claude Code's Anthropic OAuth token
	// passing through) must not enable OpenAI-compat upstreams just because
	// they share the Authorization: Bearer header format.
	if installationIDFromContext(ctx) == (uuid.UUID{}) && surfaceProvider != "" {
		if _, already := out[surfaceProvider]; !already {
			if ExtractClientCredentials(surfaceProvider, headers) != nil {
				out[surfaceProvider] = struct{}{}
			}
		}
	}
	return out
}

// resolveAndInjectCredentials resolves credentials for provider and stashes
// them on ctx. Returns ctx unchanged when none are available.
//
// When the request was authenticated via a router key (installation ID present
// on ctx), client-header extraction is skipped. This prevents the client's
// inbound Anthropic key (Authorization: Bearer sk-ant-...) from being
// mistakenly forwarded as credentials to a different upstream provider
// (OpenRouter, Fireworks, etc.). In that case the deployment-level env key
// on the provider client is the correct fallback.
func resolveAndInjectCredentials(ctx context.Context, provider string, headers http.Header) context.Context {
	byok := BuildCredentialsMap(externalKeysFromContext(ctx))
	var creds *Credentials
	if byok != nil {
		creds = byok[provider]
	}
	if creds == nil && installationIDFromContext(ctx) == (uuid.UUID{}) {
		// No router-key auth — allow client-supplied bearer as provider creds
		// (plan-based / dev-mode passthrough scenario).
		creds = ExtractClientCredentials(provider, headers)
	}
	if creds != nil {
		return context.WithValue(ctx, CredentialsContextKey{}, creds)
	}
	return ctx
}

// addTimingAttrs appends derived latency attributes from the request Timing.
// No-op when no Timing is attached.
func addTimingAttrs(ctx context.Context, b *otel.AttrBuilder) {
	t := otel.TimingFrom(ctx)
	if t == nil {
		return
	}
	upstreamTotal := t.Ms(&t.UpstreamRequestNanos, &t.UpstreamEOFNanos)
	fullE2E := t.MsSince(&t.EntryNanos)

	var overhead int64
	if upstreamTotal > 0 {
		overhead = fullE2E - upstreamTotal
	}

	b.Int64("latency.full_e2e_ms", fullE2E).
		Int64("latency.preupstream_ms", t.Ms(&t.EntryNanos, &t.UpstreamRequestNanos)).
		Int64("latency.upstream_headers_ms", t.Ms(&t.UpstreamRequestNanos, &t.UpstreamHeadersNanos)).
		Int64("latency.upstream_first_byte_ms", t.Ms(&t.UpstreamRequestNanos, &t.UpstreamFirstByteNanos)).
		Int64("latency.upstream_total_ms", upstreamTotal).
		Int64("latency.postupstream_ms", t.MsSince(&t.UpstreamEOFNanos)).
		Int64("latency.router_overhead_ms", overhead)
}

// fireTelemetry persists a telemetry row asynchronously. Telemetry loss is acceptable.
func (s *Service) fireTelemetry(p InsertTelemetryParams) {
	if s.telemetry == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.telemetry.InsertRequestTelemetry(ctx, p); err != nil {
			observability.Get().Debug("Telemetry insert failed", "err", err, "request_id", p.RequestID)
		}
	}()
}

// upstreamStatus extracts the HTTP status from an UpstreamStatusError, or 0.
func upstreamStatus(err error) int {
	var e *providers.UpstreamStatusError
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// finalizeAfterProxy runs a translator's Finalize step when the upstream call
// either succeeded or returned a typed UpstreamStatusError. Cross-format
// translators buffer the upstream body for non-streaming responses and only
// flush it inside Finalize; skipping that on a 4xx/5xx drops the upstream
// error envelope (e.g. an OpenRouter 402 "out of credits" message) before it
// can reach the client. UpstreamStatusError takes precedence over a Finalize
// error so telemetry preserves the upstream status code.
func finalizeAfterProxy(proxyErr error, fn func() error) error {
	var statusErr *providers.UpstreamStatusError
	isStatus := errors.As(proxyErr, &statusErr)
	if proxyErr != nil && !isStatus {
		return proxyErr
	}
	finErr := fn()
	if isStatus {
		return proxyErr
	}
	return finErr
}

// ProxyOpenAIChatCompletion routes an OpenAI Chat Completion request,
// translating cross-format when the decision picks a non-OpenAI provider.
func (s *Service) ProxyOpenAIChatCompletion(ctx context.Context, body []byte, w http.ResponseWriter, r *http.Request) error {
	log := observability.Get()
	requestStart := time.Now()
	requestID := uuid.New().String()
	buf := otel.NewBuffer(s.emitter)
	ctx = buf.WithContext(ctx)

	apiKeyID, _ := ctx.Value(APIKeyIDContextKey{}).(string)
	externalID, _ := ctx.Value(ExternalIDContextKey{}).(string)
	installationID := installationIDFromContext(ctx)
	clientID := ClientIdentityFrom(ctx)

	env, parseErr := translate.ParseOpenAI(body)
	if parseErr != nil {
		log.Error("Failed to parse OpenAI request", "err", parseErr)
		return fmt.Errorf("parse request: %w", parseErr)
	}
	embedFlag := s.embedOnlyUserMessage
	if v, ok := embedOnlyUserMessageOverride(ctx); ok {
		embedFlag = v
	}
	feats := env.RoutingFeatures(embedFlag)
	promptText := feats.PromptText
	embedInput := "concatenated_stream"
	if embedFlag && feats.OnlyUserMessageText != "" {
		promptText = feats.OnlyUserMessageText
		embedInput = "only_user_message"
	}

	bypassEval := hasEvalOverrideHeader(r)

	// OpenAI signals sub-agent identity via x-weave-subagent-type (no metadata.user_id).
	subAgentHint := r.Header.Get("x-weave-subagent-type")

	routeStart := time.Now()
	routeRes, err := s.runTurnLoop(ctx, env, feats, apiKeyID, installationID, subAgentHint, r.Header, router.Request{
		RequestedModel:       feats.Model,
		EstimatedInputTokens: feats.Tokens,
		HasTools:             feats.HasTools,
		PromptText:           promptText,
		EnabledProviders:     s.enabledProvidersForRequest(ctx, providers.ProviderOpenAI, r.Header),
		ExcludedModels:       s.excludedModelsForRequest(ctx),
	})
	routeMs := time.Since(routeStart).Milliseconds()
	if err != nil {
		log.Error("Routing failed for OpenAI request", "err", err, "route_ms", routeMs, "requested_model", feats.Model, "estimated_input_tokens", feats.Tokens)
		return err
	}
	decision := routeRes.Decision
	tt := routeRes.TurnType
	stickyHit := routeRes.StickyHit
	pinTier := routeRes.PinTier
	pinAgeSec := routeRes.PinAgeSec
	s.logPlannerOutcome(routeRes)

	// Same eligibility rules as ProxyMessages. FormatOpenAI keeps replays
	// scoped: an Anthropic-stored response is never served here.
	cacheEligible := s.semanticCache != nil && !env.Stream() && decision.Metadata != nil && externalID != "" && !bypassEval
	if cacheEligible {
		if resp, hit := s.semanticCache.Lookup(externalID, cache.FormatOpenAI, decision.Metadata.Embedding, decision.Metadata.ClusterIDs); hit {
			s.writeCachedResponse(w, resp, decision)
			otel.Record(ctx, otel.Span{
				Name:  "router.cache_hit",
				Start: requestStart,
				End:   time.Now(),
				Attrs: otel.NewAttrBuilder(7).
					String("request_id", requestID).
					String("external_id", externalID).
					String("decision.model", decision.Model).
					String("decision.provider", decision.Provider).
					Bool("cache.hit", true).
					String("cache.format", string(cache.FormatOpenAI)).
					Int64("latency.total_ms", time.Since(requestStart).Milliseconds()).
					Build(),
			})
			otel.Flush(ctx)
			log.Info("ProxyOpenAIChatCompletion cache hit", "requested_model", feats.Model, "baseline_model", s.baselineFor(feats.Model), "decision_model", decision.Model, "decision_provider", decision.Provider, "external_id", externalID, "total_ms", time.Since(requestStart).Milliseconds())
			return nil
		}
	}

	p, provErr := s.provider(decision.Provider)
	if provErr != nil {
		return provErr
	}

	w.Header().Set("x-router-decision", decision.Reason)
	w.Header().Set("x-router-provider", decision.Provider)
	w.Header().Set("x-router-model", decision.Model)

	reqPricing := otel.Lookup(s.baselineFor(feats.Model))
	actPricing := otel.Lookup(decision.Model)
	openaiDecisionBuilder := otel.NewAttrBuilder(40).
		String("request_id", requestID).
		String("external_id", externalID).
		String("router_user_id", auth.UserIDFrom(ctx)).
		String("client.device_id", clientID.DeviceID).
		String("client.account_id", clientID.AccountID).
		String("client.session_id", clientID.SessionID).
		String("client.user_agent", clientID.UserAgent).
		String("client.app", clientID.ClientApp).
		String("requested.model", feats.Model).
		String("decision.model", decision.Model).
		String("decision.provider", decision.Provider).
		String("decision.reason", decision.Reason).
		Bool("routing.sticky_hit", stickyHit).
		Bool("routing.session_pin_hit", pinTier == "in_proc" || pinTier == "postgres").
		String("routing.session_pin_tier", pinTier).
		Int64("routing.session_pin_age_s", pinAgeSec).
		String("routing.turn_type", string(tt)).
		String("routing.embed_input", embedInput).
		Int64("routing.estimated_input_tokens", int64(feats.Tokens)).
		IntSlice("routing.cluster_ids", clusterIDsFromDecision(decision)).
		Float64("pricing.requested_input_per_1m", reqPricing.InputUSDPer1M).
		Float64("pricing.requested_output_per_1m", reqPricing.OutputUSDPer1M).
		Float64("pricing.actual_input_per_1m", actPricing.InputUSDPer1M).
		Float64("pricing.actual_output_per_1m", actPricing.OutputUSDPer1M).
		Int64("latency.route_ms", routeMs)
	applyPlannerAttrs(openaiDecisionBuilder, routeRes)
	otel.Record(ctx, otel.Span{
		Name:  "router.decision",
		Start: requestStart,
		End:   time.Now(),
		Attrs: openaiDecisionBuilder.Build(),
	})
	otel.Flush(ctx)

	opts := translate.EmitOptions{
		TargetModel:        decision.Model,
		Capabilities:       router.Lookup(decision.Model),
		IncludeStreamUsage: s.usageRequired(),
	}

	ctx = resolveAndInjectCredentials(ctx, decision.Provider, r.Header)

	// Mirror post-translation wire bytes into a buffer for post-Proxy storage.
	var captureW *captureWriter
	var sink http.ResponseWriter = w
	if cacheEligible {
		captureW = newCaptureWriter(w, semanticCacheMaxBodyBytes)
		sink = captureW
	}

	proxyStart := time.Now()
	var proxyErr error
	crossFormat := false
	var extractor *otel.UsageExtractor

	switch decision.Provider {
	case providers.ProviderOpenAI, providers.ProviderOpenRouter, providers.ProviderFireworks:
		prep, emitErr := env.PrepareOpenAI(r.Header, opts)
		if emitErr != nil {
			log.Error("Failed to emit OpenAI body", "err", emitErr)
			return fmt.Errorf("emit body: %w", emitErr)
		}
		proxyWriter := sink
		if s.usageRequired() {
			extractor = otel.NewUsageExtractor(sink, decision.Provider)
			proxyWriter = extractor
		}
		proxyErr = p.Proxy(ctx, decision, prep, proxyWriter, r)
	case providers.ProviderGoogle:
		crossFormat = true
		prep, emitErr := env.PrepareGemini(r.Header, opts)
		if emitErr != nil {
			log.Error("Failed to translate OpenAI request to Gemini format", "err", emitErr)
			return fmt.Errorf("translate openai request to gemini: %w", emitErr)
		}
		var usage otel.UsageSink
		if s.usageRequired() {
			extractor = otel.NewUsageExtractor(nil, decision.Provider)
			usage = extractor
		}
		translator := translate.NewGeminiToOpenAISSETranslator(sink, decision.Model, usage)
		proxyErr = p.Proxy(ctx, decision, prep, translator, r)
		proxyErr = finalizeAfterProxy(proxyErr, translator.Finalize)
	case providers.ProviderAnthropic:
		crossFormat = true
		prep, emitErr := env.PrepareAnthropic(r.Header, opts)
		if emitErr != nil {
			log.Error("Failed to translate OpenAI request to Anthropic format", "err", emitErr)
			return fmt.Errorf("translate openai request: %w", emitErr)
		}
		var usage otel.UsageSink
		if s.usageRequired() {
			extractor = otel.NewUsageExtractor(nil, providers.ProviderAnthropic)
			usage = extractor
		}
		translator := translate.NewSSETranslator(sink, decision.Model, usage)
		proxyErr = p.Proxy(ctx, decision, prep, translator, r)
		proxyErr = finalizeAfterProxy(proxyErr, translator.Finalize)
	default:
		return fmt.Errorf("%w: %s (no translation path defined)", ErrProviderNotConfigured, decision.Provider)
	}

	if cacheEligible && proxyErr == nil && captureW != nil {
		if body, status, ok := captureW.captured(); ok && status == http.StatusOK {
			storeResp := cache.CachedResponse{
				StatusCode: status,
				Headers:    cloneCacheHeaders(w.Header()),
				Body:       body,
			}
			s.semanticCache.Store(externalID, cache.FormatOpenAI, decision.Metadata.Embedding, decision.Metadata.ClusterIDs[0], storeResp)
		}
	}

	proxyMs := time.Since(proxyStart).Milliseconds()

	in, out := extractor.Tokens()
	cacheCreation, cacheRead := extractor.CacheTokens()
	openaiUpstreamBuilder := otel.NewAttrBuilder(40).
		String("request_id", requestID).
		String("external_id", externalID).
		String("router_user_id", auth.UserIDFrom(ctx)).
		String("client.device_id", clientID.DeviceID).
		String("client.account_id", clientID.AccountID).
		String("client.session_id", clientID.SessionID).
		String("client.user_agent", clientID.UserAgent).
		String("client.app", clientID.ClientApp).
		Int64("usage.input_tokens", int64(in)).
		Int64("usage.output_tokens", int64(out)).
		Int64("usage.cache_creation_input_tokens", int64(cacheCreation)).
		Int64("usage.cache_read_input_tokens", int64(cacheRead)).
		Float64("cost.requested_input_usd", float64(in)/1_000_000*reqPricing.InputUSDPer1M).
		Float64("cost.requested_output_usd", float64(out)/1_000_000*reqPricing.OutputUSDPer1M).
		Float64("cost.actual_input_usd", float64(in)/1_000_000*actPricing.InputUSDPer1M).
		Float64("cost.actual_output_usd", float64(out)/1_000_000*actPricing.OutputUSDPer1M).
		Int64("latency.upstream_ms", proxyMs).
		Int64("latency.total_ms", time.Since(requestStart).Milliseconds()).
		Int64("upstream.status_code", int64(upstreamStatus(proxyErr))).
		Bool("routing.cross_format", crossFormat)
	applyPlannerAttrs(openaiUpstreamBuilder, routeRes)
	addTimingAttrs(ctx, openaiUpstreamBuilder)

	// Shared bundle keeps the OpenAI path in lockstep with ProxyMessages.
	openaiObs := buildObservationContext(ctx, decision)
	openaiObs.applySpanAttrs(openaiUpstreamBuilder)

	otel.Record(ctx, otel.Span{
		Name:  "router.upstream",
		Start: proxyStart,
		End:   time.Now(),
		Attrs: openaiUpstreamBuilder.Build(),
	})
	otel.Flush(ctx)

	// Persist last-turn usage to the pin row so the next turn's planner
	// has cache-hit evidence. Off the request path; drops on saturation.
	s.recordTurnUsage(routeRes, in, out, cacheCreation, cacheRead)

	installationIDOAI, _ := ctx.Value(InstallationIDContextKey{}).(string)
	if installationIDOAI != "" {
		s.fireTelemetry(InsertTelemetryParams{
			InstallationID:         installationIDOAI,
			RequestID:              requestID,
			SpanType:               "router.upstream",
			TraceID:                requestID,
			Timestamp:              requestStart,
			RequestedModel:         feats.Model,
			DecisionModel:          decision.Model,
			DecisionProvider:       decision.Provider,
			DecisionReason:         decision.Reason,
			EstimatedInputTokens:   int32(feats.Tokens),
			StickyHit:              stickyHit,
			EmbedInput:             embedInput,
			InputTokens:            int32(in),
			OutputTokens:           int32(out),
			RequestedInputCostUSD:  float64(in) / 1_000_000 * reqPricing.InputUSDPer1M,
			RequestedOutputCostUSD: float64(out) / 1_000_000 * reqPricing.OutputUSDPer1M,
			ActualInputCostUSD:     float64(in) / 1_000_000 * actPricing.InputUSDPer1M,
			ActualOutputCostUSD:    float64(out) / 1_000_000 * actPricing.OutputUSDPer1M,
			RouteLatencyMs:         routeMs,
			UpstreamLatencyMs:      proxyMs,
			TotalLatencyMs:         time.Since(requestStart).Milliseconds(),
			CrossFormat:            crossFormat,
			UpstreamStatusCode:     int32(upstreamStatus(proxyErr)),
			ClusterIDs:             openaiObs.ClusterIDs,
			CandidateModels:        openaiObs.CandidateModels,
			ChosenScore:            openaiObs.ChosenScore,
			ClusterRouterVersion:   openaiObs.ClusterRouterVersion,
			TTFTMs:                 openaiObs.TTFTMs,
			DeviceID:               clientID.DeviceID,
			SessionID:              clientID.SessionID,
		})
	}

	log.Info("ProxyOpenAIChatCompletion complete", "requested_model", feats.Model, "baseline_model", s.baselineFor(feats.Model), "decision_model", decision.Model, "decision_provider", decision.Provider, "decision_reason", decision.Reason, "requested_tier", routeRes.RequestedTier.String(), "decision_tier", capability.TierFor(decision.Model).String(), "tier_clamped", routeRes.TierClamped, "pre_clamp_model", routeRes.PreClampModel, "estimated_input_tokens", feats.Tokens, "has_tools", feats.HasTools, "embed_input", embedInput, "cross_format", crossFormat, "sticky_hit", stickyHit, "pin_tier", pinTier, "turn_type", string(tt), "route_ms", routeMs, "proxy_ms", proxyMs, "proxy_err", proxyErr, "upstream_status", upstreamStatus(proxyErr))
	return proxyErr
}
