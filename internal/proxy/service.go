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
	"workweave/router/internal/router"
	"workweave/router/internal/router/cache"
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/router/handover"
	"workweave/router/internal/router/planner"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/router/turntype"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
)

// Service orchestrates routing decisions and provider dispatch.
type Service struct {
	router               router.Router
	providers            map[string]providers.Client
	emitter              *otel.Emitter
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
	// hardPinExplore gates the Explore sub-agent hard-pin.
	hardPinExplore bool
	// hardPinProvider/hardPinModel route compaction (and, when hardPinExplore is
	// on, Explore sub-agent turns). Derived at boot from the cheapest registered
	// model; overridable via ROUTER_HARD_PIN_PROVIDER / ROUTER_HARD_PIN_MODEL.
	hardPinProvider string
	hardPinModel    string
	// hardPinResolver, when set, overrides boot-time hardPin{Provider,Model}
	// per-request. Used in byokOnly deployments where the registered cheapest
	// model is unsafe — the installation may only BYOK a subset of providers.
	// Returns (provider, model, ok); ok=false signals no eligible provider.
	hardPinResolver func(enabled map[string]struct{}) (provider, model string, ok bool)
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
	// BYOK). Entries here are added to the eligible set only when the
	// inbound request came in via the matching surface — otherwise the
	// OpenAI client would forward an Anthropic-surface request's `x-api-key`
	// to api.openai.com (and vice versa), which is a cross-provider
	// credential leak even when upstream 401s. Surface-scoping ensures
	// passthrough creds only enable the upstream they were issued for.
	passthroughEligibleProviders map[string]struct{}
	// planner parameterizes the Prism-style EV policy for stay-vs-switch.
	planner planner.EVConfig
	// plannerEnabled is the kill switch. When false, the orchestrator falls
	// back to first-decision-wins behavior.
	plannerEnabled bool
	// effortEscalation enables the escalate-on-failure reasoning-effort policy:
	// gpt-5.x serves low effort by default and high after an observed
	// failed/no-progress turn; gemini is pinned low. Off by default (set from
	// ROUTER_EFFORT_ESCALATION) so it can be baked off before enabling.
	effortEscalation bool
	// loopEscalationEnabled is the kill switch for the cyclic-loop
	// escalate-to-opus ACTION. When false, detection and telemetry keep
	// running (events recorded with action=disabled) but no escalation pin is
	// written. Defaults to true (the lever shipped enabled); set from
	// ROUTER_LOOP_ESCALATION_ENABLED.
	loopEscalationEnabled bool
	// loopEscalationHoldoutPct is the percentage of loop-detected sessions
	// deterministically assigned to the log-not-act holdout: the event is
	// recorded but the rescue withheld, so the self-recovery baseline can be
	// subtracted from rescue-rate claims. 0 disables the holdout. Set from
	// ROUTER_LOOP_ESCALATION_HOLDOUT_PCT.
	loopEscalationHoldoutPct int
	// loopEscalationStore persists loop detections durably
	// (router.loop_escalation_events) and enforces the once-per-session
	// budget. Nil disables persistence — and with it the holdout, which is
	// only meaningful when the withheld rescue leaves a row behind.
	loopEscalationStore LoopEscalationStore
	// spiralShadowEnabled gates the shadow-mode spiral detector (log-only
	// death-march signals; see spiral_detection.go). Defaults to true — shadow
	// mode changes no routing behavior — with ROUTER_SPIRAL_SHADOW_ENABLED as
	// the kill switch.
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
}

// pinSessionTTL mirrors Anthropic's prompt-cache TTL on Sonnet/Haiku/Opus 4.5+
// so the pin lifecycle tracks the cache it's keeping warm.
const pinSessionTTL = time.Hour

// pinNeverExpires is the sentinel PinnedUntil for user-forced pins. A
// /force-model is an explicit, durable user directive — it must persist across
// arbitrarily long idle gaps and only clear on /unforce-model, never lapse on
// the cache-driven session TTL. Far enough out to never be reached, well within
// Postgres's timestamp range; loadPin's PinnedUntil.After(now) check and the
// pinned_until-based sweep both read it as live indefinitely. /unforce-model
// rewrites the row with a past PinnedUntil, so the escape hatch still works.
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

// prevTurnMaxedOutThreshold is the LastOutputTokens count above which we treat
// the previous turn as having saturated the output cap. Set just under the
// 8192 defaultMaxOutputTokenCap; legitimate end_turn completions almost never
// approach this on tool-calling turns, while OSS-model parse-failure runaways
// land exactly at the cap. Used by runTurnLoop to break the auto-continue
// loop by excluding the pinned model for the next turn.
const prevTurnMaxedOutThreshold = 8000

// APIKeyIDContextKey is the request-context key for the authenticated api_key_id.
type APIKeyIDContextKey struct{}

// ExternalIDContextKey is the request-context key for the installation's external_id.
type ExternalIDContextKey struct{}

// CredentialsContextKey is the request-context key for resolved per-request credentials.
type CredentialsContextKey struct{}

// AnthropicSubscriptionContextKey is the request-context key for a caller's raw
// Claude subscription OAuth token, stashed by the auth middleware from the
// X-Weave-Anthropic-Subscription header on router-keyed requests.
type AnthropicSubscriptionContextKey struct{}

// InstallationExcludedModelsContextKey is the context key for the authed
// installation's model exclusion list. Carried as []string.
type InstallationExcludedModelsContextKey struct{}

// InstallationExcludedProvidersContextKey is the context key for the authed
// installation's provider exclusion list. Carried as []string.
type InstallationExcludedProvidersContextKey struct{}

// InstallationRoutingKnobsContextKey is the context key for the authed
// installation's persisted routing preference (the "quality vs price" dial).
// Carried as *router.Overrides with only Alpha (quality weight) set; the
// per-request x-weave-routing-* header override takes precedence over it. See
// routingKnobsForRequest.
type InstallationRoutingKnobsContextKey struct{}

// installationExcludedModelsFromContext returns the per-installation exclusion
// list stashed on ctx by the auth middleware, or nil when none is present.

// routingMarkerHeader lets a client suppress the in-band "✦ **Weave Router** → …"
// routing badge. Programmatic clients that surface the routed model out-of-band
// (e.g. pi reads the x-router-model response header into its status bar) and
// render the assistant message into their own UI can't show a standalone marker
// text block without it hiding the actual answer. Any of off/false/0/none
// disables it; absent or anything else keeps the default.
const routingMarkerHeader = "X-Weave-Routing-Marker"

// suppressMarkerIfRequested returns "" when the request opted out of the routing
// marker via routingMarkerHeader, otherwise the marker unchanged. Scoped to the
// per-turn routing badge; the no-progress / loop / force-model markers are
// standalone single-block messages and are intentionally always emitted.
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
	// Suppress the marker on tool-result follow-ups: every post-tool turn would
	// otherwise re-emit a duplicate mid-stream. But always show the marker if the
	// model changed, even if the reason code is unknown (recovery codes return
	// empty from humanReasonFromPlanner).
	modelChanged := res.PriorServedModel != "" && res.PriorServedModel != res.Decision.Model
	if res.PlannerDecision.Reason == "" && !res.HardPinned && res.StickyHit && !modelChanged {
		return ""
	}
	parts := []string{"✦ **Weave Router** → " + decision.Model}
	if reason := routingReasonShort(res); reason != "" {
		parts = append(parts, reason)
	}
	return strings.Join(parts, " · ") + "\n\n"
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

func installationExcludedModelsFromContext(ctx context.Context) []string {
	v := ctx.Value(InstallationExcludedModelsContextKey{})
	if v == nil {
		return nil
	}
	out, _ := v.([]string)
	return out
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

// contextWindowOverheadFactor scales the raw body-bytes token estimate to
// account for JSON structure overhead (field names, brackets, quotes) inflating
// byte count relative to actual tokens. The inverse of 5 bytes/token
// comes from empirical Anthropic request bodies; tool-heavy sessions run higher.
// This constant is intentionally baked into FullTokenEstimate (body/5).

// contextWindowOutputReserve is the minimum tokens reserved for the model's
// response when comparing the request estimate against the context window.
const contextWindowOutputReserve = 8_000

// extendedContextTriggerTokens is the estimated request size (input estimate +
// output reserve) at which the proxy turns on a CapExtendedContext model's 1M
// window by injecting the context-1m-2025-08-07 beta. It sits well below the
// 200K standard window on purpose: FullTokenEstimate (body bytes ÷5)
// undercounts real tokens by ~20-30% on dense Claude Code bodies, so 140K
// estimated is roughly 175-200K real tokens — the beta is in place before a
// request that's truly near 200K reaches the upstream and 400s on the default
// window. Below this the beta is omitted so ordinary sub-200K turns stay on the
// standard window (no needless opt-in to long-context behavior/pricing).
const extendedContextTriggerTokens = 140_000

// shouldEnableExtendedContext reports whether a request is large enough to
// warrant turning on a CapExtendedContext model's 1M window. Gating on the
// estimate (rather than always-on) keeps ordinary turns on the standard
// window; the trigger sits low enough that the ÷5 estimate's undercount can't
// let a genuinely-near-200K request slip onto the 200K default.
func shouldEnableExtendedContext(est, outputReserve int) bool {
	return est+outputReserve > extendedContextTriggerTokens
}

// contextWindowForRequest returns the effective context window for a model.
// CapExtendedContext models (Opus 4.6+, Sonnet 4.6) always report 1M: the proxy
// unconditionally injects the context-1m-2025-08-07 beta when it dispatches to
// them (EmitOptions.EnableExtendedContext), so the filter must not exclude them
// for requests that fit 1M. Gating this on the client's beta header — or on the
// body-byte token estimate — would let a large request slip onto a 200K window
// it overflows on the first turn. All other models report their catalog window.
func contextWindowForRequest(modelID string) int {
	if router.Lookup(modelID).Supports(router.CapExtendedContext) {
		return 1_000_000
	}
	return catalog.ContextWindowFor(modelID)
}

// excludeContextOverflowModels returns a copy of excluded augmented with every
// model in available whose context window is too small to serve the request,
// plus the sorted IDs of the models it newly excluded (for logging).
// est is the full-body token estimate (translate.RequestEnvelope.FullTokenEstimate).
// outputReserve is the expected output budget (feats.MaxTokens or the const above).
// Returns the original excluded map unchanged and a nil slice when no models are added.
func excludeContextOverflowModels(est, outputReserve int, excluded, available map[string]struct{}) (map[string]struct{}, []string) {
	if est <= 0 {
		return excluded, nil
	}
	needed := est + outputReserve
	var out map[string]struct{}
	var overflowed []string
	for model := range available {
		if _, alreadyExcluded := excluded[model]; alreadyExcluded {
			continue
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

// excludeGemini3xOnUnsignedHistory augments excluded with every Gemini 3.x model
// in available when the request history carries an assistant tool call lacking a
// Gemini thoughtSignature. Routing such a turn into Gemini 3.x is a guaranteed
// 400 (foreign/cross-model history — planner switch or tier clamp into Gemini),
// so the models are made ineligible and the scorer/clamp pick a non-Gemini
// candidate instead. Returns the original map unchanged (and nil) when nothing
// is added. Native Gemini continuations round-trip their own signature and are
// not affected.
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

// restrictToTier returns a copy of excluded augmented with every routable model
// whose tier differs from the target. It is the scorer-side counterpart to a
// dropped user-forced pin: when the forced model can no longer serve a turn
// (most often the session outgrew its context window and the pre-filter evicted
// it), the user still asked for a model of that tier, so the fresh decision
// should pick the next-best model in the same tier rather than collapsing to the
// cheap tier-default. ok is false (and the original map returned unchanged) when
// no in-tier model would survive the constraint, so the caller can leave routing
// unconstrained instead of handing the scorer an empty pool.
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
	// nil availableModels means "every model routable" (see WithAvailableModels);
	// enumerate the catalog in that case so the constraint still has a universe.
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

// servedOnSubscription reports whether the turn's resolved credential is a
// Claude subscription OAuth token — i.e. the customer's own plan paid for it,
// so billing applies only the subscription fee rather than full cost. The
// credential is read from the same ctx that resolveAndInjectCredentials
// stamped and dispatch used.
func servedOnSubscription(ctx context.Context) bool {
	creds := CredentialsFromContext(ctx)
	return creds != nil && creds.OAuth
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

func NewService(r router.Router, providerMap map[string]providers.Client, emitter *otel.Emitter, embedOnlyUserMessage bool, semanticCache *cache.Cache, pinStore sessionpin.Store, hardPinExplore bool, hardPinProvider, hardPinModel string, telemetry TelemetryRepository) *Service {
	return &Service{
		router:               r,
		providers:            providerMap,
		emitter:              emitter,
		embedOnlyUserMessage: embedOnlyUserMessage,
		semanticCache:        semanticCache,
		pinStore:             pinStore,
		noProgress:           newNoProgressTracker(),
		compaction:           newCompactionTracker(),
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
	return s
}

// WithPlannerEnabled is the kill switch. When false, the orchestrator
// preserves first-decision-wins behavior.
func (s *Service) WithPlannerEnabled(enabled bool) *Service {
	s.plannerEnabled = enabled
	return s
}

// WithEffortEscalation enables the escalate-on-failure reasoning-effort policy.
// When false (default) the router leaves request-derived effort untouched.
func (s *Service) WithEffortEscalation(enabled bool) *Service {
	s.effortEscalation = enabled
	return s
}

// WithLoopEscalationConfig sets the cyclic-loop escalation kill switch and the
// log-not-act holdout percentage. enabled=false keeps detection and telemetry
// running but never writes the escalation pin. holdoutPct is clamped to
// [0, 100]; it only takes effect when a LoopEscalationStore is wired, because
// a withheld rescue with no durable row is pure loss, not a measurement.
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

// forcedReasoningEffort implements the escalate-on-failure effort policy and
// returns the EmitOptions.ForceReasoningEffort override ("" = no override):
//
//   - gpt-5.x: "low" by default, "high" once escalate is set (an observed
//     failed/no-progress prior turn). Measured on SWE-Bench Pro, serving low
//     then retrying high on failure beats both fixed policies (low 24% < high
//     32% < escalate ~40% resolved) because it spends high only where it flips
//     the outcome and avoids the cases high regresses.
//   - gemini-3.x: pinned "low" — effort-immune on hard tasks (0/15 effort-helps
//     in the sweep), so high is wasted spend.
//   - everything else (incl. anthropic adaptive): "" — left to its own path.
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
func (s *Service) WithHardPinResolver(resolver func(enabled map[string]struct{}) (provider, model string, ok bool)) *Service {
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

// upstreamBodyLogHead and upstreamBodyLogTail bound the bytes attached to
// each "upstream prepared body" log entry. Real Claude Code turns routinely
// hit 50-400KB; logging them whole at Info would blow up GCP ingest cost and
// hit the 256KB per-entry hard cap. Head + tail captures both useful slices:
//
//   - head (4KB): JSON envelope, sampling params, system prompt header,
//     opening of tool definitions — answers "is the request shape right?"
//   - tail (4KB): end of the messages array, last tool_result / user
//     message / assistant turn — answers "what did the model just see?"
//
// Middle (typically tool def details + middle of message history) is dropped
// with a "…<n bytes omitted>…" marker so the truncation is obvious in the
// log. Bodies <= upstreamBodyLogHead + upstreamBodyLogTail go through whole.
const (
	upstreamBodyLogHead = 4 * 1024
	upstreamBodyLogTail = 4 * 1024
)

// logUpstreamBody emits the prepared upstream request body at Info so it
// shows up in GCP without flipping LOG_LEVEL. Used for per-turn investigation
// of "what did we actually send" — model misbehavior, broken tool shapes,
// prompt-cache stability, etc.
//
// Bodies over upstreamBodyLogHead+upstreamBodyLogTail are head+tail
// truncated. body_truncated + body_omitted_bytes make the cut obvious so a
// reader doesn't mistake a truncated body for a malformed one.
func logUpstreamBody(log *slog.Logger, sessionKey [sessionpin.SessionKeyLen]byte, decision router.Decision, feats translate.RoutingFeatures, body []byte) {
	bodyStr, truncated, omitted := truncateBodyForLog(body, upstreamBodyLogHead, upstreamBodyLogTail)
	log.Info("upstream prepared body",
		"session_key", hex.EncodeToString(sessionKey[:8]),
		"decision_model", decision.Model,
		"decision_provider", decision.Provider,
		"message_count", feats.MessageCount,
		"body_len", len(body),
		"body_truncated", truncated,
		"body_omitted_bytes", omitted,
		"body_head_limit", upstreamBodyLogHead,
		"body_tail_limit", upstreamBodyLogTail,
		"body", bodyStr,
	)
}

// truncateBodyForLog returns the body unchanged when it fits in head+tail,
// otherwise concatenates the first `head` bytes, an "…<n omitted>…" marker,
// and the last `tail` bytes. Returns (output, wasTruncated, omittedBytes).
func truncateBodyForLog(body []byte, head, tail int) (string, bool, int) {
	if len(body) <= head+tail {
		return string(body), false, 0
	}
	omitted := len(body) - head - tail
	var b strings.Builder
	b.Grow(head + tail + 32)
	b.Write(body[:head])
	fmt.Fprintf(&b, "\n…<%d bytes omitted>…\n", omitted)
	b.Write(body[len(body)-tail:])
	return b.String(), true, omitted
}

// ProxyMessages routes a raw Anthropic-Messages request body and streams the
// upstream response back. The routing decision is reflected in x-router-* headers.
// anthropicNativeAttempt builds the per-binding dispatch closure for an
// Anthropic-native upstream (no cross-format translation). prep is the
// emitted body for the attempt's model; the marker sink and usage extractor
// are rebuilt per attempt off the dispatched decision (d) so a baseline
// failover that switches the model id renders the routing marker for the
// model that actually served. setExtractor publishes the attempt's extractor
// to the caller for post-dispatch token attribution.
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
		// Post-commit streaming error: the routing-marker chunk has already
		// been flushed past the buffer to the wire; render the upstream error
		// as an in-stream `data: {...}` frame instead of letting dispatch's
		// flushErr append a corrupting Anthropic envelope. Pre-commit errors
		// are handled by dispatchWithFallback (Discard + flushErr).
		if err != nil && env.Stream() && preludeBuf.Committed() {
			err = emitAnthropicSSEErrorEvent(sink, err)
		}
		return err
	}
}

func (s *Service) ProxyMessages(ctx context.Context, body []byte, w http.ResponseWriter, r *http.Request) error {
	log := observability.FromContext(ctx)
	requestStart := time.Now()
	requestID := uuid.New().String()
	buf := otel.NewBuffer(s.emitter)
	ctx = buf.WithContext(ctx)

	// Strip the routing marker that prior cross-format responses injected as
	// standalone assistant text blocks. Without this, the marker round-trips
	// through clients that preserve content verbatim and ends up in upstream
	// context on every subsequent turn.
	body, stripErr := translate.StripRoutingMarkerFromMessages(body)
	if stripErr != nil {
		log.Error("Failed to strip routing marker from inbound messages", "err", stripErr)
		return fmt.Errorf("strip routing marker: %w", stripErr)
	}

	// Strip Claude Code's 1M-context model variant tag (e.g.
	// "claude-opus-4-8[1m]") to the canonical catalog id before parsing, so
	// routing, session pins, and telemetry key off the real model — and so the
	// tag never reaches a native Anthropic upstream, which 404s on it. The 1M
	// window is enabled separately, size-triggered via the context-1m beta.
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

	// Bind session_key/request_id/api_key_id/ingress onto a ctx-scoped logger so
	// every downstream log line in this turn carries them. The derived key is
	// reused for the force-model and loop-break paths below to avoid a second
	// hash + a divergent key in the rare case env.body mutates mid-flow.
	var sessionKey [sessionpin.SessionKeyLen]byte
	ctx, log, sessionKey = bindRequestLogger(ctx, env, apiKeyID, requestID, "anthropic_messages")
	log.Info("ProxyMessages start",
		"requested_model", feats.Model,
		"stream", env.Stream(),
		"message_count", feats.MessageCount,
		"has_tools", feats.HasTools,
		"total_input_tokens", feats.Tokens,
		"prompt_preview", preview(promptText, 200),
	)

	// Handle /force-model <model> and /unforce-model commands before routing.
	// The command is stripped from env.body so the upstream never sees it.
	// Session key is derived before extraction: ExtractForceModelCommand mutates
	// env.body, and DeriveSessionKey falls back to prompt text when
	// metadata.user_id is absent. Deriving after the strip would produce a key
	// that mismatches subsequent turns where the unstripped message is present.
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

	// Tool-call loop break: when the same (tool_name, args) appears at least
	// loopDetectionMaxRepeats times in the last loopDetectionWindowSize
	// assistant turns, synthesize end_turn and expire the session pin. Catches
	// runaway OSS-model tool-call cycles (qwen3, in particular) that the
	// previous-turn-maxed-out guard misses because each individual tool call
	// returns quickly and well under the output cap.
	// Wide cyclic re-read loop on a cheap/mid model (re-reading the same few
	// files for dozens of turns, no edits) → escalate the session to opus and
	// fall through to normal routing. The escalation pin (an immutable sticky)
	// takes effect from the next turn, like /force-model. This takes precedence
	// over the tight-loop break below: rescuing the session beats stopping it.
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
	excluded, ctxOverflowed := excludeContextOverflowModels(env.FullTokenEstimate(), outputReserve, baseExcluded, s.availableModels)
	if len(ctxOverflowed) > 0 {
		log.Info("context window pre-filter: excluded over-capacity models",
			"full_token_estimate", env.FullTokenEstimate(),
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

	// Snapshot inbound tool-call count before runTurnLoop potentially mutates
	// env (model-switch handover may call RewriteForHandover). The compaction
	// tracker must compare the count the client actually sent, not the
	// post-rewrite count, to avoid false-positive detection when the router
	// itself is the one that shortened the message window.
	inboundToolCallCount := len(env.AssistantToolCallSignatures())

	// Snapshot spiral signals from the inbound body for the same reason: a
	// model-switch handover replaces the history with a summary, which would
	// wipe error streaks / thrash / repetition on exactly the turns they
	// cross thresholds. The signals must reflect what the client actually
	// sent; the fire decision happens after routing, where the decision and
	// turn type are known.
	var inboundSpiralSignals spiralSignals
	if s.spiralShadowEnabled {
		inboundSpiralSignals = computeSpiralSignals(env, feats.MessageCount)
	}

	// Snapshot the inbound tool-output size before runTurnLoop, for the same
	// reason: a model-switch / compaction handover RewriteForHandover strips
	// tool_result blocks from env, which would zero out tool_result_bytes on a
	// genuine tool_result turn read at telemetry time.
	inboundLastUser := env.LastUserMessage()

	routeStart := time.Now()
	routeRes, routeErr := s.runTurnLoop(ctx, env, feats, apiKeyID, installationID, "", r.Header, router.Request{
		RequestedModel:       feats.Model,
		EstimatedInputTokens: feats.Tokens,
		HasTools:             feats.HasTools,
		HasImages:            feats.HasImages,
		PromptText:           promptText,
		EnabledProviders:     enabledProviders,
		ExcludedModels:       excluded,
		RoutingKnobs:         routingKnobsForRequest(ctx),
	})
	if routeErr != nil {
		log.Error("Routing failed", "err", routeErr, "route_ms", time.Since(routeStart).Milliseconds(), "requested_model", feats.Model, "total_input_tokens", feats.Tokens)
		return routeErr
	}
	routeRes.SuggestionMode = r.Header.Get("x-weave-suggestion-mode") == "true"
	decision := routeRes.Decision
	tt := routeRes.TurnType
	stickyHit := routeRes.StickyHit
	pinTier := routeRes.PinTier
	pinAgeSec := routeRes.PinAgeSec
	routeMs := time.Since(routeStart).Milliseconds()
	s.logPlannerOutcome(ctx, routeRes)

	// Cross-envelope no-progress detector: if this session has dispatched the
	// same (decision_model, decision_provider, message_count, tool-progress,
	// prompt-prefix) burst >= noProgressMatchThreshold times within
	// noProgressTimeWindow, the agent is stuck (a sub-agent spawn loop, or a
	// model re-issuing one identical call) and another dispatch will only
	// reproduce the same useless response. Break the pin and emit a synthetic
	// stop instead. The tool-progress marker is the primary guard: a
	// progressing agent appends a new, distinct tool call each turn, so its
	// fingerprint diverges and it is never mistaken for a stuck loop — even when
	// the top-level message count stays flat, as it does for Claude Code's
	// Explore sub-agent.
	if fp := computeNoProgressFingerprint(decision, promptText, feats.MessageCount, toolProgressMarker(env)); s.noProgress != nil {
		role := roleForTier(catalog.TierFor(feats.Model))
		if looped, count := s.noProgress.recordAndDetect(routeRes.SessionKey, installationID, role, fp, time.Now()); looped {
			return s.handleNoProgressBreak(ctx, w, env, count, installationID, routeRes.SessionKey, role, decision.Model, decision.Provider, feats.Tokens)
		}
	}

	// Shadow-mode spiral detector: log-only death-march signals (error grind,
	// same-file thrash, fuzzy repetition, monologue) recorded once per
	// (session, reason) so live fire rates and precision can be measured
	// before any escalation action is armed. Signals were snapshotted from
	// the inbound body above, before any handover rewrite. Main-loop /
	// tool-result turns only — hard-pinned turn types (Probe, TitleGen,
	// Compaction, sub-agent dispatch) carry history shapes that mimic the
	// signals.
	if s.spiralShadowEnabled && (tt == turntype.MainLoop || tt == turntype.ToolResult) {
		if reasons := spiralReasons(inboundSpiralSignals); len(reasons) > 0 {
			role := roleForTier(catalog.TierFor(feats.Model))
			// Use the bindRequestLogger digest (same DeriveSessionKey, computed
			// unconditionally) rather than routeRes.SessionKey, which is zero
			// when no pin store is configured. This keeps the spiral event's
			// session_key equal to the telemetry row's in every mode so the
			// offline join holds; in the pinned production path the two are
			// already identical, so this is a no-op there.
			s.handleSpiralShadow(ctx, inboundSpiralSignals, reasons, installationID, sessionKey, role, decision.Model, string(tt))
		}
	}

	// Compaction-aware handover: Claude Code can trim its history window in two
	// ways — full compaction (messageCount drops sharply) or rolling-window
	// trimming (messageCount flat but tool-call count shrinks by one per turn).
	// Either case leaves the non-Anthropic model without awareness of edits and
	// decisions that lived only in the now-elided turns. Detect either drop and
	// rewrite the envelope with a handover summary before dispatch.
	compactionHandoverRan := false
	var compactionHandoverOutcome handoverOutcome
	// Skip detection for all hard-pinned turn types (Compaction, Probe, TitleGen,
	// Classifier, SubAgentDispatch with hardPinExplore). These turns carry far
	// fewer messages than main-loop turns — a Probe or TitleGen after a long
	// session would show a sharp count drop that mimics client history trimming
	// and falsely trigger runCompactionHandover. Hard-pinned turns also do not
	// model the conversational context the compaction handover is meant to
	// preserve, so rewrites there would be both wrong and wasteful.
	//
	// Also skip when the planner already ran a model-switch handover for this
	// turn (routeRes.Handover.Invoked). Applying runCompactionHandover on top of
	// an already-rewritten envelope would double-trim it.
	if decision.Provider != providers.ProviderAnthropic && s.compaction != nil && !routeRes.HardPinned && !routeRes.Handover.Invoked {
		role := roleForTier(catalog.TierFor(feats.Model))
		if s.compaction.checkAndRecord(routeRes.SessionKey, installationID, role, feats.MessageCount, inboundToolCallCount) {
			log.Info("Context trimming detected on non-Anthropic route; rewriting context with handover summary",
				"message_count", feats.MessageCount,
				"tool_call_count", inboundToolCallCount,
				"decision_model", decision.Model,
				"decision_provider", decision.Provider,
			)
			compactionHandoverOutcome = s.runCompactionHandover(ctx, env, r.Header, decision.Model)
			compactionHandoverRan = true
		}
	}

	// Semantic-cache eligibility: configured, non-streaming, decision has
	// metadata, externalID present, not eval traffic.
	// Skip when a compaction handover rewrote env: the embedding in
	// decision.Metadata was computed from the pre-handover body, so a cache
	// hit would return a response built for different upstream context.
	cacheEligible := s.semanticCache != nil && !env.Stream() && decision.Metadata != nil && externalID != "" && !bypassEval && !compactionHandoverRan
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

	ctx = resolveAndInjectCredentials(ctx, decision.Provider, r.Header)

	// Wrap the client writer in a preludeBuffer for every request, not just
	// multi-binding ones. The buffer absorbs per-attempt Prelude bytes (the
	// routing-marker text block + message_start) so that when the upstream
	// errors before producing its first byte, we discard the buffered prelude
	// and render an upstream-error envelope instead of stranding the marker
	// on the wire. Single-binding requests previously bypassed the buffer for
	// TTFB, but the v0.58 SWE-bench bake-off attributed 46/84 empty-patch
	// failures to that bypass: half of all upstream calls api_error'd, and
	// each one delivered a turn that was just `✦ **Weave Router** → …` text
	// to Claude Code, which then rejected the turn for missing tool_use.
	// The TTFB cost is a single round-trip's worth of buffered SSE bytes
	// (~200B) released the moment the upstream's first byte arrives.
	bindings := s.resolveBindingsForDispatch(ctx, decision)
	contentSink, contentCap := s.maybeCaptureResponse(w)
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
	// (finish_reason, emitted stop_reason, tool_use count) for the completion
	// log. Populated by the translator-backed paths; stays zero for the
	// Anthropic-native passthrough path, which has no translator.
	var respSummary translate.ResponseSummary
	// reqStats captures the translation-time mutations applied to the
	// winning attempt's upstream request body. Overwritten on each attempt;
	// on dispatch success it reflects the binding that actually ran. Zero
	// for Anthropic-native passthrough — that path skips the translator.
	var reqStats providers.RequestMutationStats

	marker := suppressMarkerIfRequested(r.Header, routingMarkerFor(routeRes))
	// toolValidator compiles the request's tool schemas once for all attempts
	// (LRU-cached across turns); response translators validate and repair
	// model-emitted tool calls against it. Nil when the request has no tools.
	toolValidator := env.ToolValidator()
	// markerSink wraps sink with an AnthropicRoutingMarkerWriter per attempt.
	// Unlike translator-backed paths, the Anthropic-native writer must wait
	// for upstream headers so non-2xx responses can stay buffered/retryable.
	setExtractor := func(e *otel.UsageExtractor) { extractor = e }
	var attempt dispatchAttempt
	switch decision.Provider {
	case providers.ProviderAnthropic:
		prep, emitErr := env.PrepareAnthropic(r.Header, opts)
		if emitErr != nil {
			log.Error("Failed to emit Anthropic body", "err", emitErr)
			return fmt.Errorf("emit body: %w", emitErr)
		}
		logUpstreamBody(log, routeRes.SessionKey, decision, feats, prep.Body)
		attempt = s.anthropicNativeAttempt(env, r, prep, sink, preludeBuf, marker, setExtractor)
	case providers.ProviderOpenAI, providers.ProviderOpenRouter, providers.ProviderFireworks, providers.ProviderDeepInfra, providers.ProviderBedrock:
		crossFormat = true
		// Prep rebuilt per attempt: targetIsOpenRouter(opts) gates four
		// OpenRouter-only body fields (provider hint, reasoning, system
		// reminder, tool-temp override). If the primary is Fireworks and
		// we fail over to OpenRouter, the second attempt's body must be
		// re-emitted with TargetProvider = openrouter so those gates fire.
		attempt = func(actx context.Context, d router.Decision, p providers.Client) error {
			attemptOpts := opts
			attemptOpts.TargetProvider = d.Provider
			respSummary = translate.ResponseSummary{}
			// Reasoning OpenAI models (gpt-5.x) reject reasoning_effort + tools
			// on /v1/chat/completions; an agentic Anthropic client that asks the
			// model to reason must go through the Responses API instead. Scoped
			// to the direct OpenAI provider (the only one with /v1/responses).
			useResponses := d.Provider == providers.ProviderOpenAI &&
				attemptOpts.Capabilities.Supports(router.CapReasoning) &&
				feats.HasTools && env.ReasoningRequested()
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
					WithToolValidator(toolValidator)
			}
			if err := translator.Prelude(env.Stream()); err != nil {
				log.Error("Anthropic SSE prelude failed (OpenAI upstream)", "err", err)
			}
			if preludeBuf != nil {
				preludeBuf.Seal()
			}
			err := p.Proxy(actx, d, prep, translator, r)
			// Post-commit streaming error: the preludeBuffer has already
			// flushed HTTP 200 + message_start past the buffer to the
			// wire, so the dispatch loop's flushErr would append a
			// trailing JSON envelope that corrupts the SSE stream.
			// Render the upstream error as an in-stream `event: error`
			// frame instead. Pre-commit errors are handled cleanly by
			// dispatchWithFallback (Discard + flushErr).
			if err != nil && env.Stream() && preludeBuf.Committed() {
				err = emitAnthropicSSEErrorEvent(sink, err)
			}
			finErr := finalizeAfterProxy(err, translator.Finalize)
			respSummary = translator.Summary()
			return finErr
		}
	case providers.ProviderGoogle:
		crossFormat = true
		prep, emitErr := env.PrepareGemini(r.Header, opts)
		reqStats = prep.Stats
		if emitErr != nil {
			log.Error("Failed to translate Anthropic request to Gemini format", "err", emitErr)
			return fmt.Errorf("translate anthropic request to gemini: %w", emitErr)
		}
		logUpstreamBody(log, routeRes.SessionKey, decision, feats, prep.Body)
		attempt = func(actx context.Context, d router.Decision, p providers.Client) error {
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
				WithToolValidator(toolValidator)
			if err := anthropicTr.Prelude(env.Stream()); err != nil {
				log.Error("Anthropic SSE prelude failed (Gemini upstream)", "err", err)
			}
			if preludeBuf != nil {
				preludeBuf.Seal()
			}
			geminiTr := translate.NewGeminiToOpenAISSETranslator(anthropicTr, d.Model, nil)
			err := p.Proxy(actx, d, prep, geminiTr, r)
			// Post-commit streaming error: see ProxyMessages OpenAI-compat
			// case for rationale — render upstream error as in-stream
			// `event: error` rather than corrupt the SSE stream.
			if err != nil && env.Stream() && preludeBuf.Committed() {
				err = emitAnthropicSSEErrorEvent(sink, err)
			}
			err = finalizeAfterProxy(err, geminiTr.Finalize)
			finErr := finalizeAfterProxy(err, anthropicTr.Finalize)
			respSummary = anthropicTr.Summary()
			return finErr
		}
	default:
		return fmt.Errorf("%w: %s (no translation path defined for inbound Anthropic Messages)", ErrProviderNotConfigured, decision.Provider)
	}

	// In-turn baseline failover eligibility. When the router cost-routes an
	// Anthropic-model request to an OSS/Gemini model and every binding for that
	// model fails (provider outage or model-not-found), the turn should fall
	// back to the requested model on Anthropic rather than hard-fail with
	// "the selected model may not exist". Eligible only when: the request isn't
	// BYOK/inbound-credential bound (those resolve to a single provider),
	// Anthropic isn't excluded for the installation (otherwise failing over to
	// Anthropic would violate the exclusion contract — and deferring the OSS
	// error to a baseline that then can't dispatch would surface a generic
	// gateway failure instead of the real upstream error), the routed model
	// isn't already Anthropic, and the baseline is a known Anthropic-served
	// catalog model distinct from the routed one. Computed pre-dispatch so the
	// primary dispatch defers its exhaustion flush to us.
	baselineModel := s.baselineFor(feats.Model)
	baselineCatalog, baselineKnown := catalog.ByID(baselineModel)
	_, anthropicExcluded := s.excludedProvidersForRequest(ctx)[providers.ProviderAnthropic]
	baselineEligible := s.shouldFailover(ctx) &&
		!anthropicExcluded &&
		decision.Provider != providers.ProviderAnthropic &&
		baselineModel != decision.Model &&
		baselineKnown && baselineCatalog.PrimaryProvider() == providers.ProviderAnthropic

	primaryProvider := decision.Provider
	var winnerIdx int
	winnerIdx, proxyErr = s.dispatchWithFallback(ctx, failoverInputs{
		// contentSink routes the failover-exhaustion error envelope through the
		// content-capture writer; it is the raw w when capture is off.
		w:                      contentSink,
		buf:                    preludeBuf,
		initialDecision:        decision,
		bindings:               bindings,
		attempt:                attempt,
		flushErr:               flushUpstreamErrorAsAnthropic,
		deferFlushOnExhaustion: baselineEligible,
	})

	// The routed model's bindings all failed with a fault a different model
	// could satisfy, and nothing reached the client yet — re-dispatch the
	// requested model on Anthropic. crossFormat/respSummary/reqStats are reset
	// to their Anthropic-native (no-translator) values so completion telemetry
	// reflects the binding that actually served.
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
		// Recompute the switch flag against the baseline model that actually
		// serves (not the cost-routed OSS id). Otherwise PrepareAnthropic may
		// leave stale signed thinking blocks the baseline model rejects → 400.
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
			baselineCtx := resolveAndInjectCredentials(ctx, providers.ProviderAnthropic, r.Header)
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
			// Reflect whether the baseline actually served the turn — a failed
			// Anthropic retry must not report baseline_failover=true and skew
			// bake-off / incident analysis.
			baselineFailoverUsed = proxyErr == nil
		}
	} else if baselineEligible && proxyErr != nil {
		// We deferred the primary's exhaustion flush but didn't run the baseline
		// (response committed mid-stream, or a non-failoverable error). Surface
		// the original upstream error envelope now.
		flushUpstreamErrorAsAnthropic(contentSink, proxyErr)
	}

	finalProvider := primaryProvider
	if winnerIdx >= 0 && winnerIdx < len(bindings) {
		finalProvider = bindings[winnerIdx].Provider
	} else if baselineAttempted {
		// Baseline failover ran but no binding served (winnerIdx == -1). The
		// last provider attempted was Anthropic with the baseline model, so
		// finalProvider must match decision.Model rather than reverting to the
		// OSS primary that never served the requested baseline model.
		finalProvider = providers.ProviderAnthropic
	}
	decision.Provider = finalProvider

	// Re-resolve actual pricing for the binding that actually served the
	// request. The pre-dispatch lookup (`otel.Lookup(decision.Model)`)
	// always returns the catalog's PRIMARY binding price; on a successful
	// failover we'd otherwise debit + report the primary's per-1M rate
	// while the request was actually billed at the fallback's rate.
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
		Bool("dispatch.failover_used", finalProvider != primaryProvider).
		Bool("dispatch.baseline_failover", baselineFailoverUsed)
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
		failoverUsed := finalProvider != primaryProvider
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
			// Evict the session pin so the next turn re-scores instead of
			// continuing to serve the same misbehaving model. The current
			// turn has already been streamed (cannot retry), but eviction
			// is the best available recovery for the session's next turn.
			s.evictPinAfterDegenerateResponse(ctx, stickyHit, decision.Reason, installationID, routeRes.SessionKey, routeRes.PinRole)
		}
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
			// ToolUseBlocks and InvalidToolArgsBlocks are only valid when a
			// translator ran (StopReason is populated). The Anthropic-native
			// passthrough path leaves respSummary zero; storing 0 there would
			// look like a measured zero-tool turn rather than missing data.
			ToolUseBlocks:         int32PtrIfKnown(int32(respSummary.ToolUseBlocks), respSummary.StopReason != ""),
			InvalidToolArgsBlocks: int32PtrIfKnown(int32(respSummary.InvalidToolArgsBlocks), respSummary.StopReason != ""),
			FailoverUsed:          boolPtrTrue(failoverUsed),
			DegenerateShadow:      boolPtrOrNil(degShadow),
			// (session_key, role) are the offline join key to spiral_shadow_events
			// and session_pins. sessionKey is the bindRequestLogger digest — the
			// SAME DeriveSessionKey(env, apiKeyID) the turn loop and spiral use, but
			// computed unconditionally, so it is populated even on hard-pin turns
			// and no-pin-store paths where routeRes.SessionKey stays zero. On the
			// main_loop/tool_result turns spiral actually writes, the two are equal
			// byte-for-byte. role mirrors routeRes.PinRole (set unconditionally) =
			// roleForTier(catalog.TierFor(feats.Model)), the spiral join value.
			SessionKey: sessionKey[:],
			Role:       routeRes.PinRole,
			// Shadow-mode hysteresis instrumentation: the fresh scorer's pick +
			// score vector (captured even on STAY, where obs.CandidateScores is
			// NULL) and the loaded pin's age, so the downgrade opportunity is
			// measurable offline. No routing action is taken on these.
			FreshDecisionModel:   obs.FreshDecisionModel,
			FreshCandidateScores: obs.FreshCandidateScores,
			PinAgeSec:            int64PtrIf(stickyHit && pinAgeSec > 0, pinAgeSec),
			// Shadow-mode tier-cap instrumentation: incoming tool-output size on
			// tool_result turns (the structural triviality signal). NULL on turns
			// with no trailing tool_result. No routing action is taken on it.
			ToolResultBytes: toolResultBytesPtr(inboundLastUser, tt),
		})
	}

	// Debit prepaid credits — no-op when billing is unwired (selfhosted).
	// The cache-hit branch above already returned, so we only reach this
	// point on a real upstream call.
	if proxyErr == nil {
		s.emitBilling(ctx, requestID, externalID, decision, actPricing, routeRes, in, out, cacheCreation, cacheRead)
		if compactionHandoverOutcome.Invoked && !compactionHandoverOutcome.FallbackToFullHistory {
			sumUsage := compactionHandoverOutcome.SummaryUsage
			if sumUsage.Model != "" && (sumUsage.InputTokens > 0 || sumUsage.OutputTokens > 0) {
				sumPricing, _ := catalog.PrimaryPriceFor(sumUsage.Model)
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
				})
			}
		}
	}

	// Two-strike pin eviction: a session pinned to a model that keeps
	// returning non-retryable 4xx wedges until the user manually
	// /force-model's out. Increment a persistent counter and expire the
	// pin once it reaches the threshold so the next turn re-routes.
	// Successful turns reset the counter.
	s.maybeEvictPinAfterUpstreamErr(ctx, stickyHit, proxyErr, decision.Reason, installationID, routeRes.SessionKey, routeRes.PinRole)

	// One event per tool_use block that failed toolcheck validation —
	// including blocks deterministic repair recovered. Queryable per
	// model×provider, so beyond debugging it doubles as a per-upstream
	// tool-calling-quality signal for future routing decisions.
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

	log.Info("ProxyMessages complete", "requested_model", feats.Model, "baseline_model", s.baselineFor(feats.Model), "decision_model", decision.Model, "decision_provider", decision.Provider, "primary_provider", primaryProvider, "fallback_attempts", winnerIdx, "failover_used", finalProvider != primaryProvider, "decision_reason", decision.Reason, "requested_tier", routeRes.RequestedTier.String(), "decision_tier", catalog.TierFor(decision.Model).String(), "embedded_tokens", len(promptText)/4, "total_input_tokens", feats.Tokens, "has_tools", feats.HasTools, "message_count", feats.MessageCount, "last_kind", feats.LastKind, "last_preview", feats.LastPreview, "embed_input", embedInput, "cross_format", crossFormat, "sticky_hit", stickyHit, "route_ms", routeMs, "proxy_ms", proxyMs, "proxy_err", proxyErr, "upstream_status", upstreamStatus(proxyErr), "upstream_finish_reason", respSummary.UpstreamFinishReason, "resp_stop_reason", respSummary.StopReason, "stop_reason_promoted", respSummary.StopReasonPromoted, "tool_use_blocks", respSummary.ToolUseBlocks, "invalid_tool_args_blocks", respSummary.InvalidToolArgsBlocks, "text_only_turn_nudged", respSummary.TextOnlyTurnNudged, "stop_reason_demoted", respSummary.StopReasonDemoted, "suppressed_tool_calls", respSummary.SuppressedToolCalls, "tool_call_invalid_blocks", len(respSummary.ToolCallIssues), "cc_only_tools_stripped", reqStats.CCOnlyToolsStripped, "gemini_reminder_injected", reqStats.GeminiReminderInjected, "resp_output_tokens", respSummary.OutputTokens, "prelude_committed", preludeBuf.Committed())
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

// pinDecision rehydrates a router.Decision from a stored pin. Metadata is nil
// (embedding isn't persisted, acceptable since the pin short-circuits routing).
func pinDecision(p sessionpin.Pin) router.Decision {
	return router.Decision{
		Provider: p.Provider,
		Model:    p.Model,
		Reason:   p.Reason,
	}
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
// or client-supplied creds for upstream calls. The summarizer is wired with
// deployment-level creds; calling it on a BYOK request would route prior
// conversation context through the platform account, violating tenant data
// boundaries. The orchestrator uses this to skip the summarizer and pass the
// full prior history through unchanged.
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
		providers.ProviderDeepInfra,
		providers.ProviderBedrock,
	} {
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
// never as a licence to enable other OpenAI-compat upstreams that share the
// same Authorization header format. A router-key-authed request must rely on
// BYOK; a header on such a request is for the inbound surface only.
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
	// A caller's Claude subscription enrolls Anthropic for routing
	// eligibility, mirroring resolveAndInjectCredentials so the scorer can
	// actually pick a Claude model. The dedicated X-Weave-Anthropic-Subscription
	// header unambiguously carries only a subscription token, so — like
	// credential injection — it is honored even on router-keyed requests, past
	// the installation guard below. Without this a managed request carrying
	// only a subscription token (no BYOK) leaves Anthropic out of the enabled
	// set and the scorer fails with ErrNoEligibleProvider before any Claude
	// turn runs. Anthropic-only: the token can't authenticate any other
	// upstream. (The self-hosted inbound-bearer path is already covered by the
	// ExtractClientCredentials block below.)
	if subscriptionCredsFromHeaderValue(anthropicSubscriptionFromContext(ctx)) != nil {
		out[providers.ProviderAnthropic] = struct{}{}
	}
	// Likewise, a Claude subscription bearer (sk-ant-oat-) in the inbound
	// Authorization enrolls Anthropic even on router-keyed requests — the
	// managed Claude Code path keeps its OAuth token there while the router key
	// rides in X-Weave-Router-Key. Mirrors resolveAndInjectCredentials so the
	// scorer can pick a Claude model the subscription will pay for. OAuth-subset
	// only (ExtractClientCredentials gates OAuth to sk-ant-oat-): a general
	// inbound API key still cannot enroll a provider on the router-key path.
	if c := ExtractClientCredentials(providers.ProviderAnthropic, headers); c != nil && c.OAuth {
		out[providers.ProviderAnthropic] = struct{}{}
	}
	// Passthrough-eligible providers are surface-scoped: a provider
	// registered without a deployment key joins the eligible set only when
	// the inbound surface matches. Otherwise an Anthropic-surface request's
	// `x-api-key` would flow to api.openai.com (and vice versa) when no
	// BYOK / env keys are configured — a cross-provider credential leak
	// even when upstream 401s.
	//
	// Skip when the request is router-key-authed (installationID set) and
	// surfaceProvider isn't already enrolled via BYOK. Passthrough depends on
	// the client's inbound auth header, but for router-key auth that header
	// IS the router key — setAuth strips it, so the upstream call would
	// dispatch unauthenticated and 401 instead of failing fast with a 503.
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
// them on ctx, in precedence order: a caller's Claude subscription token
// (Anthropic only) first, then BYOK, then a client-supplied header credential.
//
// Subscription-first lets a caller's own Claude subscription pay for their
// Claude turns. It arrives one of two ways, both honored even on router-keyed
// requests: the dedicated X-Weave-Anthropic-Subscription header, or a
// subscription OAuth bearer (sk-ant-oat-) in the inbound Authorization — the
// shape Claude Code sends when routed through the Weave Router (router key in
// X-Weave-Router-Key, its own subscription token left in Authorization).
//
// The inbound-bearer path is restricted to the OAuth subset: a general client
// API key is still NOT extracted on the router-key path, since that would let
// the client's inbound Anthropic key be forwarded to a different upstream
// provider. The deployment-level env key on the provider client is the correct
// fallback in that case.
func resolveAndInjectCredentials(ctx context.Context, provider string, headers http.Header) context.Context {
	routerKeyed := installationIDFromContext(ctx) != (uuid.UUID{})
	if provider == providers.ProviderAnthropic {
		// Subscription-first (precedence: subscription -> BYOK -> deployment). A
		// caller's Claude subscription pays for their Claude turns ahead of any
		// BYOK or deployment key. It arrives via the dedicated header on
		// router-keyed requests, or — when not router-keyed — as the inbound
		// Authorization bearer. Resolving it before the BYOK lookup keeps the
		// precedence explicit here rather than relying on BYOK being absent off
		// the router-key path (it is today, but a future BYOK-loading path must
		// not silently outrank the subscription).
		if sub := subscriptionCredsFromHeaderValue(anthropicSubscriptionFromContext(ctx)); sub != nil {
			observability.FromContext(ctx).Info("Resolved Claude subscription credential",
				"credential_source", sub.Source)
			return context.WithValue(ctx, CredentialsContextKey{}, sub)
		}
		// A Claude subscription bearer (sk-ant-oat-) in the inbound Authorization
		// is honored even on router-keyed requests. Claude Code routed through
		// the Weave Router keeps its own subscription OAuth token in
		// Authorization while the router key rides in X-Weave-Router-Key, so a
		// managed CC turn pays from the caller's own plan without needing the
		// dedicated header. Restricted to the OAuth subset: ExtractClientCredentials
		// only sets OAuth for sk-ant-oat-, so a general inbound API key is still
		// NOT forwarded on the router-key path (the cross-provider-leak guard
		// below still applies to it).
		if inbound := ExtractClientCredentials(provider, headers); inbound != nil && inbound.OAuth {
			observability.FromContext(ctx).Info("Resolved Claude subscription credential",
				"credential_source", inbound.Source)
			return context.WithValue(ctx, CredentialsContextKey{}, inbound)
		}
	}
	byok := BuildCredentialsMap(externalKeysFromContext(ctx))
	var creds *Credentials
	if byok != nil {
		creds = byok[provider]
	}
	if creds == nil && !routerKeyed {
		creds = ExtractClientCredentials(provider, headers)
	}
	if creds != nil {
		return context.WithValue(ctx, CredentialsContextKey{}, creds)
	}
	return ctx
}

// addTimingAttrs appends derived latency attributes from the request Timing.
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
// pin_age_sec: callers gate on sticky_hit AND a positive age, so hard-pin turns
// and no-pin turns (which carry sticky_hit but never compute a real age, leaving
// it 0) stay NULL instead of recording a spurious measured zero that would skew
// min-dwell analysis.
func int64PtrIf(known bool, v int64) *int64 {
	if !known {
		return nil
	}
	return &v
}

// toolResultBytesPtr returns the incoming tool-output size for telemetry on a
// tool_result turn, else nil. It takes an inbound LastUserMessage snapshot, NOT
// the live env: a model-switch or compaction handover may call RewriteForHandover
// and strip tool_result blocks from env before the telemetry write, which would
// otherwise read 0 bytes on a genuine tool_result turn. The snapshot is taken
// before runTurnLoop, alongside inboundToolCallCount / inboundSpiralSignals.
//
// Gated on the classified turn type, not just info.HasToolResult: the
// Anthropic/Gemini walkers report the last *user* message in the whole history,
// so a request ending in a trailing assistant reply after a prior tool_result
// would otherwise write a stale non-NULL value. turntype.ToolResult is itself
// derived from LastKind=="tool_result" (the trailing message), so this ties the
// column exactly to its meaning. NULL elsewhere so a 0 stays distinct from "no
// tool output this turn".
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
// stopReasonDemoted guards against false positives from cross-format demotions:
// broken finish_reason="tool_calls" turns that the translator demotes to end_turn
// (zero surviving tool blocks) must not fire the degenerate shadow — they are
// handled translation failures, not genuinely empty completions.
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
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.telemetry.InsertRequestTelemetry(ctx, p); err != nil {
			observability.Get().Debug("Telemetry insert failed", "err", err, "request_id", p.RequestID)
		}
	}()
}

// emitBilling debits the customer for one upstream call and, on switch
// turns that invoked the handover summarizer successfully, a second
// debit for the summary call under a `_summary` request_id suffix. Safe
// to call when billing is unwired or externalID is empty — both branches
// no-op.
//
// Pricing for the summary turn is looked up from the canonical pricing
// table by the summarizer's reported model name. Unknown model → zero
// pricing → notional_cost=0 ledger row (still recorded so the audit
// trail is complete even if the price table doesn't know about a
// freshly-deployed handover model).
func (s *Service) emitBilling(ctx context.Context, requestID, externalID string, decision router.Decision, actPricing catalog.Pricing, routeRes turnLoopResult, in, out, cacheCreation, cacheRead int) {
	if s.billing == nil || externalID == "" {
		return
	}
	hasOverride := billing.HasOverrideFromContext(ctx)
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
	})

	// The handover summary runs on the deployment/BYOK key (never the
	// subscription token — see resolveSummarizerCreds), so it bills at full
	// cost regardless of whether the main turn was subscription-served.
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
			})
		}
	}
}

// fireBilling debits the org's prepaid credit balance for one upstream
// call. Synchronous so the ledger row is durable before handler return,
// but uses context.Background() so cancellation by the customer doesn't
// abort the write — the inference has already been served and we owe
// ourselves the bookkeeping. 5s timeout matches fireTelemetry.
//
// On failure we log Error with full context for manual reconciliation;
// the customer's response is unaffected because they already got it.
// The accompanying OTel span lets log-based metrics alert on debit
// failure rate without adding a prometheus dependency.
//
// Inputs are intentionally small — composition root wires up everything
// the billing service needs; this hook only forwards token counts +
// pricing + request metadata.
func (s *Service) fireBilling(ctx context.Context, p billing.DebitInferenceParams) {
	if s.billing == nil {
		return
	}
	if p.OrganizationID == "" {
		// Shouldn't happen on managed-mode authed requests; middleware
		// already pulled installation.ExternalID. Log Debug so a synthetic
		// test exercising the hook doesn't page on-call.
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

// logBillingDebitFailure emits a structured Error log and OTel attributes
// so on-call alerting can fire on the resulting log/span rate without
// requiring a new prometheus dependency. Counter-style metrics are
// derivable from the structured log query in the dashboard panel.
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
	log := observability.FromContext(ctx)
	requestStart := time.Now()
	requestID := uuid.New().String()
	buf := otel.NewBuffer(s.emitter)
	ctx = buf.WithContext(ctx)

	apiKeyID, _ := ctx.Value(APIKeyIDContextKey{}).(string)
	externalID, _ := ctx.Value(ExternalIDContextKey{}).(string)
	installationID := installationIDFromContext(ctx)
	clientID := ClientIdentityFrom(ctx)

	body, stripErr := translate.StripRoutingMarkerFromMessages(body)
	if stripErr != nil {
		log.Error("Failed to strip routing marker from OpenAI messages", "err", stripErr)
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
		"prompt_preview", preview(promptText, 200),
	)

	// Handle /force-model <model> and /unforce-model commands before routing.
	// The command is stripped from env.body so the upstream never sees it.
	// Session key is derived before extraction: ExtractForceModelCommand mutates
	// env.body, and DeriveSessionKey falls back to prompt text when
	// metadata.user_id is absent. Deriving after the strip would produce a key
	// that mismatches subsequent turns where the unstripped message is present.
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

	// Pre-filter models whose context window cannot fit this request.
	outputReserveOAI := contextWindowOutputReserve
	if feats.MaxTokens > outputReserveOAI {
		outputReserveOAI = feats.MaxTokens
	}
	baseExcludedOAI := s.excludedModelsForRequest(ctx)
	excludedOAI, ctxOverflowedOAI := excludeContextOverflowModels(env.FullTokenEstimate(), outputReserveOAI, baseExcludedOAI, s.availableModels)
	if len(ctxOverflowedOAI) > 0 {
		log.Info("context window pre-filter: excluded over-capacity models",
			"full_token_estimate", env.FullTokenEstimate(),
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

	// Snapshot the inbound tool-output size before runTurnLoop (which may rewrite
	// env via switch handover); see toolResultBytesPtr.
	inboundLastUser := env.LastUserMessage()

	routeStart := time.Now()
	routeRes, err := s.runTurnLoop(ctx, env, feats, apiKeyID, installationID, subAgentHint, r.Header, router.Request{
		RequestedModel:       feats.Model,
		EstimatedInputTokens: feats.Tokens,
		HasTools:             feats.HasTools,
		HasImages:            feats.HasImages,
		PromptText:           promptText,
		EnabledProviders:     enabledProviders,
		ExcludedModels:       excludedOAI,
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

	cacheEligible := s.semanticCache != nil && !env.Stream() && decision.Metadata != nil && externalID != "" && !bypassEval
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
	contentSink, contentCap := s.maybeCaptureResponse(w)
	preludeBuf := newPreludeBuffer(contentSink)
	var rootSink http.ResponseWriter = preludeBuf

	// Responses entry point delegates the eager response.created emit to
	// this layer because it has the post-routing binding count. Fire only
	// when single-binding so multi-binding requests stay failover-safe
	// (Codex client sees response.created via ResponsesWriter's lazy
	// emitCreated on the first upstream byte instead).
	if rw, ok := w.(*translate.ResponsesWriter); ok && len(bindings) <= 1 {
		if err := rw.Prelude(env.Stream()); err != nil {
			log.Error("Responses prelude failed", "err", err)
		}
	}

	var captureW *captureWriter
	var sink http.ResponseWriter = rootSink
	if cacheEligible {
		captureW = newCaptureWriter(rootSink, semanticCacheMaxBodyBytes)
		sink = captureW
	}

	marker := suppressMarkerIfRequested(r.Header, routingMarkerFor(routeRes))
	_, isResponses := w.(*translate.ResponsesWriter)
	// markerSink wraps sink with an OpenAIRoutingMarkerWriter that emits
	// the routing-marker chunk + HTTP 200 eagerly (Prelude). Skipped when
	// the inbound is /v1/responses (ResponsesWriter handles its own badge)
	// and when no marker is configured for this route. Called per attempt
	// so retries re-emit into a fresh preludeBuffer state.
	makeMarkerSink := func() http.ResponseWriter {
		if marker == "" || isResponses {
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
	switch decision.Provider {
	case providers.ProviderOpenAI, providers.ProviderOpenRouter, providers.ProviderFireworks, providers.ProviderDeepInfra, providers.ProviderBedrock:
		// Prep rebuilt per attempt: targetIsOpenRouter(opts) gates four
		// OpenRouter-only body fields (provider hint, reasoning, system
		// reminder, tool-temp override) that the Fireworks/DeepInfra/
		// Bedrock primary should not see. On failover to OpenRouter the
		// body must be re-emitted with TargetProvider = openrouter.
		attempt = func(actx context.Context, d router.Decision, p providers.Client) error {
			attemptOpts := opts
			attemptOpts.TargetProvider = d.Provider
			prep, emitErr := env.PrepareOpenAI(r.Header, attemptOpts)
			if emitErr != nil {
				log.Error("Failed to emit OpenAI body", "err", emitErr, "decision_provider", d.Provider)
				return fmt.Errorf("emit body: %w", emitErr)
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
			// Post-commit streaming error: the routing-marker chunk has
			// already been flushed past the buffer to the wire; render
			// the upstream error as an in-stream `data: {...}` frame
			// instead of letting dispatch's flushErr append a corrupting
			// JSON envelope. Pre-commit errors are handled by
			// dispatchWithFallback (Discard + flushErr).
			if err != nil && env.Stream() && preludeBuf.Committed() {
				err = emitOpenAISSEErrorEvent(sink, err)
			}
			return err
		}
	case providers.ProviderGoogle:
		crossFormat = true
		prep, emitErr := env.PrepareGemini(r.Header, opts)
		if emitErr != nil {
			log.Error("Failed to translate OpenAI request to Gemini format", "err", emitErr)
			return fmt.Errorf("translate openai request to gemini: %w", emitErr)
		}
		attempt = func(actx context.Context, d router.Decision, p providers.Client) error {
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
			err := p.Proxy(actx, d, prep, translator, r)
			// Post-commit streaming error: see same-format OpenAI case above.
			if err != nil && env.Stream() && preludeBuf.Committed() {
				err = emitOpenAISSEErrorEvent(sink, err)
			}
			return finalizeAfterProxy(err, translator.Finalize)
		}
	case providers.ProviderAnthropic:
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
		// contentSink routes the failover-exhaustion error envelope through the
		// content-capture writer; it is the raw w when capture is off.
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

	// Re-resolve actual pricing for the binding that actually served the
	// request — see ProxyMessages for the rationale.
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
	}

	// See ProxyMessages for the two-strike eviction rationale.
	s.maybeEvictPinAfterUpstreamErr(ctx, stickyHit, proxyErr, decision.Reason, installationIDFromContext(ctx), routeRes.SessionKey, routeRes.PinRole)

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
			// Shadow-mode tier-cap instrumentation: incoming tool-output size on
			// tool_result turns (the structural triviality signal). NULL on turns
			// with no trailing tool_result. No routing action is taken on it.
			ToolResultBytes: toolResultBytesPtr(inboundLastUser, tt),
		})
	}

	log.Info("ProxyOpenAIChatCompletion complete", "requested_model", feats.Model, "baseline_model", s.baselineFor(feats.Model), "decision_model", decision.Model, "decision_provider", decision.Provider, "primary_provider", primaryProvider, "fallback_attempts", winnerIdx, "failover_used", finalProvider != primaryProvider, "decision_reason", decision.Reason, "requested_tier", routeRes.RequestedTier.String(), "decision_tier", catalog.TierFor(decision.Model).String(), "embedded_tokens", len(promptText)/4, "total_input_tokens", feats.Tokens, "has_tools", feats.HasTools, "embed_input", embedInput, "cross_format", crossFormat, "sticky_hit", stickyHit, "pin_tier", pinTier, "turn_type", string(tt), "route_ms", routeMs, "proxy_ms", proxyMs, "proxy_err", proxyErr, "upstream_status", upstreamStatus(proxyErr))
	return proxyErr
}

// ProxyOpenAIResponses routes an OpenAI Responses API request. The Responses
// wire format is translated to Chat Completions on entry, dispatched through
// the existing chat-completions path, then the chat-completions response is
// re-emitted as Responses-shaped SSE / JSON. This keeps the turn loop, cache,
// pricing, and translation matrix unchanged.
func (s *Service) ProxyOpenAIResponses(ctx context.Context, body []byte, w http.ResponseWriter, r *http.Request) error {
	chatBody, _, model, err := translate.ResponsesToChatCompletions(body)
	if err != nil {
		return fmt.Errorf("translate responses request: %w", err)
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
	// Prelude (response.created emit) deferred to ProxyOpenAIChatCompletion.
	// It knows the post-routing decision and the binding count; only fires
	// eagerly when the request is single-binding (no failover possible).
	// Multi-binding requests rely on ResponsesWriter.Write's lazy
	// emitCreated on first upstream byte instead — losing #220's TTFB win
	// on /v1/responses for multi-binding models, but preserving the failover
	// invariant that nothing reaches the client before the upstream
	// commits.
	proxyErr := s.ProxyOpenAIChatCompletion(ctx, chatBody, wrapper, r)
	if proxyErr != nil {
		// On error, let the handler write the error envelope unless we've
		// already committed to streaming — in which case the chat-completions
		// path will have surfaced a status error and we just propagate.
		deferredLog.run()
		return proxyErr
	}
	finErr := wrapper.Finalize()
	deferredLog.run()
	return finErr
}
