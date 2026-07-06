package proxy

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/billing"
	"workweave/router/internal/feedback"
	"workweave/router/internal/observability"
	"workweave/router/internal/observability/otel"
	"workweave/router/internal/providers"
	"workweave/router/internal/proxy/usage"
	"workweave/router/internal/router"
	"workweave/router/internal/router/bandit"
	"workweave/router/internal/router/bandswap"
	"workweave/router/internal/router/cache"
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/router/handover"
	"workweave/router/internal/router/hmm"
	"workweave/router/internal/router/planner"
	"workweave/router/internal/router/rl"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/router/turntype"
	"workweave/router/internal/timing"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
	"github.com/tidwall/sjson"
)

// TelemetryEmitter is the narrow interface proxy owns for OTel: a
// request-scoped span/log buffer. Implemented by *otel.Emitter.
type TelemetryEmitter interface {
	// NewBuffer returns a request-scoped span/log buffer, or nil when the
	// emitter itself is disabled.
	NewBuffer() *otel.Buffer
}

// Service orchestrates routing decisions and provider dispatch.
type Service struct {
	router router.Router
	// rlRouter is the opt-in RL/DPO policy router, selected per-request via the
	// x-weave-router-strategy: rl header. Nil when no policy sidecar is wired
	// (ROUTER_RL_SIDECAR_URL unset); the strategy header then 503s rather than
	// silently serving the cluster scorer.
	rlRouter router.Router
	// hmmRouter is the opt-in policy router selected per-request via
	// x-weave-router-strategy: hmm.
	hmmRouter          router.Router
	hmmOutcomeReporter hmm.OutcomeReporter
	// banditRouter is the opt-in Thompson-sampling router, selected per-request
	// via x-weave-router-strategy: bandit. Nil when ROUTER_BANDIT_POSTERIOR_FILE
	// is unset at boot; the strategy header then 503s.
	banditRouter         router.Router
	providers            map[string]providers.Client
	emitter              TelemetryEmitter
	embedOnlyUserMessage bool
	semanticCache        *cache.Cache
	// pinStore persists session-sticky routing decisions. Nil when the feature
	// flag is off; the orchestrator then runs the scorer every turn.
	pinStore sessionpin.Store
	// noProgress tracks per-session dispatch fingerprints to catch the
	// cross-envelope subagent loop (parent agent re-spawning identical
	// sub-conversations). Nil disables the detector.
	noProgress *noProgressTracker
	// compaction detects Claude Code context compaction events (message count
	// drops) so the router can rewrite non-Anthropic requests with a handover
	// summary before the model loses awareness of prior completed work.
	compaction *compactionTracker
	// prefixTrimFreeSwitch treats a detected client history trim as a
	// free-switch window: the planner prices the pin's cache as cold on that
	// turn and the switch handover is skipped. Kill switch:
	// ROUTER_PREFIX_TRIM_FREE_SWITCH.
	prefixTrimFreeSwitch bool
	// escapeNormalize enables the escape-repair pass on file-edit tool
	// (Edit/Write/MultiEdit) args for cross-format OpenAI-upstream responses.
	// Off by default. Kill switch: ROUTER_DEEPSEEK_ESCAPE_NORMALIZE.
	escapeNormalize bool
	// hardPinExplore gates the Explore sub-agent hard-pin.
	hardPinExplore bool
	// hardPinProvider/hardPinModel route compaction (and, when hardPinExplore is
	// on, Explore sub-agent turns). Derived at boot from the cheapest registered
	// model; overridable via ROUTER_HARD_PIN_PROVIDER / ROUTER_HARD_PIN_MODEL.
	hardPinProvider string
	hardPinModel    string
	// hardPinResolver, when set, overrides boot-time hardPin{Provider,Model}
	// per-request: keeps byokOnly deployments on a provider they can
	// authenticate to, and honors excluded_models on the hard-pin tier via
	// denySet. ok=false signals no eligible provider.
	hardPinResolver func(enabled, denySet map[string]struct{}) (provider, model string, ok bool)
	// telemetry is an optional repository for persisting per-request telemetry.
	telemetry TelemetryRepository
	// captureMode controls whether high-fidelity `router.call` OTLP log
	// records carry full request/response bodies, content hashes, or are
	// suppressed entirely. Default CaptureOff (no log records emitted).
	captureMode ContentCaptureMode
	// captureMaxBytes caps the buffered response body when capture is on;
	// larger bodies are dropped and flagged io.truncated.
	captureMaxBytes int
	// redactor scrubs captured content before export. Nil passes through.
	redactor Redactor
	// byokOnly disables deployment-level credential fallback so customer
	// requests never silently consume the platform's API key budget.
	byokOnly bool
	// excludedModelsOverride, when non-nil, replaces the per-installation
	// exclusion list on every request. Set from ROUTER_EXCLUDED_MODELS at boot.
	excludedModelsOverride map[string]struct{}
	// excludedProvidersOverride, when non-nil, replaces the per-installation
	// provider exclusion list on every request. Set from
	// ROUTER_EXCLUDED_PROVIDERS at boot.
	excludedProvidersOverride map[string]struct{}
	// deploymentKeyedProviders is the subset of registered providers whose
	// upstream API key is configured at the deployment level. When nil, all
	// registered providers are treated as deployment-keyed (legacy behavior).
	deploymentKeyedProviders map[string]struct{}
	// passthroughEligibleProviders is the subset of registered providers
	// reachable via client-supplied auth headers (no deployment key, no
	// BYOK). Surface-scoped: only enabled when the inbound surface matches,
	// otherwise an Anthropic-surface `x-api-key` could forward to
	// api.openai.com (and vice versa) — a cross-provider credential leak.
	passthroughEligibleProviders map[string]struct{}
	// planner parameterizes the Prism-style EV policy for stay-vs-switch.
	planner planner.EVConfig
	// plannerEnabled is the kill switch. When false, the orchestrator falls
	// back to first-decision-wins behavior.
	plannerEnabled bool
	// effortEscalation enables escalate-on-failure reasoning effort: gpt-5.x
	// serves low by default, high after a failed/no-progress turn; gemini
	// stays pinned low. Off by default (ROUTER_EFFORT_ESCALATION).
	effortEscalation bool
	// bandSwap is the per-turn large-vs-small action classifier. Non-nil only
	// when ROUTER_BAND_SWAP is on and the head loaded; a sticky MainLoop STAY
	// then serves the predicted band (one of the pin's {Model, PairedModel})
	// instead of always the anchor.
	bandSwap *bandswap.Classifier
	// loopEscalationEnabled is the kill switch for the cyclic-loop
	// escalate-to-opus action. False keeps detection/telemetry running
	// (action=disabled) but writes no escalation pin. Defaults true.
	loopEscalationEnabled bool
	// loopEscalationHoldoutPct is the percentage of loop-detected sessions
	// deterministically assigned to a log-not-act holdout, so the self-recovery
	// baseline can be subtracted from rescue-rate claims. 0 disables it.
	loopEscalationHoldoutPct int
	// loopEscalationStore persists loop detections (router.loop_escalation_events)
	// and enforces the once-per-session budget. Nil disables persistence and the
	// holdout (which needs a durable row for the withheld rescue).
	loopEscalationStore LoopEscalationStore
	// spiralShadowEnabled gates the shadow-mode spiral detector (log-only
	// death-march signals; see spiral_detection.go). Defaults true — shadow mode
	// changes no routing behavior.
	spiralShadowEnabled bool
	// spiralTracker de-duplicates shadow fires per (session, role, reason) on
	// this replica.
	spiralTracker *spiralTracker
	// spiralShadowStore persists shadow spiral detections durably
	// (router.spiral_shadow_events) and enforces the once-per-(session,
	// reason) budget. Nil degrades to log-only fires.
	spiralShadowStore SpiralShadowStore
	// feedbackStore persists /router-feedback submissions durably
	// (router.router_feedback). Nil degrades to span + log only.
	feedbackStore RouterFeedbackStore
	// summarizer produces a bounded-cost handover summary on switch turns.
	// nil passes the full prior history through unchanged.
	summarizer handover.Summarizer
	// compactionSummarizer produces the structured summary for the proactive
	// context-window compaction cascade (maybeCompact). nil disables Tier-3
	// summarization (the cascade still runs Tier-1 cleanup + trim rescue).
	compactionSummarizer CompactionSummarizer
	// compactionTriggerPct is the fraction of the largest eligible model's
	// context window at which the compaction cascade engages. Zero disables
	// compaction entirely.
	compactionTriggerPct float64
	// availableModels is the boot-time set of model names whose providers are
	// registered. Read by the planner to decide whether a pin's model is still
	// routable.
	availableModels map[string]struct{}
	// defaultBaselineModel is the cost-comparison baseline used when the inbound
	// RequestedModel has no pricing entry. Empty means no substitution.
	defaultBaselineModel string
	// billing, when non-nil, debits the org's prepaid credit balance after
	// each completed upstream call. Wired only in managed mode; the
	// composition root leaves this nil for selfhosted deployments.
	billing *billing.Service
	// retrySleep, when non-nil, overrides the same-binding backoff wait in
	// dispatchWithFallback. Tests inject a no-op to avoid real delays; prod
	// leaves it nil and falls back to sleepWithContext.
	retrySleep func(context.Context, time.Duration) error
	// feedbackRepo persists per-request human feedback (router.request_feedback)
	// and reads it back for the no-login feedback page. Nil leaves the feedback
	// endpoints' DB access disabled (Get/Submit return ErrFeedbackUnavailable).
	feedbackRepo FeedbackRepository
	// feedbackSigner mints + verifies the signed feedback-link token. Nil when
	// ROUTER_FEEDBACK_LINK_SECRET is unset; minting and verification then no-op.
	feedbackSigner *feedback.Signer
	// feedbackBaseURL is the public origin of the feedback page (e.g.
	// https://router.workweave.ai), trailing slash trimmed. Empty disables
	// feedback-link header emission on proxied responses.
	feedbackBaseURL string
	// usageObserver records per-credential subscription rate-limit headroom from
	// upstream response headers, feeding both the cost discount (subsidyFactors)
	// and the usage-bypass gate. Wired when either feature may be used; nil
	// disables both.
	usageObserver *usage.Observer
	// subsidyEnabled gates the cost discount independently of the observer: the
	// observer can be wired for usage-bypass alone while the discount stays off.
	subsidyEnabled bool
	// subsidyEpsilon/subsidyGamma parameterize usage.Snapshot.CostFactor: the
	// floor multiplier for a fully-slack model, and the curvature keeping the
	// factor near epsilon until the window nears its cap.
	subsidyEpsilon float64
	subsidyGamma   float64
}

// pinSessionTTL mirrors Anthropic's prompt-cache TTL on Sonnet/Haiku/Opus 4.5+
// so the pin lifecycle tracks the cache it's keeping warm.
const pinSessionTTL = time.Hour

// pinNeverExpires is the sentinel PinnedUntil for user-forced pins: a
// /force-model must survive arbitrarily long idle gaps and only clear on
// /unforce-model, never lapse on the session TTL. Far enough out to read as
// live indefinitely everywhere PinnedUntil is checked, but still within
// Postgres's timestamp range. /unforce-model rewrites it to a past time.
var pinNeverExpires = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// pinExpiry returns the PinnedUntil to record for a pin with the given decision
// reason. User-forced pins get the never-expires sentinel; every other pin keeps
// the sliding one-hour session TTL.
func pinExpiry(reason string) time.Time {
	if strings.HasPrefix(reason, translate.ReasonUserForceModel) {
		return pinNeverExpires
	}
	return time.Now().Add(pinSessionTTL)
}

// prevTurnMaxedOutThreshold is the LastOutputTokens count above which the
// previous turn is treated as having saturated the output cap (just under
// the 8192 default). OSS-model parse-failure runaways land exactly at the
// cap while legitimate completions rarely approach it; runTurnLoop uses this
// to exclude the pinned model on the next turn and break the auto-continue loop.
const prevTurnMaxedOutThreshold = 8000

// APIKeyIDContextKey is the request-context key for the authenticated api_key_id.
type APIKeyIDContextKey struct{}

// apiKeyIDFromContext returns the authenticated api_key_id, or "" when no key
// is on context (selfhosted/admin paths).
func apiKeyIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(APIKeyIDContextKey{}).(string)
	return id
}

// ExternalIDContextKey is the request-context key for the installation's external_id.
type ExternalIDContextKey struct{}

// CredentialsContextKey is the request-context key for resolved per-request credentials.
type CredentialsContextKey struct{}

// AnthropicSubscriptionContextKey is the request-context key for a caller's raw
// Claude subscription OAuth token, stashed by the auth middleware from the
// X-Weave-Anthropic-Subscription header on router-keyed requests.
type AnthropicSubscriptionContextKey struct{}

// OpenAISubscriptionContextKey and OpenAIAccountIDContextKey are the
// request-context keys for a caller's raw Codex (ChatGPT) subscription OAuth
// JWT and paired ChatGPT-Account-ID, stashed from the
// X-Weave-OpenAI-Subscription / X-Weave-OpenAI-Account-ID headers.
type OpenAISubscriptionContextKey struct{}
type OpenAIAccountIDContextKey struct{}

// codexResponsesBodyContextKey carries the caller's ORIGINAL Responses request
// body on a Codex (ChatGPT) subscription turn. ProxyOpenAIResponses stashes it
// so ProxyOpenAIChatCompletion can route normally but dispatch the untranslated
// Responses body to the Codex backend (its presence marks the passthrough).
type codexResponsesBodyContextKey struct{}

// InstallationExcludedModelsContextKey is the context key for the authed
// installation's model exclusion list. Carried as []string.
type InstallationExcludedModelsContextKey struct{}

// InstallationExcludedProvidersContextKey is the context key for the authed
// installation's provider exclusion list. Carried as []string.
type InstallationExcludedProvidersContextKey struct{}

// InstallationPreferredModelsContextKey is the context key for the authed
// installation's model priority ranking. Carried as []string in descending
// preference (index 0 = first preference). See preferredModelsForRequest.
type InstallationPreferredModelsContextKey struct{}

// InstallationRoutingKnobsContextKey is the context key for the authed
// installation's persisted routing preference (the "quality vs price" dial).
// Carried as *router.Overrides with only Alpha (quality weight) set; the
// per-request x-weave-routing-* header override takes precedence over it. See
// routingKnobsForRequest.
type InstallationRoutingKnobsContextKey struct{}

// InstallationUsageBypassContextKey is the context key for the authed
// installation's subscription usage-bypass gate config. Carried as
// UsageBypassConfig. Absent when the installation hasn't enabled the gate.
type InstallationUsageBypassContextKey struct{}

// InstallationSubscriptionRoutingDisabledContextKey is the context key for the
// authed installation's "disable subscription-aware routing" toggle. Carried as
// bool; absent (== false) when the installation hasn't disabled it. When set,
// subsidyFactors returns nil so the scorer adds no subscription bonus and
// routing decides on merits. See subscriptionRoutingDisabledForRequest.
type InstallationSubscriptionRoutingDisabledContextKey struct{}

// UsageBypassConfig is the per-installation subscription usage-bypass setting,
// stashed on ctx by the auth middleware. Threshold is nil when the toggle is on
// but no value has been chosen yet; the request path falls back to
// defaultUsageBypassThreshold in that case.
type UsageBypassConfig struct {
	Enabled   bool
	Threshold *float64
}

// defaultUsageBypassThreshold is the utilization at/above which the bypass gate
// disengages when an installation has enabled the gate without choosing an
// explicit threshold. Mirrors the conservative default of the legacy
// ROUTER_USAGE_BYPASS_THRESHOLD knob.
const defaultUsageBypassThreshold = 0.95

// usageBypassFromContext returns the per-installation bypass config stashed on
// ctx by the auth middleware, and whether one is present and enabled.
func usageBypassFromContext(ctx context.Context) (UsageBypassConfig, bool) {
	cfg, ok := ctx.Value(InstallationUsageBypassContextKey{}).(UsageBypassConfig)
	if !ok || !cfg.Enabled {
		return UsageBypassConfig{}, false
	}
	return cfg, true
}

// routingMarkerHeader lets a client suppress the in-band "✦ **Weave Router** → …"
// badge — needed by programmatic clients (e.g. pi) that surface the routed
// model out-of-band and can't show a standalone marker text block without it
// hiding the actual answer. off/false/0/none disables it.
const routingMarkerHeader = "X-Weave-Routing-Marker"
const routingMarkerPrefix = "✦ **Weave Router** → "
const maxSidecarDisplayMarkerRunes = 512
const hmmOutcomeReportTimeout = 2 * time.Second

// suppressMarkerIfRequested returns "" when the request opted out via
// routingMarkerHeader, otherwise the marker unchanged. Only applies to the
// per-turn routing badge; no-progress/loop/force-model markers always fire.
func suppressMarkerIfRequested(h http.Header, marker string) string {
	switch strings.ToLower(strings.TrimSpace(h.Get(routingMarkerHeader))) {
	case "off", "false", "0", "none":
		return ""
	}
	return marker
}

// routingMarkerFor builds the "brand → model · note" snippet emitted at the
// start of every cross-format streamed response.
func routingMarkerFor(res turnLoopResult) string {
	decision := res.Decision
	if decision.Model == "" {
		return ""
	}
	if res.SuggestionMode {
		return ""
	}
	// Suppress on tool-result follow-ups (would re-emit a duplicate mid-stream),
	// but always show it if the model changed, even with an unknown reason code.
	modelChanged := res.PriorServedModel != "" && res.PriorServedModel != res.Decision.Model
	if res.PlannerDecision.Reason == "" && !res.HardPinned && res.StickyHit && !modelChanged {
		return ""
	}
	parts := []string{"✦ **Weave Router** → " + decision.Model}
	if decision.Metadata != nil {
		if marker := sanitizeSidecarDisplayMarker(decision.Metadata.DisplayMarker); marker != "" {
			return marker + "\n\n"
		}
	}
	if reason := routingReasonShort(res); reason != "" {
		parts = append(parts, reason)
	}
	return strings.Join(parts, " · ") + "\n\n"
}

func sanitizeSidecarDisplayMarker(raw string) string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.HasPrefix(raw, routingMarkerPrefix) {
		return ""
	}
	lines := strings.Split(raw, "\n")
	kept := make([]string, 0, 3)
	for i, line := range lines {
		if i >= 3 {
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if i > 0 && !strings.HasPrefix(line, "↳ ") {
			break
		}
		kept = append(kept, line)
	}
	if len(kept) == 0 {
		return ""
	}
	out := strings.Join(kept, "\n")
	runes := []rune(out)
	if len(runes) > maxSidecarDisplayMarkerRunes {
		out = string(runes[:maxSidecarDisplayMarkerRunes])
	}
	return out
}

// User-facing routing-marker prose. These are the single source of truth for
// the marker wording; tests assert the mapping against these constants rather
// than re-spelling the literals.
const (
	markerReasonHardPinned    = "pinned for compaction / sub-agent"
	markerReasonUserForced    = "pinned by force-model"
	markerReasonLoopEscalated = "escalated due to loop"
	markerReasonSwitched      = "switched for positive EV after cache eviction"
	markerReasonStayed        = "stayed on your last pick"
	markerReasonTierUpgrade   = "upgraded to a stronger tier"
	markerReasonBestPick      = "best pick for this turn"
	markerReasonBaseline      = "fell back to baseline after provider outage"
)

// baselineRoutingMarkerFor renders the routing badge for an in-turn baseline
// failover. The requested model now serves on Anthropic, so the badge names the
// baseline model rather than the cost-routed OSS slug that went dark. Honors
// suggestion mode like routingMarkerFor; the caller applies the opt-out header.
func baselineRoutingMarkerFor(res turnLoopResult, baselineModel string) string {
	if res.SuggestionMode || baselineModel == "" {
		return ""
	}
	return "✦ **Weave Router** → " + baselineModel + " · " + markerReasonBaseline + "\n\n"
}

// routingReasonShort returns a short user-facing reason for the routing
// decision, or empty when the underlying code is internal recovery noise.
func routingReasonShort(res turnLoopResult) string {
	if res.HardPinned {
		return markerReasonHardPinned
	}
	if res.PlannerDecision.Reason != "" {
		return humanReasonFromPlanner(res.PlannerDecision.Reason)
	}
	switch res.Decision.Reason {
	case translate.ReasonUserForceModel:
		return markerReasonUserForced
	case translate.ReasonLoopEscalation:
		return markerReasonLoopEscalated
	}
	return markerReasonBestPick
}

// humanReasonFromPlanner maps planner reason codes to short user-facing prose.
// Recovery codes (pin_model_missing, pricing_missing) and unknown codes return
// empty so the marker stays clean.
func humanReasonFromPlanner(code string) string {
	switch code {
	case planner.ReasonEVPositive:
		return markerReasonSwitched
	case planner.ReasonEVNegative, planner.ReasonNoPriorUsage:
		return markerReasonStayed
	case planner.ReasonTierUpgrade:
		return markerReasonTierUpgrade
	case planner.ReasonNoPin, planner.ReasonSameModel:
		return markerReasonBestPick
	default:
		return ""
	}
}

// installationExcludedModelsFromContext returns the per-installation exclusion
// list stashed on ctx by the auth middleware, or nil when none is present.
func installationExcludedModelsFromContext(ctx context.Context) []string {
	v := ctx.Value(InstallationExcludedModelsContextKey{})
	if v == nil {
		return nil
	}
	out, _ := v.([]string)
	return out
}

// subscriptionRoutingDisabledForRequest reports whether the authed installation
// has turned off subscription-aware routing. When true, the subscription
// subsidy bonus is suppressed for this request so routing decides on merits.
func subscriptionRoutingDisabledForRequest(ctx context.Context) bool {
	disabled, _ := ctx.Value(InstallationSubscriptionRoutingDisabledContextKey{}).(bool)
	return disabled
}

// routingKnobsForRequest resolves the routing knobs for a request. The
// per-request x-weave-routing-* header override (used by the eval harness)
// wins; otherwise the authed installation's persisted preference applies;
// otherwise nil leaves the scorer on its tuned bundle defaults.
func routingKnobsForRequest(ctx context.Context) *router.Overrides {
	if k := router.RoutingKnobsFromContext(ctx); k != nil {
		return k
	}
	if v, ok := ctx.Value(InstallationRoutingKnobsContextKey{}).(*router.Overrides); ok {
		return v
	}
	return nil
}

// excludedModelsForRequest returns the request's model exclusion set.
// Env override wins; otherwise the installation list is converted to a set.
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

func installationExcludedProvidersFromContext(ctx context.Context) []string {
	v := ctx.Value(InstallationExcludedProvidersContextKey{})
	if v == nil {
		return nil
	}
	out, _ := v.([]string)
	return out
}

// excludedProvidersForRequest returns the request's provider exclusion set.
// Env override wins; otherwise the installation list is converted to a set.
func (s *Service) excludedProvidersForRequest(ctx context.Context) map[string]struct{} {
	if s.excludedProvidersOverride != nil {
		return s.excludedProvidersOverride
	}
	excluded := installationExcludedProvidersFromContext(ctx)
	if len(excluded) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(excluded))
	for _, p := range excluded {
		out[p] = struct{}{}
	}
	return out
}

// installationPreferredModelsFromContext returns the per-installation model
// priority ranking stashed on ctx by the auth middleware, or nil when none is
// present.
func installationPreferredModelsFromContext(ctx context.Context) []string {
	v := ctx.Value(InstallationPreferredModelsContextKey{})
	if v == nil {
		return nil
	}
	out, _ := v.([]string)
	return out
}

// preferredModelsForRequest returns the request's ordered model priority
// ranking (index 0 = first preference). The installation list flows through
// unchanged; the scorer ignores entries not in the eligible pool. There is no
// env override (priority is a per-installation product knob, not an eval lever).
func (s *Service) preferredModelsForRequest(ctx context.Context) []string {
	return installationPreferredModelsFromContext(ctx)
}

// contextWindowOutputReserve is the minimum tokens reserved for the model's
// response when comparing the request estimate against the context window.
const contextWindowOutputReserve = 8_000

// extendedContextTriggerTokens triggers the context-1m-2025-08-07 beta well
// below the 200K standard window: FullTokenEstimate (body bytes ÷5) undercounts
// real tokens by ~20-30% on dense Claude Code bodies, so 140K estimated is
// roughly 175-200K real — the beta must be in place before that arrives.
const extendedContextTriggerTokens = 140_000

// shouldEnableExtendedContext reports whether a request is large enough to
// warrant a CapExtendedContext model's 1M window. Gating on the estimate keeps
// ordinary turns on the standard window; the trigger is low enough that the
// ÷5 undercount can't let a genuinely-near-200K request slip through.
func shouldEnableExtendedContext(est, outputReserve int) bool {
	return est+outputReserve > extendedContextTriggerTokens
}

// contextWindowForRequest returns the effective context window for a model.
// CapExtendedContext models (Opus 4.6+, Sonnet 4.6) always report 1M since the
// proxy unconditionally injects the context-1m beta for them — gating on the
// client's beta header or the token estimate instead would let a large
// request slip onto 200K and overflow on the first turn.
func contextWindowForRequest(modelID string) int {
	if router.Lookup(modelID).Supports(router.CapExtendedContext) {
		return 1_000_000
	}
	return catalog.ContextWindowFor(modelID)
}

// modelStripsAnthropicSignatures reports whether dispatching to model drops
// the Anthropic-only thought-signature blocks (every non-Anthropic-family
// target does). Lets the overflow check discount those bytes for stripping
// targets while counting them for Anthropic passthrough. Unknown models
// default to "keeps" (the conservative side).
func modelStripsAnthropicSignatures(model string) bool {
	m, ok := catalog.ByID(model)
	if !ok {
		return false
	}
	return providers.FamilyFor(m.PrimaryProvider()) != providers.FamilyAnthropic
}

// excludeContextOverflowModels returns a copy of excluded augmented with every
// model in available whose context window is too small for the request, plus
// the sorted IDs newly excluded (for logging). est is the full-body token
// estimate; sigSavings (tokens a signature-stripping target saves) is
// subtracted only for stripping targets, so Anthropic passthrough is still
// checked against the full body. Returns excluded unchanged and nil when
// nothing is added.
func excludeContextOverflowModels(est, sigSavings, outputReserve int, excluded, available map[string]struct{}) (map[string]struct{}, []string) {
	if est <= 0 {
		return excluded, nil
	}
	var out map[string]struct{}
	var overflowed []string
	for model := range available {
		if _, alreadyExcluded := excluded[model]; alreadyExcluded {
			continue
		}
		needed := est + outputReserve
		if sigSavings > 0 && modelStripsAnthropicSignatures(model) {
			needed -= sigSavings
		}
		cw := contextWindowForRequest(model)
		if needed <= cw {
			continue
		}
		if out == nil {
			out = make(map[string]struct{}, len(excluded)+1)
			for k := range excluded {
				out[k] = struct{}{}
			}
		}
		out[model] = struct{}{}
		overflowed = append(overflowed, model)
	}
	if len(overflowed) == 0 {
		return excluded, nil
	}
	sort.Strings(overflowed)
	return out, overflowed
}

// gemini3xRequiresSignedHistory reports whether model is a Gemini 3.x model,
// which 400s (INVALID_ARGUMENT) when the request history carries function-call
// parts lacking the thoughtSignature Gemini issued. Scoped by family name; if
// the catalog later grows a per-model capability flag this should move there.
func gemini3xRequiresSignedHistory(model string) bool {
	return strings.HasPrefix(model, "gemini-3")
}

// excludeGemini3xOnUnsignedHistory augments excluded with every Gemini 3.x
// model when the request history carries an assistant tool call lacking a
// Gemini thoughtSignature (guaranteed 400 on foreign/cross-model history).
// Native Gemini continuations round-trip their own signature and are
// unaffected. Returns excluded unchanged (and nil) when nothing is added.
func excludeGemini3xOnUnsignedHistory(env *translate.RequestEnvelope, excluded, available map[string]struct{}) (map[string]struct{}, []string) {
	if env == nil || !env.HasUnsignedToolCallHistory() {
		return excluded, nil
	}
	var out map[string]struct{}
	var added []string
	for model := range available {
		if !gemini3xRequiresSignedHistory(model) {
			continue
		}
		if _, already := excluded[model]; already {
			continue
		}
		if out == nil {
			out = make(map[string]struct{}, len(excluded)+1)
			for k := range excluded {
				out[k] = struct{}{}
			}
		}
		out[model] = struct{}{}
		added = append(added, model)
	}
	if len(added) == 0 {
		return excluded, nil
	}
	sort.Strings(added)
	return out, added
}

// restrictToTier returns a copy of excluded augmented with every routable
// model whose tier differs from target. Counterpart to a dropped user-forced
// pin: when the forced model can no longer serve (e.g. the pre-filter evicted
// it for context size), the fresh decision should stay in the requested tier
// rather than collapse to the cheap default. ok is false (map unchanged) when
// no in-tier model survives, so the caller can leave routing unconstrained.
func (s *Service) restrictToTier(excluded map[string]struct{}, tier catalog.Tier) (map[string]struct{}, bool) {
	if tier == catalog.TierUnknown {
		return excluded, false
	}
	out := make(map[string]struct{}, len(excluded))
	for k := range excluded {
		out[k] = struct{}{}
	}
	inTierEligible := 0
	consider := func(model string) {
		if catalog.TierFor(model) == tier {
			if _, alreadyExcluded := excluded[model]; !alreadyExcluded {
				inTierEligible++
			}
			return
		}
		out[model] = struct{}{}
	}
	// nil availableModels means "every model routable"; enumerate the catalog
	// in that case so the constraint still has a universe.
	if s.availableModels != nil {
		for model := range s.availableModels {
			consider(model)
		}
	} else {
		for _, m := range catalog.Models {
			consider(m.ID)
		}
	}
	if inTierEligible == 0 {
		return excluded, false
	}
	return out, true
}

// CredentialsFromContext returns the resolved credentials stashed on ctx.
func CredentialsFromContext(ctx context.Context) *Credentials {
	v := ctx.Value(CredentialsContextKey{})
	if v == nil {
		return nil
	}
	creds, _ := v.(*Credentials)
	return creds
}

// anthropicSubscriptionFromContext returns the raw Claude subscription token
// stashed by the auth middleware (router-keyed path), or "" when none.
func anthropicSubscriptionFromContext(ctx context.Context) string {
	v, _ := ctx.Value(AnthropicSubscriptionContextKey{}).(string)
	return v
}

// suppressClaudeSubscriptionContextKey, when true, tells
// resolveAndInjectCredentials to skip the caller's Claude subscription OAuth
// token (falls through to BYOK / deployment key) because the subscription is
// observed-exhausted and would just 429. Scoped to Claude only — a Codex
// subscription on the same request is unaffected.
type suppressClaudeSubscriptionContextKey struct{}

// withSuppressedClaudeSubscription marks ctx so the next credential resolution
// skips the caller's Claude subscription OAuth token (Anthropic only).
func withSuppressedClaudeSubscription(ctx context.Context) context.Context {
	return context.WithValue(ctx, suppressClaudeSubscriptionContextKey{}, true)
}

// claudeSubscriptionSuppressed reports whether the Claude subscription OAuth
// token must be skipped during Anthropic credential resolution for this request.
func claudeSubscriptionSuppressed(ctx context.Context) bool {
	v, _ := ctx.Value(suppressClaudeSubscriptionContextKey{}).(bool)
	return v
}

// servedOnSubscription reports whether the turn's resolved credential is a
// subscription OAuth token (Claude or Codex) — i.e. the customer's own plan
// paid, so billing applies the subscription fee rather than full cost.
func servedOnSubscription(ctx context.Context) bool {
	creds := CredentialsFromContext(ctx)
	return creds != nil && creds.OAuth
}

// openaiSubscriptionFromContext / openaiAccountIDFromContext return the raw Codex
// (ChatGPT) subscription JWT and paired account-id stashed by the auth middleware
// (router-keyed path), or "" when none.
func openaiSubscriptionFromContext(ctx context.Context) string {
	v, _ := ctx.Value(OpenAISubscriptionContextKey{}).(string)
	return v
}

func openaiAccountIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(OpenAIAccountIDContextKey{}).(string)
	return v
}

// codexSubscriptionFromContext resolves a Codex subscription credential from the
// dedicated router-keyed headers (token + account-id), or nil when either is
// absent or the pair isn't a usable Codex subscription.
func codexSubscriptionFromContext(ctx context.Context) *Credentials {
	return codexSubscriptionCreds(openaiSubscriptionFromContext(ctx), openaiAccountIDFromContext(ctx))
}

// codexResponsesRequest reports whether this /v1/responses request carries a
// usable Codex (ChatGPT) subscription — the dedicated header pair, or an
// inbound Authorization bearer + ChatGPT-Account-ID. When true,
// ProxyOpenAIResponses routes to the Codex backend instead of the
// chat-completions path. Mirrors resolveAndInjectCredentials's precedence so
// detection and injection never disagree; the inbound-bearer shape is honored
// even on router-keyed requests (Codex CLI keeps its auth in Authorization
// while the router key rides in X-Weave-Router-Key).
func codexResponsesRequest(ctx context.Context, headers http.Header) bool {
	if codexSubscriptionFromContext(ctx) != nil {
		return true
	}
	if c := ExtractClientCredentials(providers.ProviderOpenAI, headers); c != nil && c.OAuth {
		return true
	}
	return false
}

// DefaultPlannerThresholdUSD is the minimum positive EV over remaining-turn
// horizon to switch off a pinned model. Small enough for arbitrage; large
// enough to avoid near-tie noise.
const DefaultPlannerThresholdUSD = 0.001

// DefaultPlannerExpectedRemainingTurns is the horizon for amortizing per-turn
// savings. Matches observed agentic-loop tail length.
const DefaultPlannerExpectedRemainingTurns = 3

// DefaultPlannerTierUpgradeEnabled turns on the tier guard so a trivial first
// turn can't pin a Low-tier model for the session.
const DefaultPlannerTierUpgradeEnabled = true

// DefaultPlannerColdPinFollowFresh ships off: size it against the planner_*
// shadow telemetry before arming.
const DefaultPlannerColdPinFollowFresh = false

func NewService(r router.Router, providerMap map[string]providers.Client, emitter TelemetryEmitter, embedOnlyUserMessage bool, semanticCache *cache.Cache, pinStore sessionpin.Store, hardPinExplore bool, hardPinProvider, hardPinModel string, telemetry TelemetryRepository) *Service {
	return &Service{
		router:               r,
		providers:            providerMap,
		emitter:              emitter,
		embedOnlyUserMessage: embedOnlyUserMessage,
		semanticCache:        semanticCache,
		pinStore:             pinStore,
		noProgress:           newNoProgressTracker(),
		compaction:           newCompactionTracker(),
		prefixTrimFreeSwitch: true,
		spiralTracker:        newSpiralTracker(),
		spiralShadowEnabled:  true,
		hardPinExplore:       hardPinExplore,
		hardPinProvider:      hardPinProvider,
		hardPinModel:         hardPinModel,
		telemetry:            telemetry,
		planner: planner.EVConfig{
			ThresholdUSD:           DefaultPlannerThresholdUSD,
			ExpectedRemainingTurns: DefaultPlannerExpectedRemainingTurns,
			TierUpgradeEnabled:     DefaultPlannerTierUpgradeEnabled,
		},
		plannerEnabled:        true,
		loopEscalationEnabled: true,
	}
}

// WithPlanner overrides the EV-policy configuration. ThresholdUSD is assigned
// verbatim (zero and negative are legitimate). ExpectedRemainingTurns falls
// back to the default on non-positive values.
func (s *Service) WithPlanner(cfg planner.EVConfig) *Service {
	s.planner.ThresholdUSD = cfg.ThresholdUSD
	if cfg.ExpectedRemainingTurns > 0 {
		s.planner.ExpectedRemainingTurns = cfg.ExpectedRemainingTurns
	}
	s.planner.TierUpgradeEnabled = cfg.TierUpgradeEnabled
	s.planner.ColdPinFollowFresh = cfg.ColdPinFollowFresh
	return s
}

// WithPlannerEnabled is the kill switch. When false, the orchestrator
// preserves first-decision-wins behavior.
func (s *Service) WithPlannerEnabled(enabled bool) *Service {
	s.plannerEnabled = enabled
	return s
}

// WithPrefixTrimFreeSwitch is the kill switch for the prefix-trim free-switch
// window. Detection and the post-routing compaction handover are unaffected.
func (s *Service) WithPrefixTrimFreeSwitch(enabled bool) *Service {
	s.prefixTrimFreeSwitch = enabled
	return s
}

// WithEscapeNormalize is the kill switch for the file-edit tool escape-repair
// pass on cross-format OpenAI-upstream responses (see
// translate.AnthropicSSETranslator.WithEscapeNormalize).
func (s *Service) WithEscapeNormalize(enabled bool) *Service {
	s.escapeNormalize = enabled
	return s
}

// WithEffortEscalation enables the escalate-on-failure reasoning-effort policy.
// When false (default) the router leaves request-derived effort untouched.
func (s *Service) WithEffortEscalation(enabled bool) *Service {
	s.effortEscalation = enabled
	return s
}

// WithBandSwap enables the per-turn large-vs-small action-classifier swap,
// loading the compiled-in head once. A load failure logs and leaves the swap
// disabled (fail-safe to anchor-only) rather than killing boot.
func (s *Service) WithBandSwap(enabled bool) *Service {
	if !enabled {
		s.bandSwap = nil
		return s
	}
	clf, err := bandswap.New()
	if err != nil {
		observability.Get().Error("band swap head failed to load; per-turn swap disabled", "err", err)
		s.bandSwap = nil
		return s
	}
	s.bandSwap = clf
	return s
}

// WithLoopEscalationConfig sets the cyclic-loop escalation kill switch and the
// log-not-act holdout percentage (clamped to [0, 100]). The holdout only takes
// effect when a LoopEscalationStore is wired — otherwise a withheld rescue
// leaves no durable row and is pure loss, not a measurement.
func (s *Service) WithLoopEscalationConfig(enabled bool, holdoutPct int) *Service {
	s.loopEscalationEnabled = enabled
	if holdoutPct < 0 {
		holdoutPct = 0
	}
	if holdoutPct > 100 {
		holdoutPct = 100
	}
	s.loopEscalationHoldoutPct = holdoutPct
	return s
}

// WithLoopEscalationStore wires the durable sink for loop-escalation events
// (router.loop_escalation_events). Nil disables persistence, the holdout, and
// the cross-TTL once-per-session budget (the pin-reason check still applies).
func (s *Service) WithLoopEscalationStore(store LoopEscalationStore) *Service {
	s.loopEscalationStore = store
	return s
}

// WithSpiralShadowConfig sets the shadow-mode spiral detector kill switch.
// enabled=false skips signal computation entirely. Shadow mode takes no
// routing action either way; the switch exists to shed the per-turn scan
// cost if it ever misbehaves.
func (s *Service) WithSpiralShadowConfig(enabled bool) *Service {
	s.spiralShadowEnabled = enabled
	return s
}

// WithSpiralShadowStore wires the durable sink for shadow spiral events
// (router.spiral_shadow_events). Nil degrades to log-only fires with
// replica-local de-duplication.
func (s *Service) WithSpiralShadowStore(store SpiralShadowStore) *Service {
	s.spiralShadowStore = store
	return s
}

// WithRouterFeedbackStore wires the durable sink for /router-feedback
// submissions (router.router_feedback). Nil degrades to span + log only.
func (s *Service) WithRouterFeedbackStore(store RouterFeedbackStore) *Service {
	s.feedbackStore = store
	return s
}

// WithContentCapture configures high-fidelity `router.call` OTLP log emission.
// mode selects off/hashed/full; maxBytes caps the buffered response body;
// redactor (optional) scrubs content before export. No-op effect when the
// emitter is disabled. Default (unset) is CaptureOff.
func (s *Service) WithContentCapture(mode ContentCaptureMode, maxBytes int, redactor Redactor) *Service {
	s.captureMode = mode
	if maxBytes > 0 {
		s.captureMaxBytes = maxBytes
	}
	s.redactor = redactor
	return s
}

// forcedReasoningEffort implements escalate-on-failure effort policy, returning
// the EmitOptions.ForceReasoningEffort override ("" = no override):
//
//   - gpt-5.x: "low" by default, "high" after a failed/no-progress prior turn.
//     On SWE-Bench Pro this beats both fixed policies (24% < 32% < ~40% resolved)
//     since high is spent only where it flips the outcome.
//   - gemini-3.x: pinned "low" — effort-immune on hard tasks (0/15 in the sweep).
//   - everything else: "" — left to its own path.
func forcedReasoningEffort(model string, escalate bool) string {
	switch {
	case strings.HasPrefix(model, "gpt-5"):
		if escalate {
			return "high"
		}
		return "low"
	case strings.HasPrefix(model, "gemini-3"):
		return "low"
	default:
		return ""
	}
}

// WithSummarizer installs the cheap-model summarizer for handover on switch
// turns. nil disables the summary step; the full prior history is passed
// through unchanged.
func (s *Service) WithSummarizer(sz handover.Summarizer) *Service {
	s.summarizer = sz
	return s
}

// WithCompaction installs the summarizer and trigger threshold for the
// proactive context-window compaction cascade (maybeCompact). pct == 0
// disables compaction (operators set ROUTER_COMPACTION_PCT=0 to turn the
// cascade off); an out-of-range pct (negative or > 1) falls back to
// DefaultCompactionTriggerPct. A nil summarizer leaves Tier-3 summarization off
// (Tier-1 cleanup + trim rescue still run).
func (s *Service) WithCompaction(cs CompactionSummarizer, pct float64) *Service {
	s.compactionSummarizer = cs
	if pct < 0 || pct > 1 {
		pct = DefaultCompactionTriggerPct
	}
	s.compactionTriggerPct = pct
	return s
}

// WithAvailableModels installs the boot-time set of routable model names.
// The planner consults this set so a pin whose model is no longer
// available forces a switch. nil treats every model as available.
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

// WithHardPinResolver installs a per-request hard-pin resolver. nil
// preserves the boot-time hardPin{Provider,Model} for every request.
// ok=false signals no eligible provider, surfacing ErrClusterUnavailable.
func (s *Service) WithHardPinResolver(resolver func(enabled, denySet map[string]struct{}) (provider, model string, ok bool)) *Service {
	s.hardPinResolver = resolver
	return s
}

// WithDefaultBaselineModel installs the cost-comparison fallback for when
// the inbound RequestedModel has no pricing entry. Empty disables.
func (s *Service) WithDefaultBaselineModel(model string) *Service {
	s.defaultBaselineModel = model
	return s
}

// baselineFor returns requested if it has a pricing entry, otherwise the
// configured defaultBaselineModel (which may be "").
func (s *Service) baselineFor(requested string) string {
	if requested != "" {
		if _, ok := catalog.PrimaryPriceFor(requested); ok {
			return requested
		}
	}
	return s.defaultBaselineModel
}

// WithByokOnly enables BYOK-only credential resolution: providers without
// caller-supplied credentials are ineligible.
func (s *Service) WithByokOnly(byokOnly bool) *Service {
	s.byokOnly = byokOnly
	return s
}

// WithExcludedModelsOverride pins the per-request model exclusion list to a
// deployment-wide set. Pass nil or empty slice to clear the override.
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

// HasExcludedModelsOverride reports whether an excluded-models override is active.
func (s *Service) HasExcludedModelsOverride() bool {
	return s.excludedModelsOverride != nil
}

// ExcludedModelsOverride returns a sorted copy of the override list.
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

// WithExcludedProvidersOverride pins the per-request provider exclusion list
// to a deployment-wide set. Pass nil or empty slice to clear the override.
func (s *Service) WithExcludedProvidersOverride(providerNames []string) *Service {
	if len(providerNames) == 0 {
		s.excludedProvidersOverride = nil
		return s
	}
	set := make(map[string]struct{}, len(providerNames))
	for _, p := range providerNames {
		set[p] = struct{}{}
	}
	s.excludedProvidersOverride = set
	return s
}

// HasExcludedProvidersOverride reports whether an excluded-providers override is active.
func (s *Service) HasExcludedProvidersOverride() bool {
	return s.excludedProvidersOverride != nil
}

// ExcludedProvidersOverride returns a sorted copy of the override list.
func (s *Service) ExcludedProvidersOverride() []string {
	if s.excludedProvidersOverride == nil {
		return nil
	}
	out := make([]string, 0, len(s.excludedProvidersOverride))
	for p := range s.excludedProvidersOverride {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// usageRequired reports whether per-request token usage must be captured.
// OTel export, DB telemetry persistence, and credit billing all need it.
func (s *Service) usageRequired() bool {
	return s.emitter != nil || s.telemetry != nil || s.billing != nil
}

// newTelemetryBuffer returns a request-scoped buffer, or nil when OTel is
// disabled — guards against a nil-interface method-call panic.
func (s *Service) newTelemetryBuffer() *otel.Buffer {
	if s.emitter == nil {
		return nil
	}
	return s.emitter.NewBuffer()
}

// WithBillingService installs the credit-billing service. Nil disables the
// per-request debit hook. Wired only in managed mode by the composition
// root; the WithBalanceCheck middleware is paired with it so a request
// that depleted its balance is 402'd before reaching the proxy.
func (s *Service) WithBillingService(b *billing.Service) *Service {
	s.billing = b
	return s
}

// WithDeploymentKeyedProviders restricts the default eligible set to
// providers whose deployment env key is set. nil restores legacy behavior
// (all registered providers eligible).
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

// WithPassthroughEligibleProviders names providers that are reachable via
// client-supplied auth headers (no deployment key, no BYOK). Entries are
// surface-scoped in enabledProvidersForRequest: an Anthropic-surface
// request can enable Anthropic via passthrough but NOT OpenAI, and vice
// versa. Without this guard, cross-surface routing would forward the
// wrong credential type to a third-party API.
func (s *Service) WithPassthroughEligibleProviders(set map[string]struct{}) *Service {
	if set == nil {
		s.passthroughEligibleProviders = nil
		return s
	}
	copied := make(map[string]struct{}, len(set))
	for p := range set {
		copied[p] = struct{}{}
	}
	s.passthroughEligibleProviders = copied
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

// ErrRequestNotJSONObject re-exports translate.ErrNotJSONObject so api/* handlers
// avoid importing internal/translate directly (layering rule, root CLAUDE.md).
var ErrRequestNotJSONObject = translate.ErrNotJSONObject

// stripRoutingMarkerFromMessages is a seam over translate.StripRoutingMarkerFromMessages
// so tests can force a strip failure without depending on a real reproducer;
// prod code never overrides it.
var stripRoutingMarkerFromMessages = translate.StripRoutingMarkerFromMessages

// semanticCacheMaxBodyBytes caps how large a response the cache will store;
// larger bodies stream through but skip the Store call to bound peak memory.
const semanticCacheMaxBodyBytes = 1 << 20

// headersToSkipOnHit lists response headers the cache must NOT replay.
// request-id ties to a specific upstream call; x-router-* are set fresh from
// the live decision so the client sees current routing, not stale. The
// feedback link encodes the original request's signed token; cache hits write
// no telemetry row to back a feedback page, so the link is omitted on hits
// entirely — the skip here guards against ever replaying the cached one.
var headersToSkipOnHit = map[string]struct{}{
	"Request-Id":            {},
	"X-Request-Id":          {},
	"X-Router-Decision":     {},
	"X-Router-Provider":     {},
	"X-Router-Model":        {},
	"X-Router-Cache":        {},
	"X-Router-Feedback-Url": {},
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
// from the live decision so the client sees an accurate routing trace. No
// feedback link is set: a cache hit writes no telemetry row, so its feedback
// page would have no routing context to show.
func (s *Service) writeCachedResponse(w http.ResponseWriter, resp cache.CachedResponse, decision router.Decision) {
	for k, vs := range resp.Headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set(HeaderRouterDecision, decision.Reason)
	w.Header().Set(HeaderRouterProvider, decision.Provider)
	w.Header().Set(HeaderRouterModel, decision.Model)
	w.Header().Set(HeaderRouterCache, RouterCacheHit)
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

// WithRLRouter installs the opt-in RL/DPO policy router. nil leaves the
// x-weave-router-strategy: rl header with no backing router, in which case
// routeFor 503s for that header rather than silently serving the cluster
// scorer.
func (s *Service) WithRLRouter(r router.Router) *Service {
	s.rlRouter = r
	return s
}

// WithHMMRouter installs the opt-in HMM routing sidecar. nil leaves the
// x-weave-router-strategy: hmm header with no backing router; routeFor then
// 503s rather than silently serving the cluster scorer.
func (s *Service) WithHMMRouter(r router.Router) *Service {
	s.hmmRouter = r
	if reporter, ok := r.(hmm.OutcomeReporter); ok {
		s.hmmOutcomeReporter = reporter
	} else {
		s.hmmOutcomeReporter = nil
	}
	return s
}

// WithBanditRouter installs the opt-in Thompson-sampling bandit router. nil
// leaves x-weave-router-strategy: bandit with no backing router, in which case
// routeFor 503s rather than silently serving the cluster scorer.
func (s *Service) WithBanditRouter(r router.Router) *Service {
	s.banditRouter = r
	return s
}

// routeFor picks the active router for the request's strategy. The default
// (and the cluster strategy) is the cluster scorer; the rl strategy uses the
// RL policy router when wired, and otherwise fails closed with
// ErrPolicyUnavailable (→ HTTP 503) — never a silent fallback that would mask
// which strategy actually served the turn.
func (s *Service) routeFor(ctx context.Context, req router.Request) (router.Decision, error) {
	switch router.StrategyFromContext(ctx) {
	case router.StrategyRL:
		if s.rlRouter == nil {
			return router.Decision{}, fmt.Errorf("rl strategy requested but no policy sidecar configured: %w", rl.ErrPolicyUnavailable)
		}
		return s.rlRouter.Route(ctx, req)
	case router.StrategyHMM:
		if s.hmmRouter == nil {
			return router.Decision{}, fmt.Errorf("hmm strategy requested but no policy sidecar configured: %w", hmm.ErrHMMUnavailable)
		}
		return s.hmmRouter.Route(ctx, req)
	case router.StrategyBandit:
		if s.banditRouter == nil {
			return router.Decision{}, fmt.Errorf("bandit strategy requested but no posterior configured: %w", bandit.ErrBanditUnavailable)
		}
		return s.banditRouter.Route(ctx, req)
	default:
		return s.router.Route(ctx, req)
	}
}

// Route exposes the underlying router for callers that need a decision
// without dispatching (e.g. admin endpoints). Honors the per-request strategy.
func (s *Service) Route(ctx context.Context, req router.Request) (router.Decision, error) {
	return s.routeFor(ctx, req)
}

// RouteAnthropicRequest parses a raw Anthropic-Messages body and returns the
// routing decision without dispatching (e.g. the /v1/route dry-run endpoint).
// Owns translate.ParseAnthropic + RoutingFeatures extraction internally so
// callers in internal/api/* never import internal/translate directly,
// matching ProxyMessages.
func (s *Service) RouteAnthropicRequest(ctx context.Context, body []byte) (decision router.Decision, err error) {
	env, parseErr := translate.ParseAnthropic(body)
	if parseErr != nil {
		err = fmt.Errorf("parse request: %w", parseErr)
		return decision, err
	}

	embedFlag := s.ResolveEmbedOnlyUserMessage(ctx)
	feats := env.RoutingFeatures(embedFlag)
	promptText := feats.PromptText
	if embedFlag && feats.OnlyUserMessageText != "" {
		promptText = feats.OnlyUserMessageText
	}

	decision, err = s.Route(ctx, router.Request{
		RequestedModel:       feats.Model,
		EstimatedInputTokens: feats.Tokens,
		HasTools:             feats.HasTools,
		PromptText:           promptText,
		RoutingKnobs:         router.RoutingKnobsFromContext(ctx),
	})
	return decision, err
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

	// Claude Code sends its 1M-context model variant tag (e.g.
	// "claude-opus-4-8[1m]") in the body. It is a client display convention,
	// not a real Anthropic model id, so a verbatim count_tokens / model-list
	// passthrough to the native Anthropic API 404s ("the selected model may not
	// exist"). Strip it to the canonical id; passthrough never rewrites the
	// model otherwise.
	if providerName == providers.ProviderAnthropic && len(body) > 0 {
		if canon, had, cerr := translate.CanonicalizeModelInBody(body); cerr == nil && had {
			body = canon
		}
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

// logUpstreamBody emits per-attempt dispatch metadata at Info. Body is
// intentionally omitted — use captureMode/Redactor (turn_logs.go) for
// per-attempt body capture.
func logUpstreamBody(log *slog.Logger, sessionKey [sessionpin.SessionKeyLen]byte, decision router.Decision, feats translate.RoutingFeatures, body []byte) {
	log.Info("upstream prepared request",
		"session_key", hex.EncodeToString(sessionKey[:8]),
		"decision_model", decision.Model,
		"decision_provider", decision.Provider,
		"message_count", feats.MessageCount,
		"body_len", len(body),
	)
}

// ProxyMessages routes a raw Anthropic-Messages request body and streams the
// upstream response back. The routing decision is reflected in x-router-* headers.
// anthropicNativeAttempt builds the per-binding dispatch closure for an
// Anthropic-native upstream (no cross-format translation). The marker sink
// and usage extractor are rebuilt per attempt off the dispatched decision (d)
// so a baseline failover that switches models renders the right marker.
// setExtractor publishes the attempt's extractor for post-dispatch attribution.
func (s *Service) anthropicNativeAttempt(
	env *translate.RequestEnvelope,
	r *http.Request,
	prep providers.PreparedRequest,
	sink http.ResponseWriter,
	preludeBuf *preludeBuffer,
	marker string,
	setExtractor func(*otel.UsageExtractor),
) dispatchAttempt {
	return func(actx context.Context, d router.Decision, p providers.Client) error {
		attemptSink := sink
		if marker != "" {
			attemptSink = translate.NewAnthropicRoutingMarkerWriter(sink, d.Model, marker)
		}
		proxyWriter := attemptSink
		if s.usageRequired() {
			ex := otel.NewUsageExtractor(attemptSink, d.Provider)
			proxyWriter = ex
			setExtractor(ex)
		}
		if preludeBuf != nil {
			preludeBuf.Seal()
		}
		err := p.Proxy(actx, d, prep, proxyWriter, r)
		// Post-commit: bytes already on the wire, so render the error as an
		// in-stream frame instead of letting flushErr append a corrupting
		// envelope. Pre-commit errors go through dispatchWithFallback instead.
		if err != nil && env.Stream() && preludeBuf.Committed() {
			err = emitAnthropicSSEErrorEvent(sink, err)
		}
		return err
	}
}

func (s *Service) ProxyMessages(ctx context.Context, body []byte, w http.ResponseWriter, r *http.Request) error {
	ctx = s.withUsageObserver(ctx, r.Header)
	log := observability.FromContext(ctx)
	requestStart := time.Now()
	requestID := uuid.New().String()
	buf := s.newTelemetryBuffer()
	ctx = buf.WithContext(ctx)

	// Strip the routing marker prior responses injected as assistant text —
	// clients echo it back verbatim, so left in place it accumulates in
	// upstream context every turn.
	body, stripErr := stripRoutingMarkerFromMessages(body)
	if stripErr != nil {
		log.Error("Failed to strip routing marker from inbound messages", "err", stripErr)
		return fmt.Errorf("strip routing marker: %w", stripErr)
	}

	// Same for the one-click thumbs footer (and its signed rate URLs), which
	// would otherwise shift assistant prefixes off the prompt cache.
	// Best-effort: log-and-continue on failure rather than abort over cosmetic
	// cleanup, matching the OpenAI chat path.
	if strippedBody, ferr := translate.StripFeedbackFooterFromMessages(body); ferr != nil {
		log.Error("Failed to strip feedback footer from inbound messages", "err", ferr)
	} else {
		body = strippedBody
	}

	// Strip Claude Code's 1M-context model variant tag (e.g.
	// "claude-opus-4-8[1m]") to the canonical id before parsing, so routing/pins/
	// telemetry key off the real model and it never reaches a native Anthropic
	// upstream (which 404s on it). The 1M window is enabled separately via the
	// context-1m beta.
	if canon, _, modelErr := translate.CanonicalizeModelInBody(body); modelErr != nil {
		log.Error("Failed to canonicalize inbound model", "err", modelErr)
	} else {
		body = canon
	}

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

	// Bind session_key/request_id/api_key_id/ingress onto a ctx-scoped logger.
	// The derived key is reused below to avoid a second hash + a divergent key
	// if env.body mutates mid-flow.
	var sessionKey [sessionpin.SessionKeyLen]byte
	ctx, log, sessionKey = bindRequestLogger(ctx, env, apiKeyID, requestID, "anthropic_messages")
	log.Info("ProxyMessages start",
		"requested_model", feats.Model,
		"stream", env.Stream(),
		"message_count", feats.MessageCount,
		"has_tools", feats.HasTools,
		"total_input_tokens", feats.Tokens,
		"prompt_preview", observability.Preview(promptText, 200),
	)

	// Handle /force-model and /unforce-model before routing (stripped from
	// env.body so the upstream never sees it). Session key is derived before
	// extraction: DeriveSessionKey can fall back to prompt text, and deriving
	// after the strip would mismatch subsequent turns with the unstripped message.
	if s.pinStore != nil {
		if cmd, hasCmd := env.ExtractForceModelCommand(); hasCmd {
			log.Info("ProxyMessages force-model command", "force_model_cmd", cmd)
			return s.handleForceModelCommand(ctx, w, env, cmd, installationID, sessionKey, feats.Tokens)
		}
	}
	if cmd, hasCmd := env.ExtractRouterFeedbackCommand(); hasCmd {
		log.Info("ProxyMessages router-feedback command")
		return s.handleRouterFeedbackCommand(ctx, w, env, cmd, installationID, sessionKey, feats.Tokens)
	}

	// Honor the x-weave-force-model header (headless equivalent of /force-model).
	// Writes the user-forced pin and falls through to normal routing, which picks
	// the pin up and serves the requested model on this same turn.
	s.applyForceModelHeader(ctx, r, env, installationID, sessionKey)

	// Tool-call loop break: catches runaway OSS-model tool-call cycles (qwen3
	// in particular) that the previous-turn-maxed-out guard misses because
	// each call returns quickly and under the output cap.
	// Wide cyclic re-read loop (same few files, no edits, dozens of turns) on a
	// cheap/mid model escalates the session to opus instead, taking precedence
	// over the tight-loop break below — rescuing beats stopping.
	escalatedLoop := false
	if cyc, csig, ccount, cratio, cwin := detectCyclicToolCallLoop(env); cyc {
		loopRole := roleForTier(catalog.TierFor(feats.Model))
		s.handleLoopEscalation(ctx, csig, ccount, cratio, cwin, installationID, sessionKey, loopRole, feats.Model)
		escalatedLoop = true
	}
	if !escalatedLoop {
		if loop, sig, count := detectToolCallLoop(env); loop {
			loopRole := roleForTier(catalog.TierFor(feats.Model))
			log.Info("ProxyMessages tool-call loop detected", "tool_sig", sig, "repeat_count", count, "role", loopRole)
			return s.handleToolCallLoopBreak(ctx, w, env, sig, count, installationID, sessionKey, loopRole, feats.Model, providers.ProviderAnthropic, feats.Tokens)
		}
	}

	// Surface inbound tool_use / tool_result blocks the model is about to see.
	// Lets us audit whether a misbehaving turn was provoked by a malformed prior
	// tool_result or an out-of-shape tool spec, without dumping the whole body.
	logInboundRequestDiagnostics(log, env)

	// Anthropic packs sub-agent identity into metadata.user_id; the
	// x-weave-subagent-type header is for non-Anthropic ingress only.
	enabledProviders := s.enabledProvidersForRequest(ctx, providers.ProviderAnthropic, r.Header)

	// Pre-filter models whose context window cannot fit this request.
	// FullTokenEstimate uses raw body bytes (÷5) to capture tool definitions,
	// tool calls, and tool results that feats.Tokens (text-only) misses.
	outputReserve := contextWindowOutputReserve
	if feats.MaxTokens > outputReserve {
		outputReserve = feats.MaxTokens
	}
	baseExcluded := s.excludedModelsForRequest(ctx)

	// Snapshot inbound (client-sent) state BEFORE any env rewrite. The
	// compaction tracker, spiral scan, and tool-output telemetry must compare
	// what the client actually sent, not a router-shortened body — either the
	// proactive compaction just below or runTurnLoop's switch-handover rewrite.
	inboundToolCallCount := len(env.AssistantToolCallSignatures())
	var inboundSpiralSignals spiralSignals
	if s.spiralShadowEnabled {
		inboundSpiralSignals = computeSpiralSignals(env, feats.MessageCount)
	}
	inboundLastUser := env.LastUserMessage()

	// Proactive context-window compaction: shrink an over-long conversation to
	// fit the largest eligible model BEFORE routing, so a genuinely huge
	// session is compacted (à la Claude Code) instead of dead-ending in the
	// scorer with no eligible provider. Mutates env; feats is recomputed after.
	maxEligibleWindow := s.maxEligibleContextWindow(baseExcluded, env.SignatureTokenSavings())
	compRes, compErr := s.maybeCompact(ctx, env, turntype.DetectFromEnvelope(env, feats, ""), outputReserve, maxEligibleWindow, r.Header)
	if compErr != nil {
		log.Warn("Compaction could not fit request to any eligible model",
			"err", compErr, "final_estimate", compRes.FinalEstimate, "max_window", maxEligibleWindow, "requested_model", feats.Model)
		return compErr
	}
	if compRes.Applied {
		feats = env.RoutingFeatures(embedFlag)
		log.Info("Proactive compaction applied",
			"tool_results_cleared", compRes.ToolResultsCleared,
			"summarized", compRes.Summarized,
			"summary_model", compRes.SummaryModel,
			"trimmed_to_recent", compRes.TrimmedToRecent,
			"final_estimate", compRes.FinalEstimate,
		)
	}

	overflowEstimate := env.ContextOverflowTokenEstimate()
	excluded, ctxOverflowed := excludeContextOverflowModels(overflowEstimate, env.SignatureTokenSavings(), outputReserve, baseExcluded, s.availableModels)
	if len(ctxOverflowed) > 0 {
		log.Info("context window pre-filter: excluded over-capacity models",
			"overflow_token_estimate", overflowEstimate,
			"output_reserve", outputReserve,
			"excluded_count", len(ctxOverflowed),
			"excluded_models", strings.Join(ctxOverflowed, ","),
		)
	}
	excluded, geminiUnsigned := excludeGemini3xOnUnsignedHistory(env, excluded, s.availableModels)
	if len(geminiUnsigned) > 0 {
		log.Info("gemini pre-filter: excluded gemini-3.x for unsigned tool-call history",
			"excluded_models", strings.Join(geminiUnsigned, ","),
		)
	}

	routeStart := time.Now()
	req := router.Request{
		RequestedModel:       feats.Model,
		EstimatedInputTokens: feats.Tokens,
		HasTools:             feats.HasTools,
		HasImages:            feats.HasImages,
		PromptText:           promptText,
		EnabledProviders:     enabledProviders,
		ExcludedModels:       excluded,
		PreferredModels:      s.preferredModelsForRequest(ctx),
		RoutingKnobs:         routingKnobsForRequest(ctx),
	}
	routeRes, routeErr := s.runTurnLoop(ctx, env, feats, apiKeyID, installationID, "", r.Header, req)
	if routeErr != nil {
		log.Error("Routing failed", "err", routeErr, "route_ms", time.Since(routeStart).Milliseconds(), "requested_model", feats.Model, "total_input_tokens", feats.Tokens)
		return routeErr
	}

	// Subscription usage-bypass: engaged inside runTurnLoop after hard-pin,
	// force-pin, and tool-result sticky branches (those still win). The
	// caller's own Claude subscription has headroom, so serve straight through
	// to Anthropic with no substitution and no billing debit.
	if routeRes.UsageBypass {
		err := s.bypassToAnthropic(ctx, env, feats, routeRes.modelSwitched(), requestStart, requestID, externalID, r, w)
		if !errors.Is(err, errBypassRetryable) {
			return err
		}
		// Bypass hit a pre-commit retryable error (e.g. Anthropic 429 weekly-limit
		// or transport error). Refresh the subsidy cost factor so the scorer
		// discounts Anthropic correctly on reroute.
		req.SubsidizedModelCostFactor = s.subsidyFactors(ctx, r.Header)

		// Bypass returns early without loading the pin, so load it now for
		// modelSwitched() to correctly detect a switch away from it.
		var priorServedModel string
		var sessionEverSwitched bool
		if s.pinStore != nil {
			sessionKey := DeriveSessionKey(env, apiKeyID)
			pin, pinFound := s.loadPin(ctx, sessionKey, roleForTier(catalog.TierFor(feats.Model)))
			if pinFound {
				priorServedModel = pin.LastServedModel
				sessionEverSwitched = pin.HasEverSwitched
			}
		}

		log.Info("usage-bypass pre-commit failure, rerouting via scorer",
			"request_id", requestID,
			"external_id", externalID,
			"requested_model", feats.Model,
		)
		routeRes.UsageBypass = false
		decision, routeErr := s.routeFor(ctx, req)
		if routeErr != nil {
			log.Error("Reroute after usage-bypass failure failed", "err", routeErr)
			return routeErr
		}
		routeRes.Decision = decision
		routeRes.Fresh = decision
		// Populate switch-detection fields skipped during bypass.
		routeRes.PriorServedModel = priorServedModel
		routeRes.SessionEverSwitched = sessionEverSwitched
	}
	routeRes.SuggestionMode = r.Header.Get("x-weave-suggestion-mode") == "true"
	decision := routeRes.Decision
	tt := routeRes.TurnType
	stickyHit := routeRes.StickyHit
	pinTier := routeRes.PinTier
	pinAgeSec := routeRes.PinAgeSec
	routeMs := time.Since(routeStart).Milliseconds()
	s.logPlannerOutcome(ctx, routeRes)

	// Cross-envelope no-progress detector: if this session dispatches the same
	// (decision, message_count, tool-progress, prompt-prefix) fingerprint
	// repeatedly within a window, the agent is stuck (sub-agent spawn loop, or
	// a re-issued identical call) — break the pin and emit a synthetic stop.
	// The tool-progress marker is the key guard: a genuinely progressing agent
	// appends a new tool call each turn, so it never false-positives even when
	// top-level message count stays flat (as with Explore sub-agents).
	if fp := computeNoProgressFingerprint(decision, promptText, feats.MessageCount, toolProgressMarker(env)); s.noProgress != nil {
		role := roleForTier(catalog.TierFor(feats.Model))
		if looped, count := s.noProgress.recordAndDetect(routeRes.SessionKey, installationID, role, fp, time.Now()); looped {
			return s.handleNoProgressBreak(ctx, w, env, count, installationID, routeRes.SessionKey, role, decision.Model, decision.Provider, feats.Tokens)
		}
	}

	// Shadow-mode spiral detector: log-only death-march signals (error grind,
	// same-file thrash, fuzzy repetition, monologue), once per (session,
	// reason), so fire rates/precision can be measured before any escalation
	// is armed. Main-loop / tool-result turns only — hard-pinned turn types
	// carry history shapes that mimic the signals.
	if s.spiralShadowEnabled && (tt == turntype.MainLoop || tt == turntype.ToolResult) {
		if reasons := spiralReasons(inboundSpiralSignals); len(reasons) > 0 {
			role := roleForTier(catalog.TierFor(feats.Model))
			// Use the bindRequestLogger digest, not routeRes.SessionKey (zero
			// with no pin store), so the spiral event's session_key matches the
			// telemetry row's in every mode for the offline join.
			s.handleSpiralShadow(ctx, inboundSpiralSignals, reasons, installationID, sessionKey, role, decision.Model, string(tt))
		}
	}

	// Compaction-aware handover: Claude Code can trim history via full
	// compaction (message count drops sharply) or rolling-window trimming
	// (flat message count, tool-call count shrinks). Either leaves the
	// non-Anthropic model unaware of elided edits/decisions, so rewrite the
	// envelope with a handover summary before dispatch.
	compactionHandoverRan := false
	var compactionHandoverOutcome handoverOutcome
	// Detection runs pre-routing in runTurnLoop; routeRes.PrefixTrimmed carries
	// the verdict. Skip if a model-switch handover already rewrote env this
	// turn — a second rewrite would double-trim it. Also skip when the proactive
	// compaction cascade already ran this turn: it shrank env (which trips the
	// client-trim detector as a false positive), so a compaction handover here
	// would be a redundant summarizer call that also discards the recent-turn
	// tail maybeCompact deliberately kept.
	if decision.Provider != providers.ProviderAnthropic && !routeRes.HardPinned && !routeRes.Handover.Invoked && !compRes.Applied && routeRes.PrefixTrimmed {
		log.Info("Context trimming detected on non-Anthropic route; rewriting context with handover summary",
			"message_count", feats.MessageCount,
			"tool_call_count", inboundToolCallCount,
			"decision_model", decision.Model,
			"decision_provider", decision.Provider,
		)
		compactionHandoverOutcome = s.runCompactionHandover(ctx, env, r.Header, decision.Model)
		compactionHandoverRan = true
	}

	// Semantic-cache eligibility: configured, non-streaming, decision has
	// metadata, externalID present, not eval traffic. Skip when a compaction
	// handover rewrote env (embedding predates the rewrite) or when subsidy
	// factors are non-empty (the cache key doesn't capture quota-headroom-
	// dependent model choice; subsidyFactors returns nil when the feature is off).
	cacheEligible := s.semanticCache != nil && !env.Stream() && decision.Metadata != nil && externalID != "" && !bypassEval && !compactionHandoverRan && len(s.subsidyFactors(ctx, r.Header)) == 0
	if cacheEligible {
		if resp, hit := s.semanticCache.Lookup(externalID, cache.FormatAnthropic, decision.Metadata.Embedding, decision.Metadata.ClusterIDs, decision.Metadata.ClusterRouterVersion, decision.Metadata.EffectiveKnobsHash); hit {
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

	w.Header().Set(HeaderRouterDecision, decision.Reason)
	w.Header().Set(HeaderRouterProvider, decision.Provider)
	w.Header().Set(HeaderRouterModel, decision.Model)
	s.setFeedbackLinkHeader(w, installationID, externalID, requestID, auth.UserIDFrom(ctx))

	if _, err := s.provider(decision.Provider); err != nil {
		return err
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
		Float64("catalog.requested_input_per_1m", reqPricing.InputUSDPer1M).
		Float64("catalog.requested_output_per_1m", reqPricing.OutputUSDPer1M).
		Float64("catalog.actual_input_per_1m", actPricing.InputUSDPer1M).
		Float64("catalog.actual_output_per_1m", actPricing.OutputUSDPer1M).
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
		TargetModel:           decision.Model,
		TargetProvider:        decision.Provider,
		Capabilities:          router.Lookup(decision.Model),
		IncludeStreamUsage:    s.usageRequired(),
		SessionAffinity:       sessionAffinityHint(routeRes.SessionKey),
		ModelSwitched:         routeRes.modelSwitched(),
		EnableExtendedContext: shouldEnableExtendedContext(env.FullTokenEstimate(), outputReserve),
	}
	if s.effortEscalation {
		opts.ForceReasoningEffort = forcedReasoningEffort(decision.Model, routeRes.EscalateEffort)
	}

	// A caller whose Claude subscription has bound its plan window can't serve
	// another turn on it (429 until reset). Suppress the spent token so
	// resolution falls through to the deployment/BYOK key — the turn serves on
	// the Weave key (full cost) instead of hard-failing. Only fires once the
	// observer has recorded exhaustion and a fallback key exists.
	if s.claudeSubscriptionExhausted(ctx, r.Header) {
		ctx = withSuppressedClaudeSubscription(ctx)
	}
	ctx = resolveAndInjectCredentials(ctx, decision.Provider, r.Header)

	// Wrap every request (not just multi-binding) in a preludeBuffer so a
	// pre-first-byte upstream error can discard the buffered prelude (marker +
	// message_start) and render an error envelope instead of stranding the
	// marker on the wire. Single-binding requests used to skip this for TTFB,
	// but the v0.58 SWE-bench bake-off traced 46/84 empty-patch failures to
	// exactly that: an api_error left Claude Code with only marker text and no
	// tool_use. Cost: one round-trip's buffered SSE bytes (~200B).
	bindings := s.resolveBindingsForDispatch(ctx, decision)
	// Append the one-click feedback thumbs as a trailing content block,
	// wrapped below the capture layer so the footer never lands in
	// cached/logged bodies. Transparent when streaming/feedback is off.
	clientSink := w
	if env.Stream() {
		if footer := s.feedbackFooter(ClientIdentityFrom(ctx).ClientApp, routeRes.TurnType); footer != "" {
			clientSink = translate.NewAnthropicRoutingFooterWriter(w, footer)
		}
	}
	contentSink, contentCap := s.maybeCaptureResponse(clientSink)
	preludeBuf := newPreludeBuffer(contentSink)
	var rootSink http.ResponseWriter = preludeBuf
	var captureW *captureWriter
	var sink http.ResponseWriter = rootSink
	if cacheEligible {
		captureW = newCaptureWriter(rootSink, semanticCacheMaxBodyBytes)
		sink = captureW
	}

	proxyStart := time.Now()
	var proxyErr error
	crossFormat := false
	var extractor *otel.UsageExtractor
	// respSummary captures the winning attempt's translated-response signals
	// for the completion log. Populated by translator-backed paths; stays
	// zero for Anthropic-native passthrough (no translator).
	var respSummary translate.ResponseSummary
	// reqStats captures translation-time mutations on the winning attempt's
	// request body. Zero for Anthropic-native passthrough.
	var reqStats providers.RequestMutationStats

	marker := suppressMarkerIfRequested(r.Header, routingMarkerFor(routeRes))
	// toolValidator compiles the request's tool schemas once (LRU-cached);
	// translators validate/repair model tool calls against it. Nil if no tools.
	toolValidator := env.ToolValidator()
	setExtractor := func(e *otel.UsageExtractor) { extractor = e }
	var attempt dispatchAttempt
	// Dispatch keys off the provider's translation family, not a hardcoded name
	// list, so a new OpenAI-compat provider routes here as soon as it has a
	// ProviderFamilies entry (see internal/providers/provider.go).
	switch providers.FamilyFor(decision.Provider) {
	case providers.FamilyAnthropic:
		prep, emitErr := env.PrepareAnthropic(r.Header, opts)
		if emitErr != nil {
			log.Error("Failed to emit Anthropic body", "err", emitErr)
			return fmt.Errorf("emit body: %w", emitErr)
		}
		logUpstreamBody(log, routeRes.SessionKey, decision, feats, prep.Body)
		attempt = s.anthropicNativeAttempt(env, r, prep, sink, preludeBuf, marker, setExtractor)
	case providers.FamilyOpenAICompat:
		crossFormat = true
		// Prep rebuilt per attempt: targetIsOpenRouter(opts) gates four
		// OpenRouter-only body fields. On failover from Fireworks to
		// OpenRouter, the body must be re-emitted with TargetProvider =
		// openrouter so those gates fire.
		attempt = func(actx context.Context, d router.Decision, p providers.Client) error {
			attemptOpts := opts
			attemptOpts.TargetProvider = d.Provider
			respSummary = translate.ResponseSummary{}
			// Reasoning OpenAI models (gpt-5.x) reject tools/stop/reasoning_effort
			// on /v1/chat/completions; agentic tool turns must use Responses
			// instead. Scoped to direct OpenAI (the only one with /v1/responses).
			useResponses := translate.UseOpenAIResponsesAPI(
				d.Provider, attemptOpts.Capabilities, feats.HasTools)
			var prep providers.PreparedRequest
			var emitErr error
			if useResponses {
				prep, emitErr = env.PrepareOpenAIResponses(r.Header, attemptOpts)
			} else {
				prep, emitErr = env.PrepareOpenAI(r.Header, attemptOpts)
			}
			if emitErr != nil {
				log.Error("Failed to translate Anthropic request to OpenAI format", "err", emitErr, "decision_provider", d.Provider, "responses_api", useResponses)
				return fmt.Errorf("translate anthropic request: %w", emitErr)
			}
			reqStats = prep.Stats
			logUpstreamBody(log, routeRes.SessionKey, d, feats, prep.Body)
			var usage otel.UsageSink
			if s.usageRequired() {
				extractor = otel.NewUsageExtractor(nil, d.Provider)
				usage = extractor
			}
			var translator translate.ResponseTranslator
			if useResponses {
				translator = translate.NewResponsesToAnthropicWriter(sink, d.Model, usage).
					WithRoutingMarker(suppressMarkerIfRequested(r.Header, routingMarkerFor(routeRes))).
					WithEstimatedInputTokens(feats.Tokens).
					WithRequestHadTools(feats.HasTools).
					WithToolValidator(toolValidator)
			} else {
				translator = translate.NewAnthropicSSETranslator(sink, d.Model, usage).
					WithRoutingMarker(suppressMarkerIfRequested(r.Header, routingMarkerFor(routeRes))).
					WithEstimatedInputTokens(feats.Tokens).
					WithRequestHadTools(feats.HasTools).
					WithThinkTagReasoning(catalog.ThinkTagReasoningFor(d.Model)).
					WithEscapeNormalize(s.escapeNormalize).
					WithToolValidator(toolValidator)
			}
			if err := translator.Prelude(env.Stream()); err != nil {
				log.Error("Anthropic SSE prelude failed (OpenAI upstream)", "err", err)
			}
			if preludeBuf != nil {
				preludeBuf.Seal()
			}
			err := p.Proxy(actx, d, prep, translator, r)
			// Post-commit: HTTP 200 + message_start already on the wire, so
			// render the error as an in-stream `event: error` frame instead of
			// a corrupting trailing envelope. Pre-commit errors go through
			// dispatchWithFallback instead.
			if err != nil && env.Stream() && preludeBuf.Committed() {
				err = emitAnthropicSSEErrorEvent(sink, err)
			}
			finErr := finalizeAfterProxy(err, translator.Finalize)
			respSummary = translator.Summary()
			return finErr
		}
	case providers.FamilyGemini:
		crossFormat = true
		prep, emitErr := env.PrepareGemini(r.Header, opts)
		reqStats = prep.Stats
		if emitErr != nil {
			log.Error("Failed to translate Anthropic request to Gemini format", "err", emitErr)
			return fmt.Errorf("translate anthropic request to gemini: %w", emitErr)
		}
		logUpstreamBody(log, routeRes.SessionKey, decision, feats, prep.Body)
		// geminiUsedValidated marks a request sent with
		// functionCallingConfig.mode=VALIDATED (Gemini 3.x, tools, unforced
		// choice): Gemini compiles each tool schema into a decode-time grammar,
		// and one it can't compile 400s the whole request. Retried once below
		// with mode=AUTO if nothing has reached the client yet.
		geminiUsedValidated := prep.Stats.GeminiValidatedToolMode
		// dispatchGemini does one call and returns the raw upstream error plus a
		// finalize thunk, split so the attempt can inspect a pre-commit 400
		// before finalize commits the prelude buffer and forecloses the retry.
		// Translators are stateful, so a retry rebuilds the chain via a fresh call.
		dispatchGemini := func(actx context.Context, d router.Decision, p providers.Client, pr providers.PreparedRequest) (error, func(error) error) {
			respSummary = translate.ResponseSummary{}
			var usage otel.UsageSink
			if s.usageRequired() {
				extractor = otel.NewUsageExtractor(nil, d.Provider)
				usage = extractor
			}
			// SSE chain: Gemini → OpenAI → Anthropic.
			anthropicTr := translate.NewAnthropicSSETranslator(sink, d.Model, usage).
				WithRoutingMarker(suppressMarkerIfRequested(r.Header, routingMarkerFor(routeRes))).
				WithEstimatedInputTokens(feats.Tokens).
				WithRequestHadTools(feats.HasTools).
				WithEscapeNormalize(s.escapeNormalize).
				WithToolValidator(toolValidator)
			if err := anthropicTr.Prelude(env.Stream()); err != nil {
				log.Error("Anthropic SSE prelude failed (Gemini upstream)", "err", err)
			}
			if preludeBuf != nil {
				preludeBuf.Seal()
			}
			geminiTr := translate.NewGeminiToOpenAISSETranslator(anthropicTr, d.Model, nil)
			rawErr := p.Proxy(actx, d, pr, geminiTr, r)
			finalize := func(err error) error {
				// Post-commit: see the OpenAI-compat case above.
				if err != nil && env.Stream() && preludeBuf.Committed() {
					err = emitAnthropicSSEErrorEvent(sink, err)
				}
				err = finalizeAfterProxy(err, geminiTr.Finalize)
				finErr := finalizeAfterProxy(err, anthropicTr.Finalize)
				respSummary = anthropicTr.Summary()
				return finErr
			}
			return rawErr, finalize
		}
		attempt = func(actx context.Context, d router.Decision, p providers.Client) error {
			rawErr, finalize := dispatchGemini(actx, d, p, prep)
			// VALIDATED-mode schema-grammar 400: retry once with mode=AUTO while
			// pre-commit. AUTO only drops the grammar constraint, so it can't make
			// things worse — a non-schema 400 just 400s again normally. The first
			// attempt's translators are abandoned (Discard).
			if rawErr != nil && geminiUsedValidated && !committed(preludeBuf) && upstreamStatus(rawErr) == http.StatusBadRequest {
				autoOpts := opts
				autoOpts.DowngradeGeminiValidatedToAuto = true
				autoPrep, autoErr := env.PrepareGemini(r.Header, autoOpts)
				if autoErr != nil {
					log.Error("Failed to re-translate Gemini request with tool mode AUTO", "err", autoErr)
					return finalize(rawErr)
				}
				log.Warn("Retrying Gemini request with functionCallingConfig.mode=AUTO after VALIDATED-mode 400",
					"model", d.Model,
					"request_id", requestID)
				if preludeBuf != nil {
					preludeBuf.Discard()
				}
				reqStats = autoPrep.Stats
				logUpstreamBody(log, routeRes.SessionKey, d, feats, autoPrep.Body)
				rawErr, finalize = dispatchGemini(actx, d, p, autoPrep)
			}
			return finalize(rawErr)
		}
	default:
		return fmt.Errorf("%w: %s (no translation path defined for inbound Anthropic Messages)", ErrProviderNotConfigured, decision.Provider)
	}

	// In-turn baseline failover eligibility: when the router cost-routes to an
	// OSS/Gemini model and every binding fails, fall back to the requested
	// model on Anthropic instead of hard-failing. Eligible only when: not
	// BYOK/inbound-credential bound (those resolve to a single provider),
	// Anthropic isn't excluded for the installation (else failing over would
	// violate the exclusion contract), the routed model isn't already
	// Anthropic, and the baseline is a distinct known Anthropic catalog model.
	// Computed pre-dispatch so the primary dispatch defers its exhaustion flush.
	baselineModel := s.baselineFor(feats.Model)
	baselineCatalog, baselineKnown := catalog.ByID(baselineModel)
	_, anthropicExcluded := s.excludedProvidersForRequest(ctx)[providers.ProviderAnthropic]
	baselineEligible := s.shouldFailover(ctx) &&
		!anthropicExcluded &&
		decision.Provider != providers.ProviderAnthropic &&
		baselineModel != decision.Model &&
		baselineKnown && baselineCatalog.PrimaryProvider() == providers.ProviderAnthropic

	// Subscription-credit failover eligibility. A Claude turn served on the
	// caller's subscription (sk-ant-oat) is pinned to a single Anthropic
	// binding, so a retryable 429/timeout has nowhere to fail over to and
	// reaches the client raw. This is the gap behind prod instability: the
	// observer-driven exhaustion suppression above only fires once a PRIOR
	// snapshot already read exhausted, but the binding 429 is usually the
	// first signal — the stale snapshot still reads "slack".
	//
	// When a non-subscription Anthropic key exists (BYOK or deployment), retry
	// the same model on it once: a retryable 429 on the subscription is served
	// on the Weave key (full cost) rather than surfaced raw — the same
	// fallback claudeSubscriptionExhausted takes pre-emptively, just driven by
	// the live error instead of a stale snapshot. Eligible only pre-commit, on
	// a subscription-served Anthropic turn, with a fallback key available.
	// Mutually exclusive with baselineEligible (non-Anthropic routed provider).
	subscriptionRetryEligible := decision.Provider == providers.ProviderAnthropic &&
		servedOnSubscription(ctx) &&
		s.anthropicFallbackKeyAvailable(ctx)

	primaryProvider := decision.Provider
	var winnerIdx int
	winnerIdx, proxyErr = s.dispatchWithFallback(ctx, failoverInputs{
		// contentSink is the raw w when capture is off.
		w:                      contentSink,
		buf:                    preludeBuf,
		initialDecision:        decision,
		bindings:               bindings,
		attempt:                attempt,
		flushErr:               flushUpstreamErrorAsAnthropic,
		deferFlushOnExhaustion: baselineEligible || subscriptionRetryEligible,
	})

	// The routed model's bindings all failed with a fault another model could
	// satisfy, pre-commit — re-dispatch the requested model on Anthropic.
	// crossFormat/respSummary/reqStats reset to Anthropic-native values so
	// telemetry reflects the binding that actually served.
	baselineFailoverUsed := false
	baselineAttempted := false
	if baselineEligible && proxyErr != nil && !preludeBuf.Committed() &&
		(providers.IsRetryable(proxyErr) || providers.IsUpstreamModelNotFound(proxyErr)) {
		baselineDecision := decision
		baselineDecision.Model = baselineModel
		baselineDecision.Provider = providers.ProviderAnthropic
		baselineOpts := opts
		baselineOpts.TargetModel = baselineModel
		baselineOpts.TargetProvider = providers.ProviderAnthropic
		baselineOpts.Capabilities = router.Lookup(baselineModel)
		// Recompute against the model that actually serves, not the cost-routed
		// OSS id — otherwise PrepareAnthropic may leave stale signed thinking
		// blocks the baseline model rejects (400).
		baselineOpts.ModelSwitched = routeRes.PriorServedModel != baselineModel || routeRes.SessionEverSwitched
		if s.effortEscalation {
			baselineOpts.ForceReasoningEffort = forcedReasoningEffort(baselineModel, routeRes.EscalateEffort)
		}
		baselinePrep, baselineEmitErr := env.PrepareAnthropic(r.Header, baselineOpts)
		if baselineEmitErr != nil {
			log.Error("Baseline failover: emit Anthropic body failed; surfacing original error", "err", baselineEmitErr, "baseline_model", baselineModel)
			flushUpstreamErrorAsAnthropic(contentSink, proxyErr)
		} else {
			log.Warn("Baseline failover: routed model exhausted, retrying requested model on Anthropic",
				"failed_model", decision.Model,
				"failed_provider", primaryProvider,
				"baseline_model", baselineModel,
				"err", proxyErr)
			baselineCtx := ctx
			if s.claudeSubscriptionExhausted(ctx, r.Header) {
				baselineCtx = withSuppressedClaudeSubscription(baselineCtx)
			}
			baselineCtx = resolveAndInjectCredentials(baselineCtx, providers.ProviderAnthropic, r.Header)
			baselineBindings := s.resolveBindingsForDispatch(baselineCtx, baselineDecision)
			baselineMarker := suppressMarkerIfRequested(r.Header, baselineRoutingMarkerFor(routeRes, baselineModel))
			baselineAttempt := s.anthropicNativeAttempt(env, r, baselinePrep, sink, preludeBuf, baselineMarker, setExtractor)
			crossFormat = false
			respSummary = translate.ResponseSummary{}
			reqStats = providers.RequestMutationStats{}
			logUpstreamBody(log, routeRes.SessionKey, baselineDecision, feats, baselinePrep.Body)
			winnerIdx, proxyErr = s.dispatchWithFallback(baselineCtx, failoverInputs{
				w:               contentSink,
				buf:             preludeBuf,
				initialDecision: baselineDecision,
				bindings:        baselineBindings,
				attempt:         baselineAttempt,
				flushErr:        flushUpstreamErrorAsAnthropic,
			})
			decision = baselineDecision
			bindings = baselineBindings
			baselineAttempted = true
			// Reflect whether the baseline actually served — a failed retry must
			// not report baseline_failover=true and skew bake-off analysis.
			baselineFailoverUsed = proxyErr == nil
		}
	} else if baselineEligible && proxyErr != nil {
		// Baseline didn't run (mid-stream commit, or non-failoverable error);
		// surface the deferred original error now.
		flushUpstreamErrorAsAnthropic(contentSink, proxyErr)
	}

	// Subscription-credit failover: suppress the OAuth token and retry the SAME
	// model once on the Weave/BYOK key when a subscription-served Anthropic turn
	// hit a transient fault (429/timeout) or an OAuth rejection (401/403),
	// pre-commit. Skipped when baseline failover already ran (non-Anthropic).
	subscriptionFailoverUsed := false
	subscriptionRetryRan := false
	if subscriptionRetryEligible && !baselineAttempted && proxyErr != nil &&
		!preludeBuf.Committed() &&
		(providers.IsRetryable(proxyErr) || anthropicOAuthCredentialRejected(proxyErr)) {
		subscriptionRetryRan = true
		subCtx := withSuppressedClaudeSubscription(ctx)
		subCtx = resolveAndInjectCredentials(subCtx, providers.ProviderAnthropic, r.Header)
		// Model is unchanged, but rebuild prep so the retry gets a pristine
		// PreparedRequest under the suppressed-subscription context.
		subPrep, subEmitErr := env.PrepareAnthropic(r.Header, opts)
		if subEmitErr != nil {
			log.Error("Subscription failover: emit Anthropic body failed; surfacing original error", "err", subEmitErr, "model", decision.Model)
			flushUpstreamErrorAsAnthropic(contentSink, proxyErr)
		} else if subBindings := s.resolveBindingsForDispatch(subCtx, decision); len(subBindings) == 0 {
			// No usable Anthropic binding under suppression — surface the
			// original retryable error (real throttle) rather than a synthetic
			// 502 that would mask it. No Weave key attempted, so attribution
			// stays on the subscription.
			log.Warn("Subscription failover: no fallback Anthropic binding available; surfacing original error",
				"model", decision.Model,
				"err", proxyErr,
				"upstream_status", upstreamStatus(proxyErr))
			flushUpstreamErrorAsAnthropic(contentSink, proxyErr)
		} else {
			log.Warn("Subscription failover: subscription throttled/timed out, retrying requested model on Weave key",
				"model", decision.Model,
				"err", proxyErr,
				"upstream_status", upstreamStatus(proxyErr))
			subAttempt := s.anthropicNativeAttempt(env, r, subPrep, sink, preludeBuf, marker, setExtractor)
			crossFormat = false
			respSummary = translate.ResponseSummary{}
			reqStats = providers.RequestMutationStats{}
			logUpstreamBody(log, routeRes.SessionKey, decision, feats, subPrep.Body)
			winnerIdx, proxyErr = s.dispatchWithFallback(subCtx, failoverInputs{
				w:               contentSink,
				buf:             preludeBuf,
				initialDecision: decision,
				bindings:        subBindings,
				attempt:         subAttempt,
				flushErr:        flushUpstreamErrorAsAnthropic,
			})
			bindings = subBindings
			subscriptionFailoverUsed = proxyErr == nil
		}
	}
	// The subscription retry didn't run (mid-stream commit, or non-retryable
	// error); surface the deferred original error now so it's never dropped.
	if subscriptionRetryEligible && !baselineAttempted && !subscriptionRetryRan && proxyErr != nil && !preludeBuf.Committed() {
		flushUpstreamErrorAsAnthropic(contentSink, proxyErr)
	}

	finalProvider := primaryProvider
	if winnerIdx >= 0 && winnerIdx < len(bindings) {
		finalProvider = bindings[winnerIdx].Provider
	} else if baselineAttempted {
		// Baseline ran but no binding served (winnerIdx == -1); the last
		// attempt was Anthropic with the baseline model, so finalProvider must
		// not revert to the OSS primary that never served it.
		finalProvider = providers.ProviderAnthropic
	}
	decision.Provider = finalProvider

	// Re-resolve credentials for the binding that actually served — each
	// failover attempt gets its own context. Carry the suppression forward on
	// subscriptionFailoverUsed (not subscriptionRetryRan) so cost.subscription_served
	// and the billing key reflect the Weave key that actually paid, not the
	// spent subscription — but only once the Weave retry actually succeeded.
	if subscriptionFailoverUsed {
		ctx = withSuppressedClaudeSubscription(ctx)
	}
	ctx = resolveAndInjectCredentials(ctx, finalProvider, r.Header)

	// Re-resolve pricing for the binding that actually served: the
	// pre-dispatch lookup always returns the catalog's PRIMARY binding price,
	// which would misreport cost after a successful failover to a different
	// binding's rate.
	if actBindingPricing, ok := catalog.PriceFor(finalProvider, decision.Model); ok {
		actPricing = actBindingPricing
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
			s.semanticCache.Store(externalID, cache.FormatAnthropic, decision.Metadata.Embedding, decision.Metadata.ClusterIDs[0], storeResp, decision.Metadata.ClusterRouterVersion, decision.Metadata.EffectiveKnobsHash)
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
		String("requested.model", feats.Model).
		String("decision.model", decision.Model).
		String("decision.provider", finalProvider).
		String("decision.reason", decision.Reason).
		String("routing.turn_type", string(routeRes.TurnType)).
		String("upstream.finish_reason", respSummary.UpstreamFinishReason).
		String("upstream.stop_reason", respSummary.StopReason).
		Int64("usage.input_tokens", int64(in)).
		Int64("usage.output_tokens", int64(out)).
		Int64("usage.cache_creation_input_tokens", int64(cacheCreation)).
		Int64("usage.cache_read_input_tokens", int64(cacheRead)).
		Float64("cost.requested_input_usd", catalog.EffectiveInputCost(in, cacheCreation, cacheRead, reqPricing.InputUSDPer1M, reqPricing, decision.Provider)).
		Float64("cost.requested_output_usd", catalog.EffectiveOutputCost(out, reqPricing.OutputUSDPer1M)).
		Float64("cost.actual_input_usd", catalog.EffectiveInputCost(in, cacheCreation, cacheRead, actPricing.InputUSDPer1M, actPricing, decision.Provider)).
		Float64("cost.actual_output_usd", catalog.EffectiveOutputCost(out, actPricing.OutputUSDPer1M)).
		Bool("cost.subscription_served", servedOnSubscription(ctx)).
		Int64("latency.upstream_ms", proxyMs).
		Int64("latency.total_ms", time.Since(requestStart).Milliseconds()).
		Int64("upstream.status_code", int64(upstreamStatus(proxyErr))).
		Bool("routing.cross_format", crossFormat).
		String("dispatch.primary_provider", primaryProvider).
		String("dispatch.final_provider", finalProvider).
		Int64("dispatch.fallback_attempts", int64(winnerIdx)).
		Bool("dispatch.failover_used", finalProvider != primaryProvider || subscriptionFailoverUsed).
		Bool("dispatch.baseline_failover", baselineFailoverUsed).
		Bool("dispatch.subscription_failover", subscriptionFailoverUsed)
	applyPlannerAttrs(upstreamBuilder, routeRes)
	addTimingAttrs(ctx, upstreamBuilder)

	obs := buildObservationContext(ctx, decision, routeRes.Fresh)
	obs.applySpanAttrs(upstreamBuilder)

	otel.Record(ctx, otel.Span{
		Name:  "router.upstream",
		Start: proxyStart,
		End:   time.Now(),
		Attrs: upstreamBuilder.Build(),
	})
	respBody, respTrunc := capturedResponse(contentCap)
	s.recordCallLog(ctx, upstreamBuilder.Build(), proxyErr != nil, body, respBody, respTrunc)
	otel.Flush(ctx)

	s.recordTurnUsage(routeRes, decision.Model, in, out, cacheCreation, cacheRead)

	if installationID != uuid.Nil {
		credentialKeyPrefix, credentialKeySuffix, credSource := s.credentialKeyParts(ctx)
		// Same-provider subscription->Weave retries keep finalProvider ==
		// primaryProvider, so OR in subscriptionFailoverUsed to match the OTel
		// span + completion log.
		failoverUsed := finalProvider != primaryProvider || subscriptionFailoverUsed
		degShadow := proxyErr == nil && isDegenerateResponse(out, respSummary.ToolUseBlocks, respSummary.StopReason, respSummary.StopReasonDemoted)
		if degShadow {
			log.Info("router.degenerate_shadow",
				"model", decision.Model,
				"provider", finalProvider,
				"output_tokens", out,
				"tool_use_blocks", respSummary.ToolUseBlocks,
				"stop_reason", respSummary.StopReason,
				"upstream_finish_reason", respSummary.UpstreamFinishReason,
				"would_failover", true,
			)
			// Evict the pin so the next turn re-scores instead of repeating the
			// same misbehaving model — this turn already streamed and can't retry.
			s.evictPinAfterDegenerateResponse(ctx, stickyHit, decision.Reason, installationID, routeRes.SessionKey, routeRes.PinRole)
		}
		s.fireTelemetry(InsertTelemetryParams{
			InstallationID:         installationID.String(),
			APIKeyID:               apiKeyIDFromContext(ctx),
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
			RequestedInputCostUSD:  catalog.EffectiveInputCost(in, cacheCreation, cacheRead, reqPricing.InputUSDPer1M, reqPricing, decision.Provider),
			RequestedOutputCostUSD: catalog.EffectiveOutputCost(out, reqPricing.OutputUSDPer1M),
			ActualInputCostUSD:     catalog.EffectiveInputCost(in, cacheCreation, cacheRead, actPricing.InputUSDPer1M, actPricing, decision.Provider),
			ActualOutputCostUSD:    catalog.EffectiveOutputCost(out, actPricing.OutputUSDPer1M),
			RouteLatencyMs:         routeMs,
			UpstreamLatencyMs:      proxyMs,
			TotalLatencyMs:         time.Since(requestStart).Milliseconds(),
			CrossFormat:            crossFormat,
			UpstreamStatusCode:     int32(upstreamStatus(proxyErr)),
			ClusterIDs:             obs.ClusterIDs,
			CandidateModels:        obs.CandidateModels,
			ChosenScore:            obs.ChosenScore,
			CandidateScores:        obs.CandidateScores,
			Propensity:             obs.Propensity,
			ClusterRouterVersion:   obs.ClusterRouterVersion,
			TTFTMs:                 obs.TTFTMs,
			CacheCreationTokens:    cacheTokenPtr(cacheCreation),
			CacheReadTokens:        cacheTokenPtr(cacheRead),
			DeviceID:               clientID.DeviceID,
			SessionID:              clientID.SessionID,
			RouterUserID:           auth.UserIDFrom(ctx),
			ClientApp:              clientID.ClientApp,
			TurnType:               string(routeRes.TurnType),
			RolloutID:              clientID.RolloutID,
			UpstreamFinishReason:   stringPtrOrEmpty(respSummary.UpstreamFinishReason),
			StopReason:             stringPtrOrEmpty(respSummary.StopReason),
			// Only valid when a translator ran (StopReason populated) — the
			// Anthropic-native passthrough path leaves respSummary zero, which
			// must not look like a measured zero-tool turn.
			ToolUseBlocks:         int32PtrIfKnown(int32(respSummary.ToolUseBlocks), respSummary.StopReason != ""),
			InvalidToolArgsBlocks: int32PtrIfKnown(int32(respSummary.InvalidToolArgsBlocks), respSummary.StopReason != ""),
			FailoverUsed:          boolPtrTrue(failoverUsed),
			DegenerateShadow:      boolPtrOrNil(degShadow),
			// (session_key, role) is the offline join key to spiral_shadow_events
			// and session_pins. sessionKey is the bindRequestLogger digest, computed
			// unconditionally so it's populated even when routeRes.SessionKey stays
			// zero (hard-pin / no-pin-store paths); equal byte-for-byte on the
			// main_loop/tool_result turns spiral actually writes.
			SessionKey: sessionKey[:],
			Role:       routeRes.PinRole,
			// Shadow-mode hysteresis instrumentation: fresh scorer's pick + score
			// vector (captured even on STAY) and the loaded pin's age, so the
			// downgrade opportunity is measurable offline. No routing action taken.
			FreshDecisionModel:   obs.FreshDecisionModel,
			FreshCandidateScores: obs.FreshCandidateScores,
			PinAgeSec:            int64PtrIf(stickyHit && pinAgeSec > 0, pinAgeSec),
			// Shadow-mode tier-cap instrumentation: tool-output size on
			// tool_result turns. NULL elsewhere. No routing action taken.
			ToolResultBytes: toolResultBytesPtr(inboundLastUser, tt),
			// Credential attribution: safe display key parts, so a shared
			// subscription (one account, many seats) shows via equal
			// prefix/suffix across router_user_ids.
			CredentialKeyPrefix: credentialKeyPrefix,
			CredentialKeySuffix: credentialKeySuffix,
			CredentialSource:    credSource,
		})
	}

	// No-op when billing is unwired (selfhosted); only reached on a real
	// upstream call since the cache-hit branch above already returned.
	if proxyErr == nil {
		s.emitBilling(ctx, requestID, externalID, decision, actPricing, routeRes, in, out, cacheCreation, cacheRead)
		if compRes.Summarized {
			s.billCompactionSummary(ctx, requestID, externalID, compRes.SummaryUsage)
		}
		if compactionHandoverOutcome.Invoked && !compactionHandoverOutcome.FallbackToFullHistory {
			sumUsage := compactionHandoverOutcome.SummaryUsage
			if sumUsage.Model != "" && (sumUsage.InputTokens > 0 || sumUsage.OutputTokens > 0) {
				sumPricing, _ := catalog.PrimaryPriceFor(sumUsage.Model)
				apiKeyID, _ := ctx.Value(APIKeyIDContextKey{}).(string)
				s.fireBilling(ctx, billing.DebitInferenceParams{
					OrganizationID:  externalID,
					RouterRequestID: requestID + "_compaction_summary",
					Model:           sumUsage.Model,
					Provider:        sumUsage.Provider,
					InputTokens:     sumUsage.InputTokens,
					OutputTokens:    sumUsage.OutputTokens,
					CacheCreation:   sumUsage.CacheCreation,
					CacheRead:       sumUsage.CacheRead,
					Pricing:         sumPricing,
					HasOverride:     billing.HasOverrideFromContext(ctx),
					APIKeyID:        apiKeyID,
				})
			}
		}
	}

	// Two-strike eviction: a session pinned to a model returning non-retryable
	// 4xx wedges until manually /force-model'd out. Expires the pin after a
	// persistent counter hits threshold; successful turns reset it.
	s.maybeEvictPinAfterUpstreamErr(ctx, stickyHit, proxyErr, decision.Reason, installationID, routeRes.SessionKey, routeRes.PinRole)

	// One event per tool_use block that failed toolcheck validation, including
	// repaired ones — doubles as a per-model×provider tool-calling-quality signal.
	for _, iss := range respSummary.ToolCallIssues {
		log.Info("router.tool_call_invalid",
			"tool_name", iss.ToolName,
			"failure_bucket", string(iss.Bucket),
			"detail", iss.Detail,
			"repaired", iss.Repaired,
			"repair_actions", iss.Actions,
			"model", decision.Model,
			"provider", finalProvider,
			"session_key_prefix", shortSessionKey(routeRes.SessionKey),
		)
	}

	log.Info("ProxyMessages complete", "requested_model", feats.Model, "baseline_model", s.baselineFor(feats.Model), "decision_model", decision.Model, "decision_provider", decision.Provider, "primary_provider", primaryProvider, "fallback_attempts", winnerIdx, "failover_used", finalProvider != primaryProvider || subscriptionFailoverUsed, "subscription_failover", subscriptionFailoverUsed, "decision_reason", decision.Reason, "requested_tier", routeRes.RequestedTier.String(), "decision_tier", catalog.TierFor(decision.Model).String(), "embedded_tokens", len(promptText)/4, "total_input_tokens", feats.Tokens, "has_tools", feats.HasTools, "message_count", feats.MessageCount, "last_kind", feats.LastKind, "last_preview", feats.LastPreview, "embed_input", embedInput, "cross_format", crossFormat, "sticky_hit", stickyHit, "route_ms", routeMs, "proxy_ms", proxyMs, "proxy_err", proxyErr, "upstream_status", upstreamStatus(proxyErr), "upstream_finish_reason", respSummary.UpstreamFinishReason, "resp_stop_reason", respSummary.StopReason, "stop_reason_promoted", respSummary.StopReasonPromoted, "tool_use_blocks", respSummary.ToolUseBlocks, "invalid_tool_args_blocks", respSummary.InvalidToolArgsBlocks, "text_only_turn_nudged", respSummary.TextOnlyTurnNudged, "stop_reason_demoted", respSummary.StopReasonDemoted, "suppressed_tool_calls", respSummary.SuppressedToolCalls, "tool_call_invalid_blocks", len(respSummary.ToolCallIssues), "cc_only_tools_stripped", reqStats.CCOnlyToolsStripped, "gemini_reminder_injected", reqStats.GeminiReminderInjected, "gemini_validated_tool_mode", reqStats.GeminiValidatedToolMode, "resp_output_tokens", respSummary.OutputTokens, "prelude_committed", preludeBuf.Committed())
	s.reportHMMOutcome(ctx, routeRes, decision, finalProvider, feats.Tokens, in, out, cacheCreation, cacheRead, routeMs, proxyMs, proxyErr)
	return proxyErr
}

// applyPlannerAttrs stamps planner and handover attributes onto a span
// attribute builder. Safe when the planner didn't run (uses "skipped" outcome).
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
		Bool("planner.pin_cache_warm", !res.PlannerDecision.PinCacheCold).
		Bool("handover.invoked", res.Handover.Invoked).
		Int64("handover.latency_ms", res.Handover.LatencyMS).
		Int64("handover.summary_tokens", int64(res.Handover.SummaryTokens)).
		Bool("handover.fallback_to_full_history", res.Handover.FallbackToFullHistory)
	return b
}

// plannerOutcomeAttr maps the planner's typed outcome to an OTel string.
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

// logPlannerOutcome emits a structured log line for the planner's verdict.
// Switch turns are Info; stay turns are Debug.
func (s *Service) logPlannerOutcome(ctx context.Context, res turnLoopResult) {
	if res.PlannerDecision.Reason == "" {
		return
	}
	log := observability.FromContext(ctx)
	if res.PlannerDecision.Outcome == planner.OutcomeSwitch {
		log.Info("router switched models",
			"from", res.PinModel,
			"to", res.Decision.Model,
			"reason", res.PlannerDecision.Reason,
			"expected_savings_usd", res.PlannerDecision.ExpectedSavingsUSD,
			"eviction_cost_usd", res.PlannerDecision.EvictionCostUSD,
			"threshold_usd", res.PlannerDecision.ThresholdUSD,
			"pin_cache_warm", !res.PlannerDecision.PinCacheCold,
			"handover_invoked", res.Handover.Invoked,
			"handover_fallback_to_full_history", res.Handover.FallbackToFullHistory,
			"handover_latency_ms", res.Handover.LatencyMS,
		)
		return
	}
	log.Info("router stayed on pinned model",
		"model", res.Decision.Model,
		"reason", res.PlannerDecision.Reason,
		"expected_savings_usd", res.PlannerDecision.ExpectedSavingsUSD,
		"eviction_cost_usd", res.PlannerDecision.EvictionCostUSD,
		"threshold_usd", res.PlannerDecision.ThresholdUSD,
		"pin_cache_warm", !res.PlannerDecision.PinCacheCold,
	)
}

func (s *Service) recordTurnUsage(res turnLoopResult, servedModel string, in, out, cacheCreation, cacheRead int) {
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
		ServedModel:       servedModel,
	}
	role := res.PinRole
	if role == "" {
		role = sessionpin.DefaultRole
	}
	if err := s.pinStore.UpdateUsage(context.Background(), res.SessionKey, role, usage); err != nil {
		observability.Get().Error("session pin usage writeback failed", "err", err)
	}
}

func (s *Service) reportHMMOutcome(ctx context.Context, res turnLoopResult, decision router.Decision, finalProvider string, estimatedInputTokens, inputTokens, outputTokens, cacheCreation, cacheRead int, routeMs, proxyMs int64, proxyErr error) {
	if s.hmmOutcomeReporter == nil {
		return
	}
	routeDecision := decision
	routeMetadata := decision.Metadata
	if routeMetadata == nil || routeMetadata.Strategy != string(router.StrategyHMM) || routeMetadata.RouteID == "" {
		routeDecision = res.Fresh
		routeMetadata = res.Fresh.Metadata
	}
	if routeMetadata == nil || routeMetadata.Strategy != string(router.StrategyHMM) || routeMetadata.RouteID == "" {
		return
	}
	payload := map[string]interface{}{
		"route_id":               routeMetadata.RouteID,
		"strategy":               routeMetadata.Strategy,
		"served_model":           decision.Model,
		"served_provider":        finalProvider,
		"decision_model":         routeDecision.Model,
		"decision_provider":      routeDecision.Provider,
		"status":                 upstreamStatus(proxyErr),
		"error":                  "",
		"estimated_input_tokens": estimatedInputTokens,
		"input_tokens":           inputTokens,
		"output_tokens":          outputTokens,
		"cache_creation_tokens":  cacheCreation,
		"cache_read_tokens":      cacheRead,
		"route_latency_ms":       routeMs,
		"upstream_latency_ms":    proxyMs,
		"turn_type":              string(res.TurnType),
		"sticky_hit":             res.StickyHit,
	}
	if proxyErr != nil {
		payload["error"] = proxyErr.Error()
	}
	log := observability.FromContext(ctx).With("route_id", routeMetadata.RouteID)
	if err := ctx.Err(); err != nil {
		log.Debug("Skipping HMM outcome report for canceled request", "err", err)
		return
	}
	observability.SafeGo(log, hmmOutcomeReportTimeout, "reportHMMOutcome", func(reportCtx context.Context) {
		if err := s.hmmOutcomeReporter.ReportOutcome(reportCtx, payload); err != nil {
			log.Error("HMM outcome report failed", "err", err)
		}
	})
}

// pinDecision rehydrates a router.Decision from a stored pin. Metadata is nil
// (embedding isn't persisted, acceptable since the pin short-circuits routing).
func pinDecision(p sessionpin.Pin) router.Decision {
	return router.Decision{
		Provider: p.Provider,
		Model:    p.Model,
		Reason:   p.Reason,
	}
}

// bandSwapServed picks which half of a pinned band pair serves this sticky
// turn. Returns the pin's anchor unchanged when the swap head is disabled,
// the pin has no runner-up, the turn isn't MainLoop, the embedding is
// unavailable, prediction fails, or the chosen model isn't servable this
// turn. Otherwise predicts the action from the embedding and serves the
// matching band member (LARGE -> stronger, SMALL -> cheaper). The pin itself
// stays anchored so the pair survives for the next turn's swap.
func (s *Service) bandSwapServed(ctx context.Context, turnType turntype.TurnType, pin sessionpin.Pin, fresh router.Decision, hasImages bool, enabledProviders, excludedModels map[string]struct{}) router.Decision {
	anchor := pinDecision(pin)
	if s.bandSwap == nil || pin.PairedModel == "" || turnType != turntype.MainLoop {
		return anchor
	}
	// Parity guard: the head trains on the user-message-only embedding. If this
	// deploy embeds the full prompt instead, skip rather than feed a skewed input.
	if !s.ResolveEmbedOnlyUserMessage(ctx) {
		return anchor
	}
	if fresh.Metadata == nil || len(fresh.Metadata.Embedding) != bandswap.EmbedDim {
		return anchor
	}
	action, band, ok := s.bandSwap.PredictBand(fresh.Metadata.Embedding)
	if !ok {
		return anchor
	}
	large, small := orderBandPair(pin)
	served := large
	if band == bandswap.Small {
		served = small
	}
	// Only honor a swap when the chosen model is actually servable this turn —
	// same guards turnloop already enforces on the anchor, so a swap can't
	// reach a model the anchor path would have rejected.
	if served.Model != pin.Model {
		if _, available := s.availableModels[served.Model]; !available {
			return anchor
		}
		if hasImages && !catalog.AcceptsImages(served.Model) {
			return anchor
		}
		// The paired model may no longer fit this turn even when the anchor
		// does — serving it would trade a safe anchor for a context error.
		if _, excluded := excludedModels[served.Model]; excluded {
			return anchor
		}
		// nil enabledProviders means "no restriction" (boot behavior), matching
		// turnloop's pin guard.
		if _, registered := s.providers[served.Provider]; !registered {
			return anchor
		}
		if enabledProviders != nil {
			if _, ok := enabledProviders[served.Provider]; !ok {
				return anchor
			}
		}
	}
	observability.FromContext(ctx).Info("band swap served",
		"predicted_action", action,
		"band", band,
		"served_model", served.Model,
		"served_provider", served.Provider,
		"anchor_model", pin.Model,
		"paired_model", pin.PairedModel,
	)
	return served
}

// orderBandPair splits a pin's {Model, PairedModel} into the stronger (large)
// and cheaper (small) member by capability tier, tie-broken by primary input
// price so two same-tier models still get a deterministic split.
func orderBandPair(pin sessionpin.Pin) (large, small router.Decision) {
	a := router.Decision{Provider: pin.Provider, Model: pin.Model, Reason: pin.Reason}
	b := router.Decision{Provider: pin.PairedProvider, Model: pin.PairedModel, Reason: pin.Reason}
	ta, tb := catalog.TierFor(a.Model), catalog.TierFor(b.Model)
	if ta != tb {
		if ta > tb {
			return a, b
		}
		return b, a
	}
	if primaryInputPrice(a.Model) >= primaryInputPrice(b.Model) {
		return a, b
	}
	return b, a
}

func primaryInputPrice(model string) float64 {
	pricing, ok := catalog.PrimaryPriceFor(model)
	if !ok {
		return 0
	}
	return pricing.InputUSDPer1M
}

// clusterIDsFromDecision returns cluster ids from a decision's metadata.
func clusterIDsFromDecision(d router.Decision) []int {
	if d.Metadata == nil {
		return nil
	}
	return d.Metadata.ClusterIDs
}

// pinAge returns seconds since first_pinned_at.
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

// requestUsesNonDeploymentCreds reports whether the request would use BYOK
// or client-supplied creds. The summarizer is wired with deployment-level
// creds, so calling it on a BYOK request would route conversation context
// through the platform account — the orchestrator skips the summarizer here.
func (s *Service) requestUsesNonDeploymentCreds(ctx context.Context, headers http.Header) bool {
	if s.byokOnly {
		return true
	}
	if len(externalKeysFromContext(ctx)) > 0 {
		return true
	}
	// Scan every known provider (not a hand-maintained subset) so a newly-added
	// provider's client-supplied credential can't slip past the BYOK guard.
	for _, p := range providers.AllProviders() {
		if ExtractClientCredentials(p, headers) != nil {
			return true
		}
	}
	return false
}

// enabledProvidersForRequest returns providers with resolvable credentials
// for this request (deployment key, BYOK, or client-supplied header).
// surfaceProvider is the inbound wire-format's natural provider. A
// client-supplied bearer header is treated as creds for that surface only —
// never a licence to enable other OpenAI-compat upstreams sharing the same
// Authorization format.
func (s *Service) enabledProvidersForRequest(ctx context.Context, surfaceProvider string, headers http.Header) map[string]struct{} {
	out := make(map[string]struct{}, len(s.providers))
	if !s.byokOnly {
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
		// Empty plaintext must not enroll the provider — argmax would pick
		// it and the upstream call would 401 with no auth header.
		if len(k.Plaintext) == 0 {
			continue
		}
		out[k.Provider] = struct{}{}
	}
	// A caller's Claude subscription enrolls Anthropic for routing eligibility
	// (mirrors resolveAndInjectCredentials), honored even on router-keyed
	// requests. Without this, a subscription-only request (no BYOK) leaves
	// Anthropic out of the enabled set and the scorer fails with
	// ErrNoEligibleProvider before any Claude turn runs.
	if subscriptionCredsFromHeaderValue(anthropicSubscriptionFromContext(ctx)) != nil {
		out[providers.ProviderAnthropic] = struct{}{}
	}
	// Likewise, a Claude subscription bearer (sk-ant-oat-) in the inbound
	// Authorization enrolls Anthropic even on router-keyed requests — Claude
	// Code keeps its OAuth token there while the router key rides in
	// X-Weave-Router-Key. OAuth-subset only: a general API key still can't
	// enroll a provider on the router-key path.
	if c := ExtractClientCredentials(providers.ProviderAnthropic, headers); c != nil && c.OAuth {
		out[providers.ProviderAnthropic] = struct{}{}
	}
	// A caller's Codex (ChatGPT) subscription enrolls OpenAI, mirroring the
	// Anthropic block above. Requires BOTH token and account-id
	// (codexSubscriptionFromContext returns nil without it) so the scorer
	// can't pick OpenAI for a turn the Codex backend would 401 on.
	if codexSubscriptionFromContext(ctx) != nil {
		out[providers.ProviderOpenAI] = struct{}{}
	}
	// Mirroring the Anthropic inbound-bearer block, a Codex subscription bearer
	// in Authorization (paired with ChatGPT-Account-ID) enrolls OpenAI even on
	// router-keyed requests. OAuth-subset only: a plain API key still can't
	// enroll OpenAI on the router-key path.
	if c := ExtractClientCredentials(providers.ProviderOpenAI, headers); c != nil && c.OAuth {
		out[providers.ProviderOpenAI] = struct{}{}
	}
	// Passthrough-eligible providers are surface-scoped: a provider without a
	// deployment key joins the eligible set only when the inbound surface
	// matches, else an Anthropic-surface `x-api-key` could leak to
	// api.openai.com (and vice versa). Skipped for router-key-authed requests
	// not already BYOK-enrolled: the inbound auth header IS the router key
	// there (stripped by setAuth), so dispatch would 401 unauthenticated
	// instead of failing fast with a 503.
	if surfaceProvider != "" {
		if _, ok := s.passthroughEligibleProviders[surfaceProvider]; ok {
			_, alreadyByok := out[surfaceProvider]
			routerKeyAuthed := installationIDFromContext(ctx) != (uuid.UUID{})
			if !routerKeyAuthed || alreadyByok {
				out[surfaceProvider] = struct{}{}
			}
		}
	}
	// Client-supplied headers are only consulted when NOT authed via a
	// router key. A router-key-authed request carrying an inbound bearer
	// must not enable OpenAI-compat upstreams that share the Authorization
	// header format.
	if installationIDFromContext(ctx) == (uuid.UUID{}) && surfaceProvider != "" {
		if _, already := out[surfaceProvider]; !already {
			if ExtractClientCredentials(surfaceProvider, headers) != nil {
				out[surfaceProvider] = struct{}{}
			}
		}
	}
	// Provider exclusions trump every enrollment path above: an excluded
	// provider must not be served even when credentials exist for it. The
	// scorer, hard-pin resolver, session pins, and tier clamp all consume
	// this set, so subtracting here enforces the exclusion everywhere a
	// routing decision is made.
	for p := range s.excludedProvidersForRequest(ctx) {
		delete(out, p)
	}
	return out
}

// resolveAndInjectCredentials resolves credentials for provider and stashes
// them on ctx, in precedence order: Claude subscription (Anthropic only),
// then BYOK, then a client-supplied header credential.
//
// Subscription-first lets a caller's own Claude subscription pay for Claude
// turns. It arrives via the dedicated X-Weave-Anthropic-Subscription header,
// or (Claude Code routed through the Weave Router) as a sk-ant-oat- bearer
// left in Authorization while the router key rides in X-Weave-Router-Key —
// both honored even on router-keyed requests.
//
// The inbound-bearer path is restricted to the OAuth subset: a general client
// API key is NOT extracted on the router-key path, since that would forward
// the client's inbound key to a different upstream provider. The deployment
// env key is the correct fallback there.
func resolveAndInjectCredentials(ctx context.Context, provider string, headers http.Header) context.Context {
	routerKeyed := installationIDFromContext(ctx) != (uuid.UUID{})
	// When the caller's Claude subscription is observed-exhausted, skip its OAuth
	// token so resolution falls through to BYOK / the deployment key instead of
	// re-hitting a token that will just 429. Anthropic only — a Codex
	// subscription on the same request still pays for its OpenAI turns.
	suppressClaudeSub := claudeSubscriptionSuppressed(ctx)
	if provider == providers.ProviderAnthropic && !suppressClaudeSub {
		// Subscription-first (subscription -> BYOK -> deployment), resolved here
		// explicitly rather than relying on BYOK being absent off the router-key
		// path — a future BYOK-loading path must not silently outrank it.
		if sub := subscriptionCredsFromHeaderValue(anthropicSubscriptionFromContext(ctx)); sub != nil {
			observability.FromContext(ctx).Info("Resolved Claude subscription credential",
				"credential_source", sub.Source)
			return context.WithValue(ctx, CredentialsContextKey{}, sub)
		}
		// A Claude subscription bearer (sk-ant-oat-) in the inbound Authorization
		// is honored even on router-keyed requests: Claude Code keeps its own
		// OAuth token there while the router key rides in X-Weave-Router-Key.
		// Restricted to the OAuth subset — a general API key is still not
		// forwarded on the router-key path (cross-provider-leak guard below).
		if inbound := ExtractClientCredentials(provider, headers); inbound != nil && inbound.OAuth {
			observability.FromContext(ctx).Info("Resolved Claude subscription credential",
				"credential_source", inbound.Source)
			return context.WithValue(ctx, CredentialsContextKey{}, inbound)
		}
	}
	if provider == providers.ProviderOpenAI {
		// Codex (ChatGPT) subscription-first, mirroring the Anthropic block above.
		if sub := codexSubscriptionFromContext(ctx); sub != nil {
			observability.FromContext(ctx).Debug("Resolved Codex subscription credential for OpenAI turn", "credential_source", sub.Source)
			return context.WithValue(ctx, CredentialsContextKey{}, sub)
		}
		// A Codex subscription bearer (ChatGPT OAuth JWT + ChatGPT-Account-ID) in
		// the inbound Authorization is honored even on router-keyed requests:
		// Codex CLI keeps its ChatGPT auth there while the router key rides in
		// X-Weave-Router-Key. OAuth subset only — a general API key is still not
		// forwarded on the router-key path (cross-provider-leak guard below).
		if inbound := ExtractClientCredentials(provider, headers); inbound != nil && inbound.OAuth {
			observability.FromContext(ctx).Debug("Resolved Codex subscription credential for OpenAI turn", "credential_source", inbound.Source)
			return context.WithValue(ctx, CredentialsContextKey{}, inbound)
		}
	}
	byok := BuildCredentialsMap(externalKeysFromContext(ctx))
	var creds *Credentials
	if byok != nil {
		creds = byok[provider]
	}
	if creds == nil && !routerKeyed {
		client := ExtractClientCredentials(provider, headers)
		// A suppressed Claude subscription must not slip back in here: off the
		// router-key path the spent sk-ant-oat bearer would otherwise re-resolve
		// as the subscription, undoing the skip above. Scoped to the Anthropic
		// OAuth bearer only.
		if suppressClaudeSub && provider == providers.ProviderAnthropic && client != nil && client.OAuth {
			client = nil
		}
		creds = client
	}
	if creds != nil {
		return context.WithValue(ctx, CredentialsContextKey{}, creds)
	}
	// Explicitly clear rather than leave as-is: on a router-keyed request with
	// no BYOK, none of the branches above resolve anything, so ctx would still
	// carry the primary attempt's subscription credential — re-sending the
	// spent sk-ant-oat the suppression meant to drop. The provider client only
	// falls back to its deployment key when ctx carries NO credential. Safe
	// because suppression is only ever set when a fallback key exists.
	if suppressClaudeSub && provider == providers.ProviderAnthropic {
		return clearCredentials(ctx)
	}
	return ctx
}

// addTimingAttrs appends derived latency attributes from the request Timing.
func addTimingAttrs(ctx context.Context, b *otel.AttrBuilder) {
	t := timing.TimingFrom(ctx)
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

// cacheTokenPtr returns nil for zero so the DB column stays NULL when the
// upstream didn't report cache usage (distinguishing "no cache" from "0 hits").
func cacheTokenPtr(n int) *int32 {
	if n <= 0 {
		return nil
	}
	v := int32(n)
	return &v
}

// int32PtrIfKnown returns a pointer to v when known is true, else nil.
// Used for nullable integer telemetry columns where 0 is a valid value
// (e.g. tool_use_blocks = 0 means zero tools) but the value may be absent
// when the translator did not run (Anthropic-native passthrough path).
func int32PtrIfKnown(v int32, known bool) *int32 {
	if !known {
		return nil
	}
	return &v
}

// boolPtrOrNil returns a pointer to v only when v is true. False is treated as
// "not set" so routine non-events don't fill nullable boolean columns.
func boolPtrOrNil(v bool) *bool {
	if !v {
		return nil
	}
	return &v
}

// boolPtrTrue always returns a non-nil pointer to v. Used for failover_used
// where both true and false are meaningful values to record.
func boolPtrTrue(v bool) *bool {
	return &v
}

// int64PtrIf returns a pointer to v when known is true, else nil. Used for
// pin_age_sec, gated on sticky_hit AND a positive age, so hard-pin/no-pin
// turns (sticky_hit true but age never computed) stay NULL instead of a
// spurious zero that would skew min-dwell analysis.
func int64PtrIf(known bool, v int64) *int64 {
	if !known {
		return nil
	}
	return &v
}

// toolResultBytesPtr returns the incoming tool-output size for telemetry on a
// tool_result turn, else nil. Takes an inbound LastUserMessage snapshot, not
// the live env: a handover may strip tool_result blocks from env before the
// telemetry write, which would otherwise read 0 on a genuine tool_result turn.
//
// Gated on the classified turn type, not just info.HasToolResult: the
// Anthropic/Gemini walkers report the last *user* message in the whole
// history, so a trailing assistant reply after a prior tool_result would
// otherwise write a stale non-NULL value.
func toolResultBytesPtr(inbound translate.LastUserMessageInfo, tt turntype.TurnType) *int32 {
	if tt != turntype.ToolResult || !inbound.HasToolResult {
		return nil
	}
	v := int32(inbound.ToolResultBytes)
	return &v
}

// stringPtrOrEmpty returns a pointer to s when it is non-empty, else nil.
func stringPtrOrEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// degenerateOutputThreshold is the output-token count below which a
// normal-completion response with no tool calls is flagged as degenerate.
const degenerateOutputThreshold = 10

// isDegenerateResponse returns true when the upstream produced a suspiciously
// short response: fewer than degenerateOutputThreshold output tokens, no tool
// calls emitted, and a normal end_turn stop reason. A valid tool-only turn or
// a brief legitimate end_turn must not trip this.
//
// stopReasonDemoted excludes cross-format demotions: a broken
// finish_reason="tool_calls" turn the translator demotes to end_turn is a
// handled translation failure, not a genuinely empty completion.
func isDegenerateResponse(outputTokens, toolUseBlocks int, stopReason string, stopReasonDemoted bool) bool {
	return outputTokens < degenerateOutputThreshold &&
		toolUseBlocks == 0 &&
		stopReason == "end_turn" &&
		!stopReasonDemoted
}

// fireTelemetry persists a telemetry row asynchronously. Telemetry loss is acceptable.
func (s *Service) fireTelemetry(p InsertTelemetryParams) {
	if s.telemetry == nil {
		return
	}
	log := observability.Get().With("request_id", p.RequestID)
	observability.SafeGo(log, 5*time.Second, "fireTelemetry", func(ctx context.Context) {
		if err := s.telemetry.InsertRequestTelemetry(ctx, p); err != nil {
			log.Debug("Telemetry insert failed", "err", err)
		}
	})
}

// emitBilling debits the customer for one upstream call and, on switch turns
// that invoked the handover summarizer, a second debit for the summary call
// (`_summary` request_id suffix). No-op when billing is unwired or
// externalID is empty. Unknown summarizer model prices as zero rather than
// skipping the ledger row, keeping the audit trail complete.
func (s *Service) emitBilling(ctx context.Context, requestID, externalID string, decision router.Decision, actPricing catalog.Pricing, routeRes turnLoopResult, in, out, cacheCreation, cacheRead int) {
	if s.billing == nil || externalID == "" {
		return
	}
	hasOverride := billing.HasOverrideFromContext(ctx)
	apiKeyID, _ := ctx.Value(APIKeyIDContextKey{}).(string)
	s.fireBilling(ctx, billing.DebitInferenceParams{
		OrganizationID:     externalID,
		RouterRequestID:    requestID,
		Model:              decision.Model,
		Provider:           decision.Provider,
		InputTokens:        in,
		OutputTokens:       out,
		CacheCreation:      cacheCreation,
		CacheRead:          cacheRead,
		Pricing:            actPricing,
		HasOverride:        hasOverride,
		SubscriptionServed: servedOnSubscription(ctx),
		APIKeyID:           apiKeyID,
	})

	// The handover summary always runs on the deployment/BYOK key (never the
	// subscription token), so it bills full cost regardless of the main turn.
	if routeRes.Handover.Invoked && !routeRes.Handover.FallbackToFullHistory {
		sumUsage := routeRes.Handover.SummaryUsage
		if sumUsage.Model != "" && (sumUsage.InputTokens > 0 || sumUsage.OutputTokens > 0) {
			sumPricing, _ := catalog.PrimaryPriceFor(sumUsage.Model)
			s.fireBilling(ctx, billing.DebitInferenceParams{
				OrganizationID:  externalID,
				RouterRequestID: requestID + "_summary",
				Model:           sumUsage.Model,
				Provider:        sumUsage.Provider,
				InputTokens:     sumUsage.InputTokens,
				OutputTokens:    sumUsage.OutputTokens,
				CacheCreation:   sumUsage.CacheCreation,
				CacheRead:       sumUsage.CacheRead,
				Pricing:         sumPricing,
				HasOverride:     hasOverride,
				APIKeyID:        apiKeyID,
			})
		}
	}
}

// fireBilling debits the org's prepaid credit balance for one upstream call.
// Synchronous so the ledger row is durable before handler return, but uses
// context.Background() so customer cancellation doesn't abort the write —
// the inference was already served, so the bookkeeping still owed. On
// failure, logs Error for manual reconciliation; the customer's response is
// unaffected since they already got it.
func (s *Service) fireBilling(ctx context.Context, p billing.DebitInferenceParams) {
	if s.billing == nil {
		return
	}
	if p.OrganizationID == "" {
		// Shouldn't happen on managed-mode authed requests. Debug level so a
		// synthetic test exercising the hook doesn't page on-call.
		observability.Get().Debug("Billing debit skipped: no organization_id on request")
		return
	}
	dbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	balance, err := s.billing.DebitForInference(dbCtx, p)
	if err == nil {
		observability.Get().Debug("Billing debit complete",
			"organization_id", p.OrganizationID,
			"router_request_id", p.RouterRequestID,
			"model", p.Model,
			"balance_usd_micros", balance,
			"override", p.HasOverride,
			"subscription_served", p.SubscriptionServed,
		)
		return
	}
	logBillingDebitFailure(ctx, p, err)
}

// logBillingDebitFailure emits a structured Error log so on-call alerting can
// fire on the resulting log rate without a new prometheus dependency.
func logBillingDebitFailure(ctx context.Context, p billing.DebitInferenceParams, err error) {
	observability.Get().Error("router_billing_debit_failed",
		"err", err,
		"organization_id", p.OrganizationID,
		"router_request_id", p.RouterRequestID,
		"model", p.Model,
		"provider", p.Provider,
		"input_tokens", p.InputTokens,
		"output_tokens", p.OutputTokens,
		"cache_creation_tokens", p.CacheCreation,
		"cache_read_tokens", p.CacheRead,
		"has_override", p.HasOverride,
		"subscription_served", p.SubscriptionServed,
	)
}

// upstreamStatus extracts the HTTP status from an upstream-typed error.
// Covers both UpstreamStatusError (bytes already flushed to client) and
// UpstreamErrorResponse (body buffered by the openaicompat adapter for
// the failover loop). Returns 0 for non-upstream errors.
func upstreamStatus(err error) int {
	var statusErr *providers.UpstreamStatusError
	if errors.As(err, &statusErr) {
		return statusErr.Status
	}
	var bufferedErr *providers.UpstreamErrorResponse
	if errors.As(err, &bufferedErr) {
		return bufferedErr.Status
	}
	return 0
}

// finalizeAfterProxy runs a translator's Finalize step. Cross-format
// translators buffer upstream body for non-streaming responses and flush only
// inside Finalize; skipping on 4xx/5xx drops the upstream error envelope before
// the client can see it. UpstreamStatusError takes precedence over Finalize
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
	ctx = s.withUsageObserver(ctx, r.Header)
	log := observability.FromContext(ctx)
	requestStart := time.Now()
	requestID := uuid.New().String()
	buf := s.newTelemetryBuffer()
	ctx = buf.WithContext(ctx)

	apiKeyID, _ := ctx.Value(APIKeyIDContextKey{}).(string)
	externalID, _ := ctx.Value(ExternalIDContextKey{}).(string)
	installationID := installationIDFromContext(ctx)
	clientID := ClientIdentityFrom(ctx)

	strippedBody, stripErr := stripRoutingMarkerFromMessages(body)
	if stripErr != nil {
		log.Error("Failed to strip routing marker from OpenAI messages", "err", stripErr)
		return fmt.Errorf("strip routing marker: %w", stripErr)
	}
	body = strippedBody

	// Same for the one-click thumbs footer (and its signed rate URLs), which
	// would otherwise shift assistant prefixes off the prompt cache.
	// Best-effort: log-and-continue on failure rather than abort over cosmetic
	// cleanup, matching the Anthropic Messages path.
	strippedBody, stripErr = translate.StripFeedbackFooterFromMessages(body)
	if stripErr != nil {
		log.Error("Failed to strip feedback footer from OpenAI messages", "err", stripErr)
	} else {
		body = strippedBody
	}
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

	// Bind session-scoped logger; see the matching block in ProxyMessages for
	// the rationale around deriving the key once and reusing it.
	var sessionKey [sessionpin.SessionKeyLen]byte
	ctx, log, sessionKey = bindRequestLogger(ctx, env, apiKeyID, requestID, "openai_chat_completions")
	log.Info("ProxyOpenAIChatCompletion start",
		"requested_model", feats.Model,
		"stream", env.Stream(),
		"message_count", feats.MessageCount,
		"has_tools", feats.HasTools,
		"total_input_tokens", feats.Tokens,
		"prompt_preview", observability.Preview(promptText, 200),
	)

	// Handle /force-model and /unforce-model before routing (stripped from
	// env.body so the upstream never sees it). Session key is derived before
	// extraction: DeriveSessionKey can fall back to prompt text, and deriving
	// after the strip would mismatch subsequent turns with the unstripped message.
	if s.pinStore != nil {
		if cmd, hasCmd := env.ExtractForceModelCommand(); hasCmd {
			log.Info("ProxyOpenAIChatCompletion force-model command", "force_model_cmd", cmd)
			return s.handleForceModelCommand(ctx, w, env, cmd, installationID, sessionKey, feats.Tokens)
		}
	}
	if cmd, hasCmd := env.ExtractRouterFeedbackCommand(); hasCmd {
		log.Info("ProxyOpenAIChatCompletion router-feedback command")
		return s.handleRouterFeedbackCommand(ctx, w, env, cmd, installationID, sessionKey, feats.Tokens)
	}

	// Honor the x-weave-force-model header (headless equivalent of /force-model).
	// Writes the user-forced pin and falls through to normal routing, which picks
	// the pin up and serves the requested model on this same turn.
	s.applyForceModelHeader(ctx, r, env, installationID, sessionKey)

	// Wide cyclic re-read loop → escalate to opus (same path as the Anthropic
	// ingress). See detectCyclicToolCallLoop / handleLoopEscalation.
	escalatedLoop := false
	if cyc, csig, ccount, cratio, cwin := detectCyclicToolCallLoop(env); cyc {
		loopRole := roleForTier(catalog.TierFor(feats.Model))
		s.handleLoopEscalation(ctx, csig, ccount, cratio, cwin, installationID, sessionKey, loopRole, feats.Model)
		escalatedLoop = true
	}
	// Tool-call loop break: same path as the Anthropic ingress. See the
	// detectToolCallLoop / handleToolCallLoopBreak doc comments for rationale.
	if !escalatedLoop {
		if loop, sig, count := detectToolCallLoop(env); loop {
			loopRole := roleForTier(catalog.TierFor(feats.Model))
			log.Info("ProxyOpenAIChatCompletion tool-call loop detected", "tool_sig", sig, "repeat_count", count, "role", loopRole)
			return s.handleToolCallLoopBreak(ctx, w, env, sig, count, installationID, sessionKey, loopRole, feats.Model, providers.ProviderOpenAI, feats.Tokens)
		}
	}

	logInboundRequestDiagnostics(log, env)

	// OpenAI signals sub-agent identity via x-weave-subagent-type (no metadata.user_id).
	subAgentHint := r.Header.Get("x-weave-subagent-type")

	enabledProviders := s.enabledProvidersForRequest(ctx, providers.ProviderOpenAI, r.Header)

	// Codex (ChatGPT) subscription passthrough: ProxyOpenAIResponses stashed the
	// caller's original Responses body. Such turns skip the routing marker +
	// semantic cache below, and dispatch the verbatim body to the Codex
	// backend when routed to an OpenAI model (see codexPassthrough branch).
	//
	// Deliberately not forcing OpenAI-only routing: enabledProviders already
	// scopes to providers the caller can pay for, so a dual Codex+Claude
	// subscription routes freely across both, each billing its own plan.
	// Subscriptions are credentials scoped to the routed model, not a pinned
	// provider.
	codexBody, _ := ctx.Value(codexResponsesBodyContextKey{}).([]byte)
	codexPassthrough := len(codexBody) > 0

	// Pre-filter models whose context window cannot fit this request.
	outputReserveOAI := contextWindowOutputReserve
	if feats.MaxTokens > outputReserveOAI {
		outputReserveOAI = feats.MaxTokens
	}
	baseExcludedOAI := s.excludedModelsForRequest(ctx)

	// Snapshot the inbound tool-output size before any env rewrite (proactive
	// compaction below, or runTurnLoop's switch handover); see toolResultBytesPtr.
	inboundLastUser := env.LastUserMessage()

	// Proactive context-window compaction, as in ProxyMessages. Skipped for
	// Codex passthrough bodies, which are forwarded verbatim.
	var compResOAI compactionResult
	if !codexPassthrough {
		maxEligibleWindowOAI := s.maxEligibleContextWindow(baseExcludedOAI, env.SignatureTokenSavings())
		var compErrOAI error
		compResOAI, compErrOAI = s.maybeCompact(ctx, env, turntype.DetectFromEnvelope(env, feats, subAgentHint), outputReserveOAI, maxEligibleWindowOAI, r.Header)
		if compErrOAI != nil {
			log.Warn("Compaction could not fit request to any eligible model",
				"err", compErrOAI, "final_estimate", compResOAI.FinalEstimate, "max_window", maxEligibleWindowOAI, "requested_model", feats.Model)
			return compErrOAI
		}
		if compResOAI.Applied {
			feats = env.RoutingFeatures(embedFlag)
			log.Info("Proactive compaction applied",
				"tool_results_cleared", compResOAI.ToolResultsCleared,
				"summarized", compResOAI.Summarized,
				"summary_model", compResOAI.SummaryModel,
				"trimmed_to_recent", compResOAI.TrimmedToRecent,
				"final_estimate", compResOAI.FinalEstimate,
			)
		}
	}

	overflowEstimateOAI := env.ContextOverflowTokenEstimate()
	excludedOAI, ctxOverflowedOAI := excludeContextOverflowModels(overflowEstimateOAI, env.SignatureTokenSavings(), outputReserveOAI, baseExcludedOAI, s.availableModels)
	if len(ctxOverflowedOAI) > 0 {
		log.Info("context window pre-filter: excluded over-capacity models",
			"overflow_token_estimate", overflowEstimateOAI,
			"output_reserve", outputReserveOAI,
			"excluded_count", len(ctxOverflowedOAI),
			"excluded_models", strings.Join(ctxOverflowedOAI, ","),
		)
	}
	excludedOAI, geminiUnsignedOAI := excludeGemini3xOnUnsignedHistory(env, excludedOAI, s.availableModels)
	if len(geminiUnsignedOAI) > 0 {
		log.Info("gemini pre-filter: excluded gemini-3.x for unsigned tool-call history",
			"excluded_models", strings.Join(geminiUnsignedOAI, ","),
		)
	}

	routeStart := time.Now()
	routeRes, err := s.runTurnLoop(ctx, env, feats, apiKeyID, installationID, subAgentHint, r.Header, router.Request{
		RequestedModel:       feats.Model,
		EstimatedInputTokens: feats.Tokens,
		HasTools:             feats.HasTools,
		HasImages:            feats.HasImages,
		PromptText:           promptText,
		EnabledProviders:     enabledProviders,
		ExcludedModels:       excludedOAI,
		PreferredModels:      s.preferredModelsForRequest(ctx),
		RoutingKnobs:         routingKnobsForRequest(ctx),
	})
	routeMs := time.Since(routeStart).Milliseconds()
	if err != nil {
		log.Error("Routing failed for OpenAI request", "err", err, "route_ms", routeMs, "requested_model", feats.Model, "total_input_tokens", feats.Tokens)
		return err
	}
	routeRes.SuggestionMode = r.Header.Get("x-weave-suggestion-mode") == "true"
	decision := routeRes.Decision
	tt := routeRes.TurnType
	stickyHit := routeRes.StickyHit
	pinTier := routeRes.PinTier
	pinAgeSec := routeRes.PinAgeSec
	s.logPlannerOutcome(ctx, routeRes)

	// See the ProxyMessages cache-eligibility note: subsidized requests bypass the
	// semantic cache (the key doesn't capture headroom-dependent model choice).
	cacheEligible := s.semanticCache != nil && !env.Stream() && decision.Metadata != nil && externalID != "" && !bypassEval && !codexPassthrough && len(s.subsidyFactors(ctx, r.Header)) == 0
	if cacheEligible {
		if resp, hit := s.semanticCache.Lookup(externalID, cache.FormatOpenAI, decision.Metadata.Embedding, decision.Metadata.ClusterIDs, decision.Metadata.ClusterRouterVersion, decision.Metadata.EffectiveKnobsHash); hit {
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

	if _, err := s.provider(decision.Provider); err != nil {
		return err
	}

	w.Header().Set(HeaderRouterDecision, decision.Reason)
	w.Header().Set(HeaderRouterProvider, decision.Provider)
	w.Header().Set(HeaderRouterModel, decision.Model)
	s.setFeedbackLinkHeader(w, installationID, externalID, requestID, auth.UserIDFrom(ctx))

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
		Float64("catalog.requested_input_per_1m", reqPricing.InputUSDPer1M).
		Float64("catalog.requested_output_per_1m", reqPricing.OutputUSDPer1M).
		Float64("catalog.actual_input_per_1m", actPricing.InputUSDPer1M).
		Float64("catalog.actual_output_per_1m", actPricing.OutputUSDPer1M).
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
		TargetModel:           decision.Model,
		TargetProvider:        decision.Provider,
		Capabilities:          router.Lookup(decision.Model),
		IncludeStreamUsage:    s.usageRequired(),
		SessionAffinity:       sessionAffinityHint(routeRes.SessionKey),
		ModelSwitched:         routeRes.modelSwitched(),
		EnableExtendedContext: shouldEnableExtendedContext(env.FullTokenEstimate(), outputReserveOAI),
	}
	if s.effortEscalation {
		opts.ForceReasoningEffort = forcedReasoningEffort(decision.Model, routeRes.EscalateEffort)
	}

	ctx = resolveAndInjectCredentials(ctx, decision.Provider, r.Header)

	// See ProxyMessages for the preludeBuffer rationale — wrap unconditionally
	// so single-binding upstream errors don't strand the routing-marker chunk
	// on the wire when the upstream never produces a first byte.
	bindings := s.resolveBindingsForDispatch(ctx, decision)
	// Append the one-click feedback thumbs as a trailing chunk (see
	// ProxyMessages). Skipped on the Responses-API path (w is a
	// *ResponsesWriter): wrapping it would defeat maybeCaptureResponse's
	// special-casing — /v1/responses footers are a follow-up.
	clientSink := w
	if _, isResponses := w.(*translate.ResponsesWriter); env.Stream() && !isResponses {
		if footer := s.feedbackFooter(ClientIdentityFrom(ctx).ClientApp, routeRes.TurnType); footer != "" {
			clientSink = translate.NewOpenAIRoutingFooterWriter(w, footer)
		}
	}
	contentSink, contentCap := s.maybeCaptureResponse(clientSink)
	preludeBuf := newPreludeBuffer(contentSink)
	var rootSink http.ResponseWriter = preludeBuf

	// Responses entry point delegates the eager response.created emit to
	// this layer because it has the post-routing binding count. Fire only
	// when single-binding so multi-binding requests stay failover-safe
	// (Codex client sees response.created via ResponsesWriter's lazy
	// emitCreated on the first upstream byte instead).
	if rw, ok := w.(*translate.ResponsesWriter); ok {
		// A Codex-sub turn routed to OpenAI streams Responses SSE natively
		// (verbatim); routed elsewhere it stays in translate mode.
		//
		// Set once here (before Prelude), not per-attempt: response.created
		// suppression depends on passthrough being engaged before the first
		// write. Safe because decision.Provider == OpenAI is always a
		// single-binding GPT model with no cross-format fallback to retry
		// into. If a GPT model ever gains a fallback, gate this per-attempt.
		if codexPassthrough && decision.Provider == providers.ProviderOpenAI {
			rw.SetPassthrough()
		}
		if len(bindings) <= 1 {
			if err := rw.Prelude(env.Stream()); err != nil {
				log.Error("Responses prelude failed", "err", err)
			}
		}
	}

	var captureW *captureWriter
	var sink http.ResponseWriter = rootSink
	if cacheEligible {
		captureW = newCaptureWriter(rootSink, semanticCacheMaxBodyBytes)
		sink = captureW
	}

	marker := suppressMarkerIfRequested(r.Header, routingMarkerFor(routeRes))
	if codexPassthrough {
		// The client receives raw Responses SSE from the Codex backend; a
		// chat-completions routing-marker chunk would corrupt that stream.
		marker = ""
	}
	_, isResponses := w.(*translate.ResponsesWriter)
	// makeMarkerSink wraps sink with an OpenAIRoutingMarkerWriter emitting the
	// marker chunk + HTTP 200 eagerly (skipped for /v1/responses). Called per
	// attempt so retries re-emit into a fresh preludeBuffer state.
	//
	// Wrapped even when marker == "": it's the only ArmOutputProgress provider
	// for the OpenAI→openaicompat passthrough, and the empty-marker Prelude
	// still flips the streaming flag so the watchdog can arm (emits a harmless
	// ": routing complete" comment, not a content chunk).
	makeMarkerSink := func() http.ResponseWriter {
		// Codex passthrough streams raw Responses SSE; wrapping it in a
		// chat-completions marker writer would inject a foreign frame (and the
		// output-progress scan reads choices[].delta, which Responses lacks).
		if isResponses || codexPassthrough {
			return sink
		}
		mw := translate.NewOpenAIRoutingMarkerWriter(sink, decision.Model, marker)
		if err := mw.Prelude(env.Stream()); err != nil {
			log.Error("OpenAI routing-marker prelude failed", "err", err)
		}
		return mw
	}

	proxyStart := time.Now()
	var proxyErr error
	crossFormat := false
	var extractor *otel.UsageExtractor

	var attempt dispatchAttempt
	// Dispatch keys off the provider's translation family, not a hardcoded name
	// list, so a new OpenAI-compat provider routes here as soon as it has a
	// ProviderFamilies entry (see internal/providers/provider.go).
	switch providers.FamilyFor(decision.Provider) {
	case providers.FamilyOpenAICompat:
		// Prep rebuilt per attempt: targetIsOpenRouter(opts) gates four
		// OpenRouter-only body fields that Fireworks/DeepInfra/Bedrock should
		// not see. On failover to OpenRouter the body must be re-emitted.
		attempt = func(actx context.Context, d router.Decision, p providers.Client) error {
			var prep providers.PreparedRequest
			if codexPassthrough && d.Provider == providers.ProviderOpenAI {
				// Dispatch the caller's ORIGINAL Responses body (untranslated) to
				// the Codex backend, rewriting only the model. Gated on
				// d.Provider == OpenAI: a Codex-sub turn routed to an OSS
				// provider gets the translated chat body below instead.
				outBody, setErr := sjson.SetBytes(codexBody, "model", d.Model)
				if setErr != nil {
					log.Error("Failed to set routed model on Codex Responses body", "err", setErr, "decision_model", d.Model)
					return fmt.Errorf("set codex model: %w", setErr)
				}
				prep = providers.PreparedRequest{Body: outBody, Endpoint: providers.EndpointResponses, Headers: make(http.Header)}
			} else {
				attemptOpts := opts
				attemptOpts.TargetProvider = d.Provider
				var emitErr error
				prep, emitErr = env.PrepareOpenAI(r.Header, attemptOpts)
				if emitErr != nil {
					log.Error("Failed to emit OpenAI body", "err", emitErr, "decision_provider", d.Provider)
					return fmt.Errorf("emit body: %w", emitErr)
				}
			}
			attemptSink := makeMarkerSink()
			proxyWriter := attemptSink
			if s.usageRequired() {
				extractor = otel.NewUsageExtractor(attemptSink, d.Provider)
				proxyWriter = extractor
			}
			if preludeBuf != nil {
				preludeBuf.Seal()
			}
			err := p.Proxy(actx, d, prep, proxyWriter, r)
			// Post-commit: bytes already on the wire, render as an in-stream
			// frame instead of a corrupting envelope (pre-commit goes through
			// dispatchWithFallback). Gate on THIS attempt being the verbatim
			// Codex backend, not codexPassthrough alone: a Codex-sub turn can
			// still route to Claude/OSS through the translating ResponsesWriter,
			// which needs its own error frame — only the verbatim Codex attempt
			// already delivered the upstream's own Responses error event.
			verbatimCodex := codexPassthrough && d.Provider == providers.ProviderOpenAI
			if err != nil && !verbatimCodex && env.Stream() && preludeBuf.Committed() {
				err = emitOpenAISSEErrorEvent(sink, err)
			}
			return err
		}
	case providers.FamilyGemini:
		crossFormat = true
		prep, emitErr := env.PrepareGemini(r.Header, opts)
		if emitErr != nil {
			log.Error("Failed to translate OpenAI request to Gemini format", "err", emitErr)
			return fmt.Errorf("translate openai request to gemini: %w", emitErr)
		}
		// See ProxyMessages' Gemini case: a VALIDATED-mode request can 400 with a
		// generic INVALID_ARGUMENT when Gemini can't compile a tool schema into
		// its decode-time grammar. Retry once with mode=AUTO when pre-commit.
		geminiUsedValidated := prep.Stats.GeminiValidatedToolMode
		dispatchGemini := func(actx context.Context, d router.Decision, p providers.Client, pr providers.PreparedRequest) (error, func(error) error) {
			var usage otel.UsageSink
			if s.usageRequired() {
				extractor = otel.NewUsageExtractor(nil, d.Provider)
				usage = extractor
			}
			attemptSink := makeMarkerSink()
			translator := translate.NewGeminiToOpenAISSETranslator(attemptSink, d.Model, usage)
			if preludeBuf != nil {
				preludeBuf.Seal()
			}
			rawErr := p.Proxy(actx, d, pr, translator, r)
			finalize := func(err error) error {
				// Post-commit streaming error: see same-format OpenAI case above.
				if err != nil && env.Stream() && preludeBuf.Committed() {
					err = emitOpenAISSEErrorEvent(sink, err)
				}
				return finalizeAfterProxy(err, translator.Finalize)
			}
			return rawErr, finalize
		}
		attempt = func(actx context.Context, d router.Decision, p providers.Client) error {
			rawErr, finalize := dispatchGemini(actx, d, p, prep)
			if rawErr != nil && geminiUsedValidated && !committed(preludeBuf) && upstreamStatus(rawErr) == http.StatusBadRequest {
				autoOpts := opts
				autoOpts.DowngradeGeminiValidatedToAuto = true
				autoPrep, autoErr := env.PrepareGemini(r.Header, autoOpts)
				if autoErr != nil {
					log.Error("Failed to re-translate Gemini request with tool mode AUTO", "err", autoErr)
					return finalize(rawErr)
				}
				log.Warn("Retrying Gemini request with functionCallingConfig.mode=AUTO after VALIDATED-mode 400",
					"model", d.Model,
					"request_id", requestID)
				if preludeBuf != nil {
					preludeBuf.Discard()
				}
				rawErr, finalize = dispatchGemini(actx, d, p, autoPrep)
			}
			return finalize(rawErr)
		}
	case providers.FamilyAnthropic:
		crossFormat = true
		prep, emitErr := env.PrepareAnthropic(r.Header, opts)
		if emitErr != nil {
			log.Error("Failed to translate OpenAI request to Anthropic format", "err", emitErr)
			return fmt.Errorf("translate openai request: %w", emitErr)
		}
		attempt = func(actx context.Context, d router.Decision, p providers.Client) error {
			var usage otel.UsageSink
			if s.usageRequired() {
				extractor = otel.NewUsageExtractor(nil, providers.ProviderAnthropic)
				usage = extractor
			}
			attemptSink := makeMarkerSink()
			translator := translate.NewSSETranslator(attemptSink, d.Model, usage)
			if preludeBuf != nil {
				preludeBuf.Seal()
			}
			err := p.Proxy(actx, d, prep, translator, r)
			// Post-commit streaming error: see same-format OpenAI case above.
			if err != nil && env.Stream() && preludeBuf.Committed() {
				err = emitOpenAISSEErrorEvent(sink, err)
			}
			return finalizeAfterProxy(err, translator.Finalize)
		}
	default:
		return fmt.Errorf("%w: %s (no translation path defined)", ErrProviderNotConfigured, decision.Provider)
	}

	primaryProvider := decision.Provider
	var winnerIdx int
	winnerIdx, proxyErr = s.dispatchWithFallback(ctx, failoverInputs{
		// contentSink is the raw w when capture is off.
		w:               contentSink,
		buf:             preludeBuf,
		initialDecision: decision,
		bindings:        bindings,
		attempt:         attempt,
		flushErr:        flushBufferedIfPresent,
	})
	finalProvider := primaryProvider
	if winnerIdx >= 0 && winnerIdx < len(bindings) {
		finalProvider = bindings[winnerIdx].Provider
	}
	decision.Provider = finalProvider

	// Re-resolve credentials for the binding that actually served — each
	// failover attempt gets its own context with potentially different creds.
	ctx = resolveAndInjectCredentials(ctx, finalProvider, r.Header)

	// Re-resolve pricing for the binding that actually served (see ProxyMessages).
	if actBindingPricing, ok := catalog.PriceFor(finalProvider, decision.Model); ok {
		actPricing = actBindingPricing
	}

	if cacheEligible && proxyErr == nil && captureW != nil {
		if body, status, ok := captureW.captured(); ok && status == http.StatusOK {
			storeResp := cache.CachedResponse{
				StatusCode: status,
				Headers:    cloneCacheHeaders(w.Header()),
				Body:       body,
			}
			s.semanticCache.Store(externalID, cache.FormatOpenAI, decision.Metadata.Embedding, decision.Metadata.ClusterIDs[0], storeResp, decision.Metadata.ClusterRouterVersion, decision.Metadata.EffectiveKnobsHash)
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
		String("requested.model", feats.Model).
		String("decision.model", decision.Model).
		String("decision.provider", finalProvider).
		String("decision.reason", decision.Reason).
		String("routing.turn_type", string(routeRes.TurnType)).
		Int64("usage.input_tokens", int64(in)).
		Int64("usage.output_tokens", int64(out)).
		Int64("usage.cache_creation_input_tokens", int64(cacheCreation)).
		Int64("usage.cache_read_input_tokens", int64(cacheRead)).
		Float64("cost.requested_input_usd", catalog.EffectiveInputCost(in, cacheCreation, cacheRead, reqPricing.InputUSDPer1M, reqPricing, decision.Provider)).
		Float64("cost.requested_output_usd", catalog.EffectiveOutputCost(out, reqPricing.OutputUSDPer1M)).
		Float64("cost.actual_input_usd", catalog.EffectiveInputCost(in, cacheCreation, cacheRead, actPricing.InputUSDPer1M, actPricing, decision.Provider)).
		Float64("cost.actual_output_usd", catalog.EffectiveOutputCost(out, actPricing.OutputUSDPer1M)).
		Bool("cost.subscription_served", servedOnSubscription(ctx)).
		Int64("latency.upstream_ms", proxyMs).
		Int64("latency.total_ms", time.Since(requestStart).Milliseconds()).
		Int64("upstream.status_code", int64(upstreamStatus(proxyErr))).
		Bool("routing.cross_format", crossFormat).
		String("dispatch.primary_provider", primaryProvider).
		String("dispatch.final_provider", finalProvider).
		Int64("dispatch.fallback_attempts", int64(winnerIdx)).
		Bool("dispatch.failover_used", finalProvider != primaryProvider)
	applyPlannerAttrs(openaiUpstreamBuilder, routeRes)
	addTimingAttrs(ctx, openaiUpstreamBuilder)

	openaiObs := buildObservationContext(ctx, decision, routeRes.Fresh)
	openaiObs.applySpanAttrs(openaiUpstreamBuilder)

	otel.Record(ctx, otel.Span{
		Name:  "router.upstream",
		Start: proxyStart,
		End:   time.Now(),
		Attrs: openaiUpstreamBuilder.Build(),
	})
	callLogBase := openaiUpstreamBuilder.Build()
	emitCallLog := func() {
		reqBody := body
		if h := deferredCallLogFrom(ctx); h != nil && h.requestBody != nil {
			reqBody = h.requestBody
		}
		respBody, respTrunc := capturedResponse(contentCap)
		s.recordCallLog(ctx, callLogBase, proxyErr != nil, reqBody, respBody, respTrunc)
		otel.Flush(ctx)
	}
	// The /v1/responses surface (ProxyOpenAIResponses) finalizes its
	// ResponsesWriter only after this function returns, so the captured body
	// isn't complete yet — defer the read+emit to run post-Finalize. All other
	// callers emit inline.
	if h := deferredCallLogFrom(ctx); h != nil {
		h.fn = emitCallLog
	} else {
		emitCallLog()
	}

	s.recordTurnUsage(routeRes, decision.Model, in, out, cacheCreation, cacheRead)

	if proxyErr == nil {
		s.emitBilling(ctx, requestID, externalID, decision, actPricing, routeRes, in, out, cacheCreation, cacheRead)
		if compResOAI.Summarized {
			s.billCompactionSummary(ctx, requestID, externalID, compResOAI.SummaryUsage)
		}
	}

	// See ProxyMessages for the two-strike eviction rationale.
	s.maybeEvictPinAfterUpstreamErr(ctx, stickyHit, proxyErr, decision.Reason, installationIDFromContext(ctx), routeRes.SessionKey, routeRes.PinRole)

	installationIDOAI, _ := ctx.Value(InstallationIDContextKey{}).(string)
	if installationIDOAI != "" {
		credentialKeyPrefix, credentialKeySuffix, credSource := s.credentialKeyParts(ctx)
		s.fireTelemetry(InsertTelemetryParams{
			InstallationID:         installationIDOAI,
			APIKeyID:               apiKeyIDFromContext(ctx),
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
			RequestedInputCostUSD:  catalog.EffectiveInputCost(in, cacheCreation, cacheRead, reqPricing.InputUSDPer1M, reqPricing, decision.Provider),
			RequestedOutputCostUSD: catalog.EffectiveOutputCost(out, reqPricing.OutputUSDPer1M),
			ActualInputCostUSD:     catalog.EffectiveInputCost(in, cacheCreation, cacheRead, actPricing.InputUSDPer1M, actPricing, decision.Provider),
			ActualOutputCostUSD:    catalog.EffectiveOutputCost(out, actPricing.OutputUSDPer1M),
			RouteLatencyMs:         routeMs,
			UpstreamLatencyMs:      proxyMs,
			TotalLatencyMs:         time.Since(requestStart).Milliseconds(),
			CrossFormat:            crossFormat,
			UpstreamStatusCode:     int32(upstreamStatus(proxyErr)),
			ClusterIDs:             openaiObs.ClusterIDs,
			CandidateModels:        openaiObs.CandidateModels,
			ChosenScore:            openaiObs.ChosenScore,
			CandidateScores:        openaiObs.CandidateScores,
			Propensity:             openaiObs.Propensity,
			ClusterRouterVersion:   openaiObs.ClusterRouterVersion,
			TTFTMs:                 openaiObs.TTFTMs,
			CacheCreationTokens:    cacheTokenPtr(cacheCreation),
			CacheReadTokens:        cacheTokenPtr(cacheRead),
			DeviceID:               clientID.DeviceID,
			SessionID:              clientID.SessionID,
			RouterUserID:           auth.UserIDFrom(ctx),
			ClientApp:              clientID.ClientApp,
			TurnType:               string(routeRes.TurnType),
			RolloutID:              clientID.RolloutID,
			FailoverUsed:           boolPtrTrue(finalProvider != primaryProvider),
			// (session_key, role) join key — see the Anthropic-path write site.
			SessionKey: sessionKey[:],
			Role:       routeRes.PinRole,
			// Shadow-mode hysteresis instrumentation — see the Anthropic-path site.
			FreshDecisionModel:   openaiObs.FreshDecisionModel,
			FreshCandidateScores: openaiObs.FreshCandidateScores,
			PinAgeSec:            int64PtrIf(stickyHit && pinAgeSec > 0, pinAgeSec),
			// Shadow-mode tier-cap instrumentation: tool-output size on
			// tool_result turns. NULL elsewhere. No routing action taken.
			ToolResultBytes: toolResultBytesPtr(inboundLastUser, tt),
			// Credential attribution — see the Anthropic-path write site.
			CredentialKeyPrefix: credentialKeyPrefix,
			CredentialKeySuffix: credentialKeySuffix,
			CredentialSource:    credSource,
		})
	}

	log.Info("ProxyOpenAIChatCompletion complete", "requested_model", feats.Model, "baseline_model", s.baselineFor(feats.Model), "decision_model", decision.Model, "decision_provider", decision.Provider, "primary_provider", primaryProvider, "fallback_attempts", winnerIdx, "failover_used", finalProvider != primaryProvider, "decision_reason", decision.Reason, "requested_tier", routeRes.RequestedTier.String(), "decision_tier", catalog.TierFor(decision.Model).String(), "embedded_tokens", len(promptText)/4, "total_input_tokens", feats.Tokens, "has_tools", feats.HasTools, "embed_input", embedInput, "cross_format", crossFormat, "sticky_hit", stickyHit, "pin_tier", pinTier, "turn_type", string(tt), "route_ms", routeMs, "proxy_ms", proxyMs, "proxy_err", proxyErr, "upstream_status", upstreamStatus(proxyErr))
	s.reportHMMOutcome(ctx, routeRes, decision, finalProvider, feats.Tokens, in, out, cacheCreation, cacheRead, routeMs, proxyMs, proxyErr)
	return proxyErr
}

// ProxyOpenAIResponses routes an OpenAI Responses API request. The Responses
// wire format is translated to Chat Completions on entry, dispatched through
// the existing chat-completions path, then the chat-completions response is
// re-emitted as Responses-shaped SSE / JSON. This keeps the turn loop, cache,
// pricing, and translation matrix unchanged.
func (s *Service) ProxyOpenAIResponses(ctx context.Context, body []byte, w http.ResponseWriter, r *http.Request) error {
	ctx = s.withUsageObserver(ctx, r.Header)
	chatBody, _, model, err := translate.ResponsesToChatCompletions(body)
	if err != nil {
		return fmt.Errorf("translate responses request: %w", err)
	}
	// Codex (ChatGPT) subscription: stash the caller's ORIGINAL Responses body.
	// A turn that resolves a Codex sub and routes to OpenAI is dispatched
	// verbatim (passthrough mode, set post-routing) so reasoning/tool/encrypted
	// content is never lossily re-translated. Routed elsewhere, it falls
	// through to normal chat->Responses translation like any Responses turn.
	// Routing, billing, and telemetry are reused via ProxyOpenAIChatCompletion
	// (chatBody feeds routing only).
	if codexResponsesRequest(ctx, r.Header) {
		ctx = context.WithValue(ctx, codexResponsesBodyContextKey{}, body)
	}
	wrapper := translate.NewResponsesWriter(w, model)
	// Defer the high-fidelity call-log emission until after Finalize: the
	// ResponsesWriter buffers (non-streaming) and emits tail events only in
	// Finalize, so the captured io.response_body is incomplete until then.
	ctx, deferredLog := withDeferredCallLog(ctx)
	// Capture the client's original Responses JSON as the request body so the
	// call log's io.request_body matches the Responses-format response body
	// (ProxyOpenAIChatCompletion otherwise sees the translated chatBody).
	deferredLog.requestBody = body
	// Prelude (response.created emit) deferred to ProxyOpenAIChatCompletion,
	// which knows the post-routing decision and binding count: fires eagerly
	// only when single-binding, else relies on ResponsesWriter's lazy
	// emitCreated on first byte — preserving the failover invariant that
	// nothing reaches the client before the upstream commits.
	proxyErr := s.ProxyOpenAIChatCompletion(ctx, chatBody, wrapper, r)
	if proxyErr != nil {
		// If the Responses stream already committed (response.created is on the
		// wire), the upstream error can no longer be rendered as a JSON error
		// envelope — terminate the SSE stream with response.failed so the client
		// (Codex) sees a clean failure instead of "stream closed before
		// response.completed". A no-op before anything is streamed, so the
		// handler still writes the JSON error envelope in that case.
		if finErr := wrapper.FinalizeError(proxyErr); finErr != nil {
			observability.FromContext(ctx).Error("Failed to finalize Responses error stream", "err", finErr)
		}
		deferredLog.run()
		return proxyErr
	}
	finErr := wrapper.Finalize()
	deferredLog.run()
	return finErr
}
