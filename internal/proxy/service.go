package proxy

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"weave-os/router/internal/auth"
	"weave-os/router/internal/billing"
	"weave-os/router/internal/feedback"
	"weave-os/router/internal/flags"
	"weave-os/router/internal/observability"
	"weave-os/router/internal/observability/otel"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/proxy/usage"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/bandit"
	"weave-os/router/internal/router/bandswap"
	"weave-os/router/internal/router/cache"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/router/handover"
	"weave-os/router/internal/router/hmm"
	"weave-os/router/internal/router/planner"
	"weave-os/router/internal/router/policy"
	"weave-os/router/internal/router/rl"
	"weave-os/router/internal/router/sessionpin"
	"weave-os/router/internal/router/sessionstrategy"
	"weave-os/router/internal/router/turntype"
	"weave-os/router/internal/sse"
	"weave-os/router/internal/subscriptions"
	"weave-os/router/internal/timing"
	"weave-os/router/internal/translate"
	"weave-os/router/internal/websearch"

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
	// strategies contains every non-default router and its optional lifecycle
	// reporters. Adding a strategy does not require another Service field.
	strategies                   map[router.Strategy]registeredStrategy
	providers                    map[string]providers.Client
	translationCompatibilityMode TranslationCompatibilityMode
	// scopedSearchRequirement gates CitationsOrSearch on actual (current or recent)
	// search-tool use, not mere advertisement; env ROUTER_SCOPED_SEARCH_REQUIREMENT.
	scopedSearchRequirement bool
	// searchRequirementDecayTurns bounds how many routed turns after the last
	// actual use keep the requirement. Env ROUTER_SEARCH_REQUIREMENT_DECAY_TURNS.
	searchRequirementDecayTurns int
	// searchUse tracks per-session recent search-tool use for
	// scopedSearchRequirement.
	searchUse            *searchUseTracker
	emitter              TelemetryEmitter
	embedOnlyUserMessage bool
	semanticCache        *cache.Cache
	// pinStore persists session-sticky routing decisions. Nil when the feature
	// flag is off; the orchestrator then runs the scorer every turn.
	pinStore sessionpin.Store
	// sessionStrategyStore persists the explicit per-session /beta selection.
	// Stable routing is represented by no row.
	sessionStrategyStore sessionstrategy.Store
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
	hardPinResolver HardPinResolver
	// subAgentProvider/subAgentModel override hardPinProvider/hardPinModel
	// for SubAgentDispatch turns only; unset leaves compaction/probe/title-gen/
	// classifier on the shared hard pin.
	subAgentProvider string
	subAgentModel    string
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
	// noResponsesGateways memoizes gateway endpoints that answered they have no
	// Responses API, so only the first tool turn against such an endpoint pays
	// the probe. Keyed by gatewayResponsesKey.
	noResponsesGateways sync.Map
	// noPromptCacheKeyGateways memoizes gateway endpoints that rejected
	// prompt_cache_key as an unknown field, so only the first turn against
	// such an endpoint pays the 400. Keyed by gatewayResponsesKey.
	noPromptCacheKeyGateways sync.Map
	// unservedGatewayModels memos (endpoint, model) pairs a gateway answered
	// model-not-found for. Keyed by gatewayModelKey.
	unservedGatewayModels sync.Map
	// excludedModelsOverride, when non-nil, replaces the per-installation
	// exclusion list on every request. Set from ROUTER_EXCLUDED_MODELS at boot.
	excludedModelsOverride map[string]struct{}
	// excludedProvidersOverride, when non-nil, replaces the per-installation
	// provider exclusion list on every request. Set from
	// ROUTER_EXCLUDED_PROVIDERS at boot.
	excludedProvidersOverride map[string]struct{}
	// globalAutomaticExclusions caches the deployment-wide models the control
	// plane withdrew from automatic routing. Nil leaves every model automatically
	// selectable.
	globalAutomaticExclusions *globalAutomaticExclusionCache
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
	// hmmUpgradeConfidenceThreshold is the minimum classifier confidence needed
	// for HMM to switch upward to a more expensive model despite cache inertia.
	hmmUpgradeConfidenceThreshold float64
	// hmmSameTierPin suppresses EV-positive same-tier lateral switches once a
	// session pin is live. Env ROUTER_HMM_SAME_TIER_PIN, off by default.
	hmmSameTierPin bool
	// authoritativeUpgradeGate applies the upgrade-confidence threshold to
	// authoritative-per-turn decisions too: a scored fresh decision that is
	// pricier than the session pin only escalates at confidence >=
	// hmmUpgradeConfidenceThreshold. Env ROUTER_AUTHORITATIVE_UPGRADE_GATE, on by default.
	authoritativeUpgradeGate bool
	// authorityCacheShadow records what the HMM cache gate would have decided on
	// an authoritative-per-turn turn, which returns before that gate can run.
	// Observation only -- it never changes the served decision. Env
	// ROUTER_AUTHORITY_CACHE_SHADOW, on by default.
	authorityCacheShadow bool
	// policyDeadlineFallback degrades a policy sidecar deadline/transport failure to
	// the session pin instead of a 503. Kill switch: env ROUTER_POLICY_DEADLINE_FALLBACK, off by default.
	policyDeadlineFallback bool
	// policyDeadlineDefaultModel is the tier-3 static fallback on a policy deadline
	// miss with no session pin (~0.2% of failures); empty = fail-closed (503). Env ROUTER_POLICY_DEADLINE_DEFAULT_MODEL.
	policyDeadlineDefaultModel string
	// plannerEnabled is the kill switch. When false, the orchestrator falls
	// back to first-decision-wins behavior.
	plannerEnabled bool
	// scoreToolResultTurns is the kill switch (ROUTER_SCORE_TOOL_RESULT_TURNS).
	// When true (default), ToolResult turns run the cluster scorer + planner
	// for MainLoop parity; when false, the pin is reused verbatim (#82 path).
	// Runs the embedder on ToolResult traffic (majority of turns).
	scoreToolResultTurns bool
	// cyberRefusalRepin is the kill switch (ROUTER_CYBER_REFUSAL_REPIN, default
	// on). When true, a safety refusal on the anthropic-native path re-pins
	// the session off the refusing model (opus ~45% refusal rate; sonnet 0%).
	// Refusal detection itself is unconditional — the flag gates only the action.
	cyberRefusalRepin bool
	// anthropicServerSideFallback (ROUTER_ANTHROPIC_SERVER_SIDE_FALLBACK, default on)
	// opts Anthropic-targeted requests into server-side fallback: Anthropic re-serves
	// a refused turn on a fallback model (rescues the current turn; cyberRefusalRepin
	// only re-pins future turns).
	anthropicServerSideFallback bool

	// siblingFailover is the kill switch (ROUTER_SIBLING_FAILOVER, default on)
	// for degrading to a same-cluster candidate when every binding of the
	// routed model fails with a transient upstream fault.
	siblingFailover bool
	// openAIResponsesBroad is the deployment default for
	// ROUTER_OPENAI_RESPONSES_BROAD; see ResolveOpenAIResponsesBroad.
	openAIResponsesBroad bool
	// allowedModelsHeader is the deployment default for
	// ROUTER_ALLOWED_MODELS_HEADER; see ResolveAllowedModelsHeader.
	allowedModelsHeader bool
	// sseKeepalive is the client-silence budget before a ping is injected
	// (ROUTER_SSE_KEEPALIVE_INTERVAL_SECONDS; 0 disables). See sse.KeepaliveWriter.
	sseKeepalive time.Duration
	// cyberRefusalFallbackModel is the model to re-pin to on a cyber refusal
	// when the session pin carries no runner-up (PairedModel). Set from
	// ROUTER_CYBER_REFUSAL_FALLBACK_MODEL; defaults to claude-sonnet-5.
	cyberRefusalFallbackModel string
	// effortEscalation enables the escalate-on-failure reasoning-effort policy:
	// gpt-5.x serves low effort by default and high after an observed
	// failed/no-progress turn; gemini is pinned low. Off by default (set from
	// ROUTER_EFFORT_ESCALATION) so it can be baked off before enabling.
	effortEscalation bool
	// ccOrchToolsCrossVendor keeps CC orchestration tools (Task*, Workflow,
	// Skill, plan-mode) on cross-vendor routes. Kill switch:
	// ROUTER_CC_ORCH_TOOLS_CROSSVENDOR=false strips all CC-only tools.
	ccOrchToolsCrossVendor bool
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

	// turnSignalCaptureEnabled gates per-turn snapshots independently of the
	// threshold-only shadow detector. Privacy gates always take precedence.
	turnSignalCaptureEnabled bool
	// textRepetitionBreakEnabled gates the enforcing assistant-text repetition
	// detector (see text_repetition.go). Defaults true; kill switch is
	// ROUTER_TEXT_REPETITION_BREAK_ENABLED.
	textRepetitionBreakEnabled bool
	// spiralTracker de-duplicates shadow fires per (session, role, reason) on
	// this replica.
	spiralTracker *spiralTracker
	// struggleShadowEnabled gates the shadow-mode struggle detector (log-only
	// session-level grind signals; see struggle_detection.go). Defaults true —
	// shadow mode changes no routing behavior.
	struggleShadowEnabled bool
	// struggleTracker de-duplicates shadow fires per (session, role, reason) on
	// this replica.
	struggleTracker *struggleTracker
	// struggleShadowStore persists shadow struggle detections durably
	// (router.struggle_shadow_events) and enforces the once-per-(session,
	// reason) budget. Nil degrades to log-only fires.
	struggleShadowStore StruggleShadowStore
	// struggleEscalationEnabled is the kill switch; arms early sideways move
	// (turns>=30, wall>=10m). Default off (ROUTER_STRUGGLE_ESCALATION_ENABLED).
	struggleEscalationEnabled bool
	// struggleEscalationHoldoutPct is the percentage of struggling sessions
	// withheld for measurement. Only applies when a store is wired.
	struggleEscalationHoldoutPct int
	// struggleEvidenceArming lets behavioral spiral evidence arm an escalation
	// before the turn/wall thresholds. Default off (ROUTER_STRUGGLE_EVIDENCE_ARMING).
	struggleEvidenceArming bool
	// struggleEscalationStore persists struggle escalation events durably
	// (router.struggle_escalation_events). Set by WithStruggleEscalationStore.
	struggleEscalationStore StruggleEscalationStore
	// struggleEscalationRoster picks the next untried arm in the same cluster
	// for a sideways move. Set by WithStruggleEscalationRoster.
	struggleEscalationRoster StruggleEscalationRoster
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
	// webSearch executes Anthropic's native web-search server tool for
	// upstreams that reject it. nil leaves such turns on normal routing.
	webSearch websearch.Executor
	// compactionSummarizer produces the structured summary for the proactive
	// context-window compaction cascade (maybeCompact). nil disables Tier-3
	// summarization (the cascade still runs Tier-1 cleanup + trim rescue).
	compactionSummarizer CompactionSummarizer
	// compactionTriggerPct is the fraction of the largest eligible model's
	// context window at which the compaction cascade engages. Zero disables
	// compaction entirely.
	compactionTriggerPct float64
	// compactionModel is the Anthropic-family model the cascade summarizes
	// with (and Claude Code's own compaction turn is pinned to) when the
	// session has no warm Anthropic pin. Empty means DefaultCompactionModel.
	compactionModel string
	// compactionHardPinEnabled routes Claude Code's own compaction turn through
	// compactionHardPin instead of the generic utility hard-pin. Off unless
	// the composition root enables it (an operator ROUTER_HARD_PIN_MODEL
	// keeps winning for every hard-pinned turn).
	compactionHardPinEnabled bool
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
	// now, when non-nil, overrides the clock dispatchWithFallback uses to
	// price the same-binding retry budget. Tests inject a fake to simulate a
	// slow attempt without burning real time; prod leaves it nil (time.Now).
	now func() time.Time
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
	// planAwareSubscriptionRouting removes models whose only linked
	// subscription plan is exhausted. It is request-scoped and never mutates
	// the deployment roster.
	planAwareSubscriptionRouting bool
	// managedSubscriptions leases encrypted, owner-scoped Claude/Codex
	// subscription credentials. Nil leaves the legacy credential path unchanged.
	managedSubscriptions subscriptions.Leaser
}

type registeredStrategy struct {
	router       router.Router
	unavailable  error
	capabilities policy.Capabilities
	outcomes     policy.OutcomeReporter
	feedback     policy.FeedbackReporter
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

// nativeResponsesBodyContextKey carries the caller's original /v1/responses
// body (badge-stripped for Codex); which model needs it is only known post-routing.
type nativeResponsesBodyContextKey struct{}

// nativeResponsesReasoningHashContextKey preserves reasoning that only native
// Responses dispatch can represent.
type nativeResponsesReasoningHashContextKey struct{}

// nativeResponsesToolHashContextKey preserves native Responses tool identity.
type nativeResponsesToolHashContextKey struct{}

// codexFeedbackSkillContextKey marks a Responses request where Codex is
// returning output from a user-invoked $rf skill.
type codexFeedbackSkillContextKey struct{}

// responsesFooterEchoedContextKey is set when the original Responses input
// already carries a rating hint after the last human turn.
type responsesFooterEchoedContextKey struct{}

// InstallationExcludedModelsContextKey is the context key for the authed
// installation's model exclusion list. Carried as []string.
type InstallationExcludedModelsContextKey struct{}

// InstallationAllowedModelsContextKey is the context key for the authed
// installation's positive model allowlist. Carried as []string; empty/absent
// means no restriction.
type InstallationAllowedModelsContextKey struct{}

// InstallationSubscriptionModelsWhenActiveContextKey is the context key for the active-subscription conditional model allowlist.
type InstallationSubscriptionModelsWhenActiveContextKey struct{}

// InstallationSubscriptionModelsWhenInactiveContextKey is the context key for the exhausted-subscription conditional model allowlist.
type InstallationSubscriptionModelsWhenInactiveContextKey struct{}

// InstallationSubscriptionConditionalModelsContextKey is the context key for the request-selected conditional model allowlist.
type InstallationSubscriptionConditionalModelsContextKey struct{}

// InstallationExcludedProvidersContextKey is the context key for the authed
// installation's provider exclusion list. Carried as []string.
type InstallationExcludedProvidersContextKey struct{}

// SessionDisabledProvidersContextKey carries providers struck out by repeated
// 529 exhaustion ([]string). Stashed after runTurnLoop so
// resolveBindingsForDispatch's failover walk honors the exclusion too.
type SessionDisabledProvidersContextKey struct{}

// InstallationPreferredModelsContextKey is the context key for the authed
// installation's model priority ranking. Carried as []string in descending
// preference (index 0 = first preference). See preferredModelsForRequest.
type InstallationPreferredModelsContextKey struct{}

// InstallationFastModeModelsContextKey is the context key for the authed
// installation's fast-mode opt-in list, carried as []string of catalog model
// IDs. Every dispatch of a listed model goes out on the provider's fast tier
// (OpenAI service_tier=priority, Anthropic speed=fast) and is billed at that
// tier's rate; routing still scores on list price. See fastModeForAttempt.
type InstallationFastModeModelsContextKey struct{}

// InstallationRoutingKnobsContextKey is the context key for the authed
// installation's persisted routing preference (the "quality vs price" dial).
// Carried as *router.Overrides with only Alpha (quality weight) set; the
// per-request x-weave-routing-* header override takes precedence over it. See
// routingKnobsForRequest.
type InstallationRoutingKnobsContextKey struct{}

// ClusterModelListsContextKey is the context key for the authed API key's
// per-cluster ordered allowlists (map[string][]string). Set by auth middleware.
type ClusterModelListsContextKey struct{}

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

// InstallationHideTerminalSurfacesContextKey is the context key for the
// installation's hide-terminal-surfaces toggle (bool; absent == false);
// suppresses the routing marker, feedback footer, and feedback-link header.
type InstallationHideTerminalSurfacesContextKey struct{}

// PolicyTrainingAllowedContextKey carries the installation's explicit
// learning eligibility. Absence is fail-closed (false).
type PolicyTrainingAllowedContextKey struct{}

// PolicyDebugEnabledContextKey carries persisted or internal-request debug mode.
type PolicyDebugEnabledContextKey struct{}

// PolicyRoutingIntentContextKey carries a strategy-neutral routing preset.
type PolicyRoutingIntentContextKey struct{}

// PolicyRolloutIDContextKey carries the installation-level rollout identifier.
type PolicyRolloutIDContextKey struct{}

// PolicyShadowStrategyContextKey carries an optional comparison-only strategy.
// Its decision is collected asynchronously and never affects dispatch.
type PolicyShadowStrategyContextKey struct{}

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
const policyOutcomeReportTimeout = 2 * time.Second
const policyFeedbackReportTimeout = 2 * time.Second
const policyShadowDecisionTimeout = 3 * time.Second
const policyOutcomeResponseMaxBytes = 256 * 1024

type policyOutcomeResponse struct {
	Body      []byte
	Truncated bool
}

// suppressMarkerIfRequested returns "" when the request opted out via
// routingMarkerHeader or the installation has hidden terminal surfaces,
// otherwise the marker unchanged. Only applies to the per-turn routing badge;
// no-progress/loop/force-model markers always fire.
func suppressMarkerIfRequested(ctx context.Context, h http.Header, marker string) string {
	if hideTerminalSurfacesForRequest(ctx) {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(h.Get(routingMarkerHeader))) {
	case "off", "false", "0", "none":
		return ""
	}
	return marker
}

// routingMarkerFor builds the "brand → model · note" snippet emitted when the
// selected serving model changes (and on the first routed turn).
func routingMarkerFor(res turnLoopResult) string {
	decision := res.Decision
	if decision.Model == "" {
		return ""
	}
	if res.SuggestionMode {
		return ""
	}
	// A dropped force-model pin contradicts an ack the user already saw, so it
	// prints even when the automatic fallback is a normally hidden hard pin.
	if res.ForcedPinDropped {
		parts := []string{"✦ **Weave Router** → " + decision.Model, markerReasonForcedPinDropped}
		if res.ForcedPinModel != "" {
			parts = []string{
				"✦ **Weave Router** → " + decision.Model,
				fmt.Sprintf("%s (%s)", markerReasonForcedPinDropped, res.ForcedPinModel),
			}
		}
		return strings.Join(parts, " · ") + "\n\n"
	}
	// Hard pins (compaction / sub-agent) return before the pin is loaded, so
	// PriorServedModel is always empty there — suppress explicitly rather than
	// letting it read as a first turn.
	if res.HardPinned {
		return ""
	}
	// Same model as last turn: the user already knows. Empty prior model means
	// the first turn of this session (or role), which still shows. Effort changes
	// do not constitute a new model choice for this display surface.
	if baseModelOf(res.PriorServedModel) == res.Decision.Model {
		return ""
	}
	// A sidecar-supplied marker is a genuine per-turn status line (e.g.
	// "Delegating work with ...") independent of whether the serving model
	// changed, so it bypasses the planner-free / switch gate below and always
	// prints when reached.
	if decision.Metadata != nil {
		if marker := sanitizeSidecarDisplayMarker(decision.Metadata.DisplayMarker); marker != "" {
			return marker + "\n\n"
		}
	}
	parts := []string{"✦ **Weave Router** → " + decision.Model}
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
	markerReasonUserForced        = "pinned by force-model"
	markerReasonLoopEscalated     = "escalated due to loop"
	markerReasonStruggleEscalated = "picked a different model to break a grind"
	markerReasonSwitched          = "switched for positive EV after cache eviction"
	markerReasonStayed            = "stayed on your last pick"
	markerReasonTierUpgrade       = "upgraded to a stronger tier"
	markerReasonBestPick          = "best pick for this turn"
	markerReasonBaseline          = "fell back to baseline after provider outage"
	markerReasonSibling           = "switched after the picked model was overloaded"
	markerReasonForcedPinDropped  = "your force-model pin could not be served this turn"
)

// baselineRoutingMarkerFor renders the routing badge for an in-turn baseline
// failover. The requested model now serves on Anthropic, so the badge names the
// baseline model rather than the cost-routed OSS slug that went dark. Honors
// suggestion mode like routingMarkerFor; the caller applies the opt-out header.
func baselineRoutingMarkerFor(res turnLoopResult, baselineModel string) string {
	if res.SuggestionMode || baselineModel == "" {
		return ""
	}
	// A failover that lands back on the model already serving is a no-op repeat;
	// only a genuine switch to a different model is worth surfacing.
	if baseModelOf(res.PriorServedModel) == baselineModel {
		return ""
	}
	return "✦ **Weave Router** → " + baselineModel + " · " + markerReasonBaseline + "\n\n"
}

// siblingRoutingMarkerFor renders the routing badge for an in-turn same-cluster
// failover, naming the candidate that actually serves.
func siblingRoutingMarkerFor(res turnLoopResult, siblingModel string) string {
	if res.SuggestionMode || siblingModel == "" || baseModelOf(res.PriorServedModel) == siblingModel {
		return ""
	}
	return "✦ **Weave Router** → " + siblingModel + " · " + markerReasonSibling + "\n\n"
}

// routingReasonShort returns a short user-facing reason for the routing
// decision, or empty when the underlying code is internal recovery noise.
func routingReasonShort(res turnLoopResult) string {
	if res.PlannerDecision.Reason != "" {
		return humanReasonFromPlanner(res.PlannerDecision.Reason)
	}
	switch res.Decision.Reason {
	case translate.ReasonUserForceModel:
		return markerReasonUserForced
	case translate.ReasonLoopEscalation:
		return markerReasonLoopEscalated
	case translate.ReasonStruggleEscalation:
		return markerReasonStruggleEscalated
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
	case planner.ReasonEVNegative, planner.ReasonNoPriorUsage, planner.ReasonSameTierPinned:
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

// installationAllowedModelsFromContext returns the per-installation positive
// allowlist stashed on ctx by the auth middleware, or nil when absent.
func installationAllowedModelsFromContext(ctx context.Context) []string {
	v := ctx.Value(InstallationAllowedModelsContextKey{})
	if v == nil {
		return nil
	}
	out, _ := v.([]string)
	return out
}

func installationSubscriptionModelsWhenActiveFromContext(ctx context.Context) []string {
	v := ctx.Value(InstallationSubscriptionModelsWhenActiveContextKey{})
	if v == nil {
		return nil
	}
	out, _ := v.([]string)
	return out
}

func installationSubscriptionModelsWhenInactiveFromContext(ctx context.Context) []string {
	v := ctx.Value(InstallationSubscriptionModelsWhenInactiveContextKey{})
	if v == nil {
		return nil
	}
	out, _ := v.([]string)
	return out
}

func subscriptionConditionalModelsForRequest(ctx context.Context) []string {
	v := ctx.Value(InstallationSubscriptionConditionalModelsContextKey{})
	if v == nil {
		return nil
	}
	out, _ := v.([]string)
	return out
}

func subscriptionConditionalModelsConfigured(ctx context.Context) bool {
	return ctx.Value(InstallationSubscriptionConditionalModelsContextKey{}) != nil
}

// allowedModelsForRequest returns the effective positive model allowlist as a
// set: the installation policy allowlist further narrowed by a request-level
// AllowedModelsHeader subset when one is present. Nil = no policy.
func allowedModelsForRequest(ctx context.Context) map[string]struct{} {
	policy := installationAllowedModelSet(ctx)
	subset := requestAllowedModelSet(ctx)
	if subset == nil {
		return policy
	}
	if policy == nil {
		return subset
	}
	out := make(map[string]struct{}, len(subset))
	for m := range subset {
		if _, ok := policy[m]; ok {
			out[m] = struct{}{}
		}
	}
	return out
}

// installationAllowedModelSet returns the installation's positive model allowlist as a set,
// intersecting the installation list with the selected subscription-state list.
// Nil = no policy; non-nil empty = fails closed (intentional empty intersection).
func installationAllowedModelSet(ctx context.Context) map[string]struct{} {
	base := installationAllowedModelsFromContext(ctx)
	conditional := subscriptionConditionalModelsForRequest(ctx)
	conditionalConfigured := subscriptionConditionalModelsConfigured(ctx)
	if len(base) == 0 && !conditionalConfigured {
		return nil
	}
	if !conditionalConfigured {
		out := make(map[string]struct{}, len(base))
		for _, m := range base {
			out[m] = struct{}{}
		}
		return out
	}
	if len(base) == 0 {
		out := make(map[string]struct{}, len(conditional))
		for _, m := range conditional {
			out[m] = struct{}{}
		}
		return out
	}
	conditionalSet := make(map[string]struct{}, len(conditional))
	for _, m := range conditional {
		conditionalSet[m] = struct{}{}
	}
	out := make(map[string]struct{}, len(base))
	for _, m := range base {
		if _, ok := conditionalSet[m]; ok {
			out[m] = struct{}{}
		}
	}
	return out
}

// modelPermittedByAllowlist reports whether model clears the org's positive
// allowlist. Must be used directly for models outside routableUniverse —
// passthrough-only models (no Tier) never enter the desugared exclusion set.
// A nil allowlist means no restriction; an empty effective intersection fails
// closed.
func modelPermittedByAllowlist(ctx context.Context, model string) bool {
	allowed := installationAllowedModelSet(ctx)
	if allowed == nil {
		return true
	}
	_, ok := allowed[model]
	return ok
}

// routableUniverse is every model this deployment can serve: the configured
// availableModels set, or the whole catalog when it is nil. Extracted so the
// allowlist desugaring and restrictToTier share one universe definition and
// cannot drift.
func (s *Service) routableUniverse() map[string]struct{} {
	if s.availableModels != nil {
		return s.availableModels
	}
	out := make(map[string]struct{}, len(catalog.Models))
	for _, m := range catalog.Models {
		out[m.ID] = struct{}{}
	}
	return out
}

// subscriptionRoutingDisabledForRequest reports whether the authed installation
// has turned off subscription-aware routing. When true, the subscription
// subsidy bonus is suppressed for this request so routing decides on merits.
func subscriptionRoutingDisabledForRequest(ctx context.Context) bool {
	disabled, _ := ctx.Value(InstallationSubscriptionRoutingDisabledContextKey{}).(bool)
	return disabled
}

// hideTerminalSurfacesForRequest reports whether terminal surfaces are hidden for this request.
func hideTerminalSurfacesForRequest(ctx context.Context) bool {
	hide, _ := ctx.Value(InstallationHideTerminalSurfacesContextKey{}).(bool)
	return hide
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

// safetyExcludedModels returns the hard request-time safety exclusion set
// (context-overflow + gemini-unsigned-history). It re-runs both filters
// against an EMPTY base — the routing-path filters skip models already in
// excluded_models, so a policy-excluded overflow model would be absent from
// those lists yet must still block bypass (it would 400 on the subscription).
// Returns nil when neither filter fires.
func (s *Service) safetyExcludedModels(env *translate.RequestEnvelope, outputReserve int, enabledProviders map[string]struct{}) map[string]struct{} {
	_, overflowed := excludeContextOverflowModels(env.ContextOverflowTokenEstimate(), env.SignatureTokenSavings(), outputReserve, enabledProviders, nil, s.availableModels)
	_, geminiUnsigned := excludeGemini3xOnUnsignedHistory(env, nil, s.availableModels)
	if len(overflowed) == 0 && len(geminiUnsigned) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(overflowed)+len(geminiUnsigned))
	for _, m := range overflowed {
		out[m] = struct{}{}
	}
	for _, m := range geminiUnsigned {
		out[m] = struct{}{}
	}
	return out
}

// excludedModelsForRequest returns the request's model exclusion set.
// Env override wins (intentional escape hatch, not an oversight).
// Otherwise desugars the positive allowlists into the exclusion set: every
// routable model absent from the effective allowlist is excluded.
func (s *Service) excludedModelsForRequest(ctx context.Context) map[string]struct{} {
	return s.excludedModelsFor(ctx, allowedModelsForRequest(ctx))
}

// policyExcludedModels is excludedModelsForRequest without the request-level
// AllowedModelsHeader subset: installation policy only. A user force is
// validated against this set because the strict pin outranks the header.
func (s *Service) policyExcludedModels(ctx context.Context) map[string]struct{} {
	return s.excludedModelsFor(ctx, installationAllowedModelSet(ctx))
}

func (s *Service) excludedModelsFor(ctx context.Context, allowed map[string]struct{}) map[string]struct{} {
	if s.excludedModelsOverride != nil {
		return s.excludedModelsOverride
	}
	excluded := installationExcludedModelsFromContext(ctx)
	out := make(map[string]struct{}, len(excluded))
	for _, m := range excluded {
		out[m] = struct{}{}
	}
	for model := range subscriptionPlanAwareExcludedModelsFromContext(ctx) {
		out[model] = struct{}{}
	}
	if allowed != nil {
		for model := range s.routableUniverse() {
			if _, ok := allowed[model]; !ok {
				out[model] = struct{}{}
			}
		}
	}
	for model := range s.gatewayUnservedModelsForRequest(ctx) {
		out[model] = struct{}{}
	}
	if len(out) == 0 {
		return nil
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

// sessionDisabledProvidersFromContext extracts the SessionDisabledProvidersContextKey
// value set by runTurnLoop.
func sessionDisabledProvidersFromContext(ctx context.Context) []string {
	v := ctx.Value(SessionDisabledProvidersContextKey{})
	if v == nil {
		return nil
	}
	out, _ := v.([]string)
	return out
}

// policyExcludedProviders returns configured exclusions only. Session
// strike-outs are omitted — transient 529 evidence must not veto a force
// the operator permits.
func (s *Service) policyExcludedProviders(ctx context.Context) map[string]struct{} {
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

// excludedProvidersForRequest merges the deployment/installation exclusion list
// with any providers this session has struck out for repeated 529 exhaustion.
func (s *Service) excludedProvidersForRequest(ctx context.Context) map[string]struct{} {
	base := s.policyExcludedProviders(ctx)
	sessionDisabled := sessionDisabledProvidersFromContext(ctx)
	if len(sessionDisabled) == 0 {
		return base
	}
	out := make(map[string]struct{}, len(base)+len(sessionDisabled))
	for p := range base {
		out[p] = struct{}{}
	}
	for _, p := range sessionDisabled {
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

// installationFastModeModelsFromContext returns the per-installation fast-mode
// opt-in list stashed on ctx by the auth middleware, or nil when none is present.
func installationFastModeModelsFromContext(ctx context.Context) []string {
	v := ctx.Value(InstallationFastModeModelsContextKey{})
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

// clusterArmOverridesForRequest returns per-cluster arm overrides from ctx, or
// nil when none are configured. Merges the API-key-scoped list (org default)
// with the resolved user's own selection — see mergeClusterOverrides for the
// composition rule. Only consumed by the HMM sidecar router.
func clusterArmOverridesForRequest(ctx context.Context) map[string][]string {
	v := ctx.Value(ClusterModelListsContextKey{})
	keyScoped, _ := v.(map[string][]string)
	return mergeClusterOverrides(keyScoped, auth.UserClusterModelListsFrom(ctx))
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
func contextWindowForRequest(modelID string, provider ...string) int {
	if router.Lookup(modelID).Supports(router.CapExtendedContext) {
		return 1_000_000
	}
	if len(provider) > 0 && provider[0] != "" {
		return catalog.ContextWindowForBinding(modelID, provider[0])
	}
	return catalog.ContextWindowFor(modelID)
}

// minContextWindowForModel returns the smallest context window any enabled
// binding can serve. Uses MIN because the primary binding dispatches first —
// a 512K primary blocks even when a 1M fallback exists. Nil enabledProviders
// falls back to the model-level window.
func minContextWindowForModel(model string, enabledProviders map[string]struct{}) int {
	cw := contextWindowForRequest(model)
	if len(enabledProviders) == 0 {
		return cw
	}
	m, ok := catalog.ByID(model)
	if !ok || len(m.Providers) == 0 {
		return cw
	}
	for _, b := range m.Providers {
		if _, enabled := enabledProviders[b.Provider]; !enabled {
			continue
		}
		if w := contextWindowForRequest(model, b.Provider); w < cw {
			cw = w
		}
	}
	return cw
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
func excludeContextOverflowModels(est, sigSavings, outputReserve int, enabledProviders, excluded, available map[string]struct{}) (map[string]struct{}, []string) {
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
		cw := minContextWindowForModel(model, enabledProviders)
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
	return (creds != nil && creds.OAuth) || managedSubscriptionServed(ctx)
}

// servedOnBYOK reports whether the turn's resolved credential is a customer-owned
// provider key. Keys off the resolved credential rather than the presence of
// a BYOK row: the row may exist for a provider this turn didn't route to.
func servedOnBYOK(ctx context.Context) bool {
	creds := CredentialsFromContext(ctx)
	return creds != nil && creds.Source == credSourceBYOK
}

// byokServedForProvider reports whether the installation has a usable BYOK key
// for provider. Used to bill summarizer calls at the fee rate: those dispatch on
// their own credential context, so the outer ctx's resolved credential is stale.
// Inspects Plaintext emptiness only — never key bytes — to satisfy CodeQL
// go/clear-text-logging.
func byokServedForProvider(ctx context.Context, provider string) bool {
	if provider == "" {
		return false
	}
	for _, key := range externalKeysFromContext(ctx) {
		// Mirrors BuildCredentialsMap's filter: an empty-plaintext row can't
		// authenticate an upstream call, so it isn't "served on BYOK".
		if key.Provider == provider && len(key.Plaintext) > 0 {
			return true
		}
	}
	return false
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
	// Subscription routing disabled: skip verbatim passthrough — route through
	// normal chat->Responses translation and bill prepaid.
	if subscriptionRoutingDisabledForRequest(ctx) {
		return false
	}
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

// DefaultPlannerCorrectedEconomics ships OFF. The corrected cost model changes
// routing, so it is armed per environment (staging first) rather than by
// merging. See docs/CONFIGURATION.md for the measured effect.
const DefaultPlannerCorrectedEconomics = false

func NewService(r router.Router, providerMap map[string]providers.Client, emitter TelemetryEmitter, embedOnlyUserMessage bool, semanticCache *cache.Cache, pinStore sessionpin.Store, hardPinExplore bool, hardPinProvider, hardPinModel string, telemetry TelemetryRepository) *Service {
	return &Service{
		router:                       r,
		providers:                    providerMap,
		translationCompatibilityMode: TranslationCompatibilityShadow,
		emitter:                      emitter,
		embedOnlyUserMessage:         embedOnlyUserMessage,
		semanticCache:                semanticCache,
		pinStore:                     pinStore,
		noProgress:                   newNoProgressTracker(),
		searchUse:                    newSearchUseTracker(),
		searchRequirementDecayTurns:  DefaultSearchRequirementDecayTurns,
		compaction:                   newCompactionTracker(),
		prefixTrimFreeSwitch:         true,
		spiralTracker:                newSpiralTracker(),
		spiralShadowEnabled:          true,
		turnSignalCaptureEnabled:     true,
		struggleTracker:              newStruggleTracker(),
		struggleShadowEnabled:        true,
		textRepetitionBreakEnabled:   true,
		ccOrchToolsCrossVendor:       true,
		hardPinExplore:               hardPinExplore,
		hardPinProvider:              hardPinProvider,
		hardPinModel:                 hardPinModel,
		telemetry:                    telemetry,
		planner: planner.EVConfig{
			ThresholdUSD:           DefaultPlannerThresholdUSD,
			ExpectedRemainingTurns: DefaultPlannerExpectedRemainingTurns,
			TierUpgradeEnabled:     DefaultPlannerTierUpgradeEnabled,
		},
		hmmUpgradeConfidenceThreshold: defaultHMMUpgradeConfidenceThreshold,
		authoritativeUpgradeGate:      true,
		authorityCacheShadow:          true,
		plannerEnabled:                true,
		scoreToolResultTurns:          true,
		loopEscalationEnabled:         true,
		cyberRefusalRepin:             true,
		anthropicServerSideFallback:   true,
		siblingFailover:               true,
		openAIResponsesBroad:          true,
		cyberRefusalFallbackModel:     "claude-sonnet-5",
	}
}

// WithTranslationCompatibilityMode injects the startup-validated rollout
// setting. Invalid values are ignored here because configuration validation is
// owned by the composition root.
func (s *Service) WithTranslationCompatibilityMode(mode TranslationCompatibilityMode) *Service {
	switch mode {
	case TranslationCompatibilityOff, TranslationCompatibilityShadow, TranslationCompatibilityEnforce:
		s.translationCompatibilityMode = mode
	}
	return s
}

// WithScopedSearchRequirement gates the citations/search native requirement on actual (current
// or recent) web-search tool use, not mere tool advertisement (ROUTER_SCOPED_SEARCH_REQUIREMENT).
// Non-positive decayTurns keeps the default.
func (s *Service) WithScopedSearchRequirement(enabled bool, decayTurns int) *Service {
	s.scopedSearchRequirement = enabled
	if decayTurns > 0 {
		s.searchRequirementDecayTurns = decayTurns
	}
	return s
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
	s.planner.CorrectedEconomics = cfg.CorrectedEconomics
	return s
}

// WithPlannerEnabled is the kill switch. When false, the orchestrator
// preserves first-decision-wins behavior.
func (s *Service) WithPlannerEnabled(enabled bool) *Service {
	s.plannerEnabled = enabled
	return s
}

// WithScoreToolResultTurns sets ROUTER_SCORE_TOOL_RESULT_TURNS; see scoreToolResultTurns.
func (s *Service) WithScoreToolResultTurns(enabled bool) *Service {
	s.scoreToolResultTurns = enabled
	return s
}

// WithCyberRefusalRepin is the kill switch for the safety-refusal re-pin backstop
// (ROUTER_CYBER_REFUSAL_REPIN); see cyberRefusalRepin.
func (s *Service) WithCyberRefusalRepin(enabled bool) *Service {
	s.cyberRefusalRepin = enabled
	return s
}

// WithAnthropicServerSideFallback is the kill switch for Anthropic's
// server-side fallback beta (ROUTER_ANTHROPIC_SERVER_SIDE_FALLBACK); see
// anthropicServerSideFallback.
func (s *Service) WithAnthropicServerSideFallback(enabled bool) *Service {
	s.anthropicServerSideFallback = enabled
	return s
}

// WithSiblingFailover is the kill switch for same-cluster model failover
// (ROUTER_SIBLING_FAILOVER); see siblingFailover.
func (s *Service) WithSiblingFailover(enabled bool) *Service {
	s.siblingFailover = enabled
	return s
}

// WithOpenAIResponsesBroad sets the rollout flag for direct-OpenAI Responses
// routing (ROUTER_OPENAI_RESPONSES_BROAD).
func (s *Service) WithOpenAIResponsesBroad(enabled bool) *Service {
	s.openAIResponsesBroad = enabled
	return s
}

// WithAllowedModelsHeader sets the deployment default for honoring the
// x-weave-allowed-models header (ROUTER_ALLOWED_MODELS_HEADER).
func (s *Service) WithAllowedModelsHeader(enabled bool) *Service {
	s.allowedModelsHeader = enabled
	return s
}

// WithSSEKeepalive sets the client-facing silence budget before a `ping` is
// injected into a committed Anthropic stream. Non-positive disables it.
func (s *Service) WithSSEKeepalive(interval time.Duration) *Service {
	s.sseKeepalive = interval
	return s
}

// WithCyberRefusalFallbackModel sets the re-pin target used when the session pin
// carries no runner-up (ROUTER_CYBER_REFUSAL_FALLBACK_MODEL). Empty is ignored.
func (s *Service) WithCyberRefusalFallbackModel(model string) *Service {
	if strings.TrimSpace(model) != "" {
		s.cyberRefusalFallbackModel = strings.TrimSpace(model)
	}
	return s
}

// WithHMMUpgradeConfidenceThreshold sets the minimum HMM sidecar ChosenScore
// for a fresh upgrade to beat an EV stay on the cheaper pinned model.
// Out-of-range values ([0,1] only) are ignored.
func (s *Service) WithHMMUpgradeConfidenceThreshold(v float64) *Service {
	if v < 0 || v > 1 {
		return s
	}
	s.hmmUpgradeConfidenceThreshold = v
	return s
}

// WithHMMSameTierPin is the kill switch (ROUTER_HMM_SAME_TIER_PIN) for
// suppressing an EV-positive HMM switch between two same-tier models.
func (s *Service) WithHMMSameTierPin(enabled bool) *Service {
	s.hmmSameTierPin = enabled
	return s
}

// WithAuthoritativeUpgradeGate is the kill switch (ROUTER_AUTHORITATIVE_UPGRADE_GATE)
// for applying the upgrade-confidence threshold to authoritative-per-turn
// decisions. On by default; disabling restores verbatim policy selection.
func (s *Service) WithAuthoritativeUpgradeGate(enabled bool) *Service {
	s.authoritativeUpgradeGate = enabled
	return s
}

// WithAuthorityCacheShadow sets the kill switch (ROUTER_AUTHORITY_CACHE_SHADOW)
// for recording the cache gate's counterfactual verdict on authoritative turns.
func (s *Service) WithAuthorityCacheShadow(enabled bool) *Service {
	s.authorityCacheShadow = enabled
	return s
}

// WithPolicyDeadlineFallback sets the kill switch (ROUTER_POLICY_DEADLINE_FALLBACK) for degrading a policy sidecar deadline to the session pin instead of a 503.
func (s *Service) WithPolicyDeadlineFallback(enabled bool) *Service {
	s.policyDeadlineFallback = enabled
	return s
}

// WithPolicyDeadlineDefaultModel sets ROUTER_POLICY_DEADLINE_DEFAULT_MODEL: the tier-3 static fallback
// on a deadline miss with no pin yet. Empty preserves fail-closed (503) for pinless sessions.
func (s *Service) WithPolicyDeadlineDefaultModel(model string) *Service {
	s.policyDeadlineDefaultModel = model
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

// WithCCOrchestrationToolsCrossVendor preserves Claude Code orchestration
// tools (Task*, Workflow, Skill, plan-mode) on cross-vendor emit. False strips all.
func (s *Service) WithCCOrchestrationToolsCrossVendor(enabled bool) *Service {
	s.ccOrchToolsCrossVendor = enabled
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

// WithTurnSignalCapture sets the per-turn signal persistence kill switch.
// It cannot override installation privacy settings.
func (s *Service) WithTurnSignalCapture(enabled bool) *Service {
	s.turnSignalCaptureEnabled = enabled
	return s
}

// WithTextRepetitionBreak sets the enforcing text-repetition detector kill
// switch (default on). enabled=false skips the per-turn scan entirely. See
// text_repetition.go.
func (s *Service) WithTextRepetitionBreak(enabled bool) *Service {
	s.textRepetitionBreakEnabled = enabled
	return s
}

// WithSpiralShadowStore wires the durable sink for shadow spiral events
// (router.spiral_shadow_events). Nil degrades to log-only fires with
// replica-local de-duplication.
func (s *Service) WithSpiralShadowStore(store SpiralShadowStore) *Service {
	s.spiralShadowStore = store
	return s
}

// WithStruggleShadowConfig sets the shadow-mode struggle detector kill switch.
// enabled=false skips the check entirely. Shadow mode takes no routing action
// either way; the switch exists to shed the per-turn check if it misbehaves.
func (s *Service) WithStruggleShadowConfig(enabled bool) *Service {
	s.struggleShadowEnabled = enabled
	return s
}

// WithStruggleShadowStore wires the durable sink for shadow struggle events
// (router.struggle_shadow_events). Nil degrades to log-only fires with
// replica-local de-duplication.
func (s *Service) WithStruggleShadowStore(store StruggleShadowStore) *Service {
	s.struggleShadowStore = store
	return s
}

// WithStruggleEscalationConfig sets the kill switch and holdout percentage
// (0–100); enabled=false makes the handler a no-op regardless of holdoutPct.
func (s *Service) WithStruggleEscalationConfig(enabled bool, holdoutPct int) *Service {
	s.struggleEscalationEnabled = enabled
	if holdoutPct < 0 {
		holdoutPct = 0
	}
	if holdoutPct > 100 {
		holdoutPct = 100
	}
	s.struggleEscalationHoldoutPct = holdoutPct
	return s
}

// WithStruggleEvidenceArming sets whether behavioral spiral evidence may arm a
// struggle escalation before the turn/wall thresholds (default off).
func (s *Service) WithStruggleEvidenceArming(enabled bool) *Service {
	s.struggleEvidenceArming = enabled
	return s
}

// WithStruggleEscalationStore wires the durable sink for struggle escalation events
// (router.struggle_escalation_events); nil disables persistence, holdout, and budget.
func (s *Service) WithStruggleEscalationStore(store StruggleEscalationStore) *Service {
	s.struggleEscalationStore = store
	return s
}

// WithStruggleEscalationRoster wires the sideways-target picker. Nil disables
// sideways target selection (events are still recorded as no_sideways_target).
func (s *Service) WithStruggleEscalationRoster(roster StruggleEscalationRoster) *Service {
	s.struggleEscalationRoster = roster
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
//   - grok-4.x: pinned "low" — omitting effort falls to xAI's non-disableable
//     "high" default, a ~15 s fixed TTFT stall on every pinned turn.
//   - everything else: "" — left to its own path.
func forcedReasoningEffort(model string, escalate bool) string {
	// Unconditional: omitting effort falls to xAI's non-disableable "high" default
	// (~15 s TTFT stall) — a defect to correct, not an escalation to tune.
	if strings.HasPrefix(model, "grok-") {
		return "low"
	}
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

// WithWebSearchExecutor installs the backend that runs Anthropic's native
// web-search server tool when the routed upstream rejects it. nil leaves
// those turns on normal routing.
func (s *Service) WithWebSearchExecutor(ex websearch.Executor) *Service {
	s.webSearch = ex
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

// WithCompactionModel overrides the Sonnet-class default summarizer for the
// compaction cascade and Claude Code's native compaction turn
// (ROUTER_COMPACTION_MODEL). A model with no Anthropic binding is rejected
// at boot by the caller; empty keeps DefaultCompactionModel.
func (s *Service) WithCompactionModel(model string) *Service {
	s.compactionModel = model
	return s
}

// WithCompactionHardPin enables pinning Claude Code's native compaction turn
// to the session's Anthropic model (or the compaction model) instead of the
// generic utility hard-pin. The composition root leaves it off when the
// operator set an explicit ROUTER_HARD_PIN_MODEL.
func (s *Service) WithCompactionHardPin(enabled bool) *Service {
	s.compactionHardPinEnabled = enabled
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

// HardPinRequest is the per-request context the hard-pin tier selects against;
// gateway-exclusive installations can only reach models aliased onto the enrolled gateway.
type HardPinRequest struct {
	EnabledProviders map[string]struct{}
	ExcludedModels   map[string]struct{}
	CustomBindings   map[string][]string
	GatewayProviders map[string]struct{}
}

// HardPinResolver picks the hard-pin tier's provider/model for one request.
// ok=false signals no eligible provider.
type HardPinResolver func(HardPinRequest) (provider, model string, ok bool)

// WithHardPinResolver installs a per-request hard-pin resolver. nil
// preserves the boot-time hardPin{Provider,Model} for every request.
// ok=false signals no eligible provider, surfacing ErrClusterUnavailable.
func (s *Service) WithHardPinResolver(resolver HardPinResolver) *Service {
	s.hardPinResolver = resolver
	return s
}

// WithDefaultBaselineModel installs the cost-comparison fallback for when
// the inbound RequestedModel has no pricing entry. Empty disables.
func (s *Service) WithDefaultBaselineModel(model string) *Service {
	s.defaultBaselineModel = model
	return s
}

// WithSubAgentOverride pins SubAgentDispatch turns to a distinct provider/model,
// leaving MainLoop/ToolResult routing untouched. Both must be non-empty;
// either empty is a no-op (falls back to hardPinExplore behavior).
func (s *Service) WithSubAgentOverride(provider, model string) *Service {
	s.subAgentProvider = provider
	s.subAgentModel = model
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

// WithGlobalAutomaticExclusions wires the deployment-wide soft exclusion list
// the Weave control plane maintains. A nil store leaves automatic routing
// unrestricted.
func (s *Service) WithGlobalAutomaticExclusions(store GlobalAutomaticExclusionStore) *Service {
	if store == nil {
		s.globalAutomaticExclusions = nil
		return s
	}
	s.globalAutomaticExclusions = newGlobalAutomaticExclusionCache(store)
	return s
}

// RoutableModels returns a copy of the set of models this deployment can
// route, so the admin guard and request-time desugaring share one definition
// and cannot drift.
func (s *Service) RoutableModels() map[string]struct{} {
	// A nil Service arrives as a typed-nil interface from server.Register;
	// return nil rather than panicking.
	if s == nil {
		return nil
	}
	universe := s.routableUniverse()
	out := make(map[string]struct{}, len(universe))
	for m := range universe {
		out[m] = struct{}{}
	}
	return out
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

// gatewayResponsesKey identifies the endpoint whose Responses support is being
// memoized: the BYOK base URL, or the provider name for a deployment-keyed
// gateway (one endpoint per process). Empty for direct vendors, which are not
// memoized.
func gatewayResponsesKey(ctx context.Context, provider string) string {
	if !providers.IsGateway(provider) {
		return ""
	}
	return EffectiveBaseURL(ctx, provider)
}

// gatewayLacksResponses reports whether that endpoint already told us it serves
// no Responses API.
func (s *Service) gatewayLacksResponses(key string) bool {
	if key == "" {
		return false
	}
	_, ok := s.noResponsesGateways.Load(key)
	return ok
}

// rememberGatewayLacksResponses records a gateway's rejection of the Responses
// API so later tool turns go straight to chat/completions.
func (s *Service) rememberGatewayLacksResponses(key string) {
	if key == "" {
		return
	}
	s.noResponsesGateways.Store(key, struct{}{})
}

// gatewayModelKey returns "endpoint|model" for gateway providers (BYOK base
// URL, or provider name when deployment-keyed); empty for direct vendors.
func gatewayModelKey(endpoint, provider, model string) string {
	if !providers.IsGateway(provider) || model == "" {
		return ""
	}
	endpoint = strings.TrimRight(endpoint, "/")
	if endpoint == "" {
		endpoint = provider
	}
	return endpoint + "|" + model
}

// gatewayLacksModel reports whether that endpoint already answered
// model-not-found for the model.
func (s *Service) gatewayLacksModel(key string) bool {
	if key == "" {
		return false
	}
	_, ok := s.unservedGatewayModels.Load(key)
	return ok
}

// rememberGatewayLacksModel records a gateway's model-not-found answer so
// later turns resolve around the alias instead of paying the 404 again.
func (s *Service) rememberGatewayLacksModel(ctx context.Context, provider, model string) {
	key := gatewayModelKey(EffectiveBaseURL(ctx, ""), provider, model)
	if key == "" {
		return
	}
	s.unservedGatewayModels.Store(key, struct{}{})
}

// gatewayRejectsPromptCacheKey reports whether that endpoint already told us
// it refuses bodies carrying prompt_cache_key.
func (s *Service) gatewayRejectsPromptCacheKey(key string) bool {
	if key == "" {
		return false
	}
	_, ok := s.noPromptCacheKeyGateways.Load(key)
	return ok
}

// rememberGatewayRejectsPromptCacheKey records a gateway's unknown-field
// rejection of prompt_cache_key so later turns go out without the hint.
func (s *Service) rememberGatewayRejectsPromptCacheKey(key string) {
	if key == "" {
		return
	}
	s.noPromptCacheKeyGateways.Store(key, struct{}{})
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

// WithManagedSubscriptions enables server-side owner/provider account pools.
func (s *Service) WithManagedSubscriptions(pool subscriptions.Leaser) *Service {
	s.managedSubscriptions = pool
	return s
}

// WithPlanAwareSubscriptionRouting enables per-user model eligibility based on
// the aggregate state of linked Claude and Codex subscription plans.
func (s *Service) WithPlanAwareSubscriptionRouting(enabled bool) *Service {
	s.planAwareSubscriptionRouting = enabled
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

// ErrSessionCostNotFound is returned for unknown, foreign, or not-yet-committed
// sessions — deliberately indistinguishable so callers cannot probe foreign sessions.
var ErrSessionCostNotFound = errors.New("no committed router telemetry for session")

// SessionCost returns the committed router cost of one client session, scoped
// to the calling installation.
func (s *Service) SessionCost(ctx context.Context, installationID, sessionID string) (SessionCost, error) {
	if s.telemetry == nil {
		return SessionCost{}, ErrSessionCostNotFound
	}
	if sessionID == "" || len(sessionID) > MaxClientIdentifierLen {
		return SessionCost{}, ErrSessionCostNotFound
	}
	return s.telemetry.GetSessionCost(ctx, installationID, sessionID)
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

// MetricsModelBreakdown returns per-bucket totals grouped by decision model
// for the dashboard's per-model usage and spend charts.
func (s *Service) MetricsModelBreakdown(ctx context.Context, installationID string, from, to time.Time, granularity string) ([]TelemetryModelBucket, error) {
	if s.telemetry == nil {
		return nil, nil
	}
	return s.telemetry.GetTelemetryModelBreakdown(ctx, installationID, from, to, granularity)
}

// MetricsModelBreakdownAll returns per-model buckets across every installation. Admin-only.
func (s *Service) MetricsModelBreakdownAll(ctx context.Context, from, to time.Time, granularity string) ([]TelemetryModelBucket, error) {
	if s.telemetry == nil {
		return nil, nil
	}
	return s.telemetry.GetTelemetryModelBreakdownAll(ctx, from, to, granularity)
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
	"Request-Id":              {},
	"X-Request-Id":            {},
	"X-Router-Decision":       {},
	"X-Router-Provider":       {},
	"X-Router-Model":          {},
	"X-Router-Context-Window": {},
	"X-Router-Cache":          {},
	"X-Router-Feedback-Url":   {},
	http.CanonicalHeaderKey(HeaderRouterCostUSD):             {},
	http.CanonicalHeaderKey(HeaderRouterCostInputUSD):        {},
	http.CanonicalHeaderKey(HeaderRouterCostOutputUSD):       {},
	http.CanonicalHeaderKey(HeaderRouterCacheReadTokens):     {},
	http.CanonicalHeaderKey(HeaderRouterCacheCreationTokens): {},
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
	w.Header().Set(HeaderRouterContextWindow, strconv.Itoa(contextWindowForRequest(decision.Model, decision.Provider)))
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
// ctx. Precedence is header override > per-organization override > service
// default. Exposed so handlers outside this package (e.g. /v1/route) can use
// the same resolution as ProxyMessages and stay in sync with customer-visible
// routing behavior.
func (s *Service) ResolveEmbedOnlyUserMessage(ctx context.Context) bool {
	flag := flags.BoolOr(ctx, flags.KeyEmbedOnlyUserMessage, s.embedOnlyUserMessage)
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

// WithPolicyStrategy registers one non-default router and its lifecycle
// capabilities. Outcome and feedback reporters are discovered from the router.
func (s *Service) WithPolicyStrategy(spec policy.StrategySpec) *Service {
	if spec.Strategy == "" || spec.Strategy == router.StrategyCluster {
		return s
	}
	if s.strategies == nil {
		s.strategies = make(map[router.Strategy]registeredStrategy)
	}
	registered := registeredStrategy{
		router:       spec.Router,
		unavailable:  spec.Unavailable,
		capabilities: spec.Capabilities,
	}
	registered.outcomes, _ = spec.Router.(policy.OutcomeReporter)
	registered.feedback, _ = spec.Router.(policy.FeedbackReporter)
	s.strategies[spec.Strategy] = registered
	return s
}

// PolicyCapabilities returns a registered strategy's declared capabilities.
func (s *Service) PolicyCapabilities(strategy router.Strategy) (policy.Capabilities, bool) {
	registered, ok := s.strategies[strategy]
	if ok {
		if source, dynamic := registered.router.(policy.CapabilitySource); dynamic {
			return source.CurrentCapabilities(), true
		}
	}
	return registered.capabilities, ok
}

func (s *Service) authoritativePerTurnSelection(ctx context.Context) bool {
	if s == nil {
		return false
	}
	capabilities, ok := s.PolicyCapabilities(router.StrategyFromContext(ctx))
	return ok && capabilities.AuthoritativePerTurnSelection
}

func (s *Service) semanticCacheAllowed(ctx context.Context) bool {
	return !s.authoritativePerTurnSelection(ctx)
}

// PolicyStrategyAvailable reports whether a strategy has a live router;
// registration stays visible when a sidecar is absent so callers see the gap.
func (s *Service) PolicyStrategyAvailable(strategy router.Strategy) bool {
	if strategy == router.StrategyCluster {
		return s != nil && s.router != nil
	}
	registered, ok := s.strategies[strategy]
	return ok && registered.router != nil
}

// RegisteredStrategies returns every configured non-default strategy in
// deterministic order. Middleware uses this list instead of hardcoding IDs.
func (s *Service) RegisteredStrategies() []router.Strategy {
	if s == nil {
		return nil
	}
	strategies := make([]router.Strategy, 0, len(s.strategies))
	for strategy := range s.strategies {
		strategies = append(strategies, strategy)
	}
	sort.Slice(strategies, func(i, j int) bool { return strategies[i] < strategies[j] })
	return strategies
}

// WithRLRouter is retained for source compatibility. New wiring should call
// WithPolicyStrategy directly.
func (s *Service) WithRLRouter(r router.Router) *Service {
	return s.WithPolicyStrategy(policy.StrategySpec{
		Strategy:     router.StrategyRL,
		Router:       r,
		Unavailable:  rl.ErrPolicyUnavailable,
		Capabilities: policy.Capabilities{},
	})
}

// WithHMMRouter is retained for source compatibility. New wiring should call
// WithPolicyStrategy directly.
func (s *Service) WithHMMRouter(r router.Router) *Service {
	return s.WithPolicyStrategy(policy.StrategySpec{
		Strategy:    router.StrategyHMM,
		Router:      r,
		Unavailable: hmm.ErrHMMUnavailable,
		Capabilities: policy.Capabilities{
			SchemaVersion:            policy.SchemaVersionV1,
			ReportsOutcomes:          true,
			ReportsFeedback:          true,
			HonorsPreferredModels:    true,
			HonorsQualityPriceBias:   true,
			SupportsDebugRouteDetail: true,
			SupportsShadow:           true,
		},
	})
}

// WithBanditRouter is retained for source compatibility. New wiring should
// call WithPolicyStrategy directly.
func (s *Service) WithBanditRouter(r router.Router) *Service {
	return s.WithPolicyStrategy(policy.StrategySpec{
		Strategy:     router.StrategyBandit,
		Router:       r,
		Unavailable:  bandit.ErrBanditUnavailable,
		Capabilities: policy.Capabilities{},
	})
}

// routeFor picks the active router for the request's strategy. The default
// (and the cluster strategy) is the cluster scorer; the rl strategy uses the
// RL policy router when wired, and otherwise fails closed with
// ErrPolicyUnavailable (→ HTTP 503) — never a silent fallback that would mask
// which strategy actually served the turn.
func (s *Service) routeFor(ctx context.Context, req router.Request) (router.Decision, error) {
	var err error
	req, err = s.applyTranslationPlan(ctx, req)
	if err != nil {
		return router.Decision{}, err
	}
	req = s.withPolicyRequestContext(ctx, req)
	strategy := router.StrategyFromContext(ctx)
	return s.routeWithStrategy(ctx, strategy, req)
}

func (s *Service) routeWithStrategy(ctx context.Context, strategy router.Strategy, req router.Request) (router.Decision, error) {
	if strategy == router.StrategyCluster {
		if s.router == nil {
			return router.Decision{}, fmt.Errorf("strategy %q requested but no router configured: %w", strategy, router.ErrStrategyUnavailable)
		}
		return s.router.Route(ctx, req)
	}
	registered, ok := s.strategies[strategy]
	if !ok || registered.router == nil {
		unavailable := defaultStrategyUnavailable(strategy)
		if ok && registered.unavailable != nil {
			unavailable = registered.unavailable
		}
		return router.Decision{}, fmt.Errorf("strategy %q requested but no router configured: %w", strategy, unavailable)
	}
	return registered.router.Route(ctx, req)
}

func (s *Service) withPolicyRequestContext(ctx context.Context, req router.Request) router.Request {
	// Set here rather than at each ingress so every routed and previewed turn
	// sees the same deployment-wide soft exclusions, including callers that
	// bypass the turn loop.
	req.AutomaticExcludedModels = s.globalAutomaticExcludedModels(ctx)
	req.OrganizationID, _ = ctx.Value(ExternalIDContextKey{}).(string)
	req.InstallationID = ""
	if installationID := installationIDFromContext(ctx); installationID != uuid.Nil {
		req.InstallationID = installationID.String()
	}
	clientIdentity := ClientIdentityFrom(ctx)
	req.ClientApp = clientIdentity.ClientApp
	req.RolloutID = policyRolloutIDFromContext(ctx)
	req.CaptureMode = s.effectiveCaptureMode(ctx).String()
	req.TrainingAllowed, _ = ctx.Value(PolicyTrainingAllowedContextKey{}).(bool)
	req.DebugEnabled, _ = ctx.Value(PolicyDebugEnabledContextKey{}).(bool)
	req.RoutingIntent, _ = ctx.Value(PolicyRoutingIntentContextKey{}).(string)
	return req
}

func policyRolloutIDFromContext(ctx context.Context) string {
	if rolloutID, ok := ctx.Value(PolicyRolloutIDContextKey{}).(string); ok && rolloutID != "" {
		return rolloutID
	}
	return ClientIdentityFrom(ctx).RolloutID
}

func defaultStrategyUnavailable(strategy router.Strategy) error {
	switch strategy {
	case router.StrategyRL:
		return rl.ErrPolicyUnavailable
	case router.StrategyHMM, router.StrategyHMMEmbedding, router.StrategyHMMBeta:
		return hmm.ErrHMMUnavailable
	case router.StrategyBandit:
		return bandit.ErrBanditUnavailable
	default:
		return router.ErrStrategyUnavailable
	}
}

// Route exposes the underlying router for callers that need a decision
// without dispatching (e.g. admin endpoints). Honors the per-request strategy.
func (s *Service) Route(ctx context.Context, req router.Request) (router.Decision, error) {
	routeCtx, span := startRoutingSpan(ctx, req)
	decision, err := s.routeFor(routeCtx, req)
	finishRoutingSpan(span, decision, err)
	return decision, err
}

// RouteAnthropicRequest parses a raw Anthropic-Messages body and returns the
// routing decision without dispatching (e.g. the /v1/route dry-run endpoint).
// Owns translate.ParseAnthropic + RoutingFeatures extraction internally so
// callers in internal/api/* never import internal/translate directly,
// matching ProxyMessages.
func (s *Service) RouteAnthropicRequest(ctx context.Context, body []byte, headers http.Header) (decision router.Decision, err error) {
	ctx, req, err := s.anthropicRoutingRequest(ctx, body, headers, "anthropic_route")
	if err != nil {
		return decision, err
	}
	return s.Route(ctx, req)
}

// PassthroughToProvider forwards a non-routing request to the default
// (Anthropic) provider for metadata endpoints (count_tokens, models).
// count_tokens is answered locally with an estimate when no Anthropic
// credential is reachable (gateway-only deployment) or when the upstream
// attempt fails transiently; see countTokens.
func (s *Service) PassthroughToProvider(ctx context.Context, body []byte, w http.ResponseWriter, r *http.Request) error {
	if isCountTokensRequest(r) {
		return s.countTokens(ctx, body, w, r)
	}
	return s.PassthroughToNamedProvider(ctx, providers.ProviderAnthropic, body, w, r)
}

// anthropicCredentialReachable reports whether any Anthropic credential
// (BYOK, deployment key, subscription, or inbound client key) is reachable.
func (s *Service) anthropicCredentialReachable(ctx context.Context, headers http.Header) bool {
	if s.anthropicFallbackKeyAvailable(ctx) {
		return true
	}
	// nil deploymentKeyedProviders means every registered provider is
	// deployment-keyed (legacy behavior, mirrors enabledProvidersForRequest).
	if s.deploymentKeyedProviders == nil && !s.byokOnly {
		if _, registered := s.providers[providers.ProviderAnthropic]; registered {
			return true
		}
	}
	if anthropicSubscriptionFromContext(ctx) != "" {
		return true
	}
	return ExtractClientCredentials(providers.ProviderAnthropic, headers) != nil
}

// PassthroughToNamedProvider forwards a non-routing request to a specific
// provider. No model rewriting, no routing decision. Anthropic targets get
// the body scrubbed via envelope parsing; others receive it verbatim.
func (s *Service) PassthroughToNamedProvider(ctx context.Context, providerName string, body []byte, w http.ResponseWriter, r *http.Request) error {
	log := observability.FromContext(ctx)
	p, err := s.provider(providerName)
	if err != nil {
		return err
	}
	ctx = resolveAndInjectCredentials(ctx, providerName, "", r.Header)

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
		// Same reason as the variant tag above: a provider-qualified id is
		// valid at ingress and on the routing path, but the native Anthropic
		// API 404s on it.
		if bare, had, cerr := translate.StripProviderPrefixInBody(body, providerName); cerr == nil && had {
			body = bare
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
	log.Info("PassthroughToProvider complete", "provider", providerName, "proxy_ms", proxyMs, "proxy_err", proxyErr)
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
	setStreamCost func(router.Decision, bool),
) dispatchAttempt {
	return func(actx context.Context, d router.Decision, p providers.Client) error {
		setStreamCost(d, false)
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

// anthropicRefusalStopReason is Anthropic's stop_reason for a turn its safety
// classifiers decline (HTTP 200).
const anthropicRefusalStopReason = "refusal"

// refusalScanCap bounds how many response bytes a refusalObserver accumulates
// while scanning for a safety-refusal signal. A refusal surfaces in the opening
// events, so a small window catches it without buffering a whole response.
const refusalScanCap = 64 * 1024

// refusalScanOverlap is the trailing window each Write re-scans so a signal
// split across two chunks is still caught, without re-scanning (and re-lowering)
// the whole accumulated buffer every write. Must exceed the longest signal in
// detectRefusalSignal (`"stop_reason":"refusal"`, 23 bytes).
const refusalScanOverlap = 32

// refusalObserver tees the anthropic-native passthrough to inner unchanged
// while scanning a bounded prefix for a safety-refusal signal. Observe-only
// because the native path has no translator Summary to read from.
// See detectRefusalSignal.
type refusalObserver struct {
	inner http.ResponseWriter
	buf   []byte
	// refused reports a safety refusal in the observed prefix; category is
	// Anthropic's refusal category (cyber, reasoning_extraction, ...) when the
	// response carried one, else empty.
	refused  bool
	category string
}

func newRefusalObserver(inner http.ResponseWriter) *refusalObserver {
	return &refusalObserver{inner: inner}
}

func (o *refusalObserver) Header() http.Header { return o.inner.Header() }

func (o *refusalObserver) WriteHeader(status int) { o.inner.WriteHeader(status) }

func (o *refusalObserver) Write(p []byte) (int, error) {
	// Accumulate up to refusalScanCap so signals split across SSE chunks are caught.
	if !o.refused && len(o.buf) < refusalScanCap {
		prevLen := len(o.buf)
		room := refusalScanCap - len(o.buf)
		if room > len(p) {
			room = len(p)
		}
		o.buf = append(o.buf, p[:room]...)
		// Scan only the newly appended bytes plus a short overlap; re-scanning the
		// whole accumulated buffer every write is O(n^2) (and ToLower re-allocates
		// it each time). A signal split across two writes is still caught.
		scanFrom := prevLen - refusalScanOverlap
		if scanFrom < 0 {
			scanFrom = 0
		}
		if detectRefusalSignal(o.buf[scanFrom:]) {
			o.refused = true
			// Scan the whole prefix for the category: it may sit before the
			// signal that tripped detection.
			o.category = refusalCategory(o.buf)
		}
	}
	return o.inner.Write(p)
}

func (o *refusalObserver) Flush() {
	if f, ok := o.inner.(http.Flusher); ok {
		f.Flush()
	}
}

// detectRefusalSignal returns true on any Anthropic safety-refusal signal.
// Over-detecting is safe — re-pin to sonnet is always valid.
func detectRefusalSignal(b []byte) bool {
	if bytes.Contains(b, []byte(`"stop_reason":"refusal"`)) ||
		bytes.Contains(b, []byte(`"type":"refusal"`)) ||
		bytes.Contains(b, []byte("api_refusal_category")) {
		return true
	}
	return bytes.Contains(bytes.ToLower(b), []byte("safeguards flagged"))
}

// refusalCategoryMaxLen bounds a plausible category token, so a malformed or
// adversarial response can't turn an unterminated field into a log-field blob.
const refusalCategoryMaxLen = 64

// refusalCategoryKeys are the JSON fields Anthropic carries the refusal
// category in; the rendered client-facing text carries it as `[category]`
// after "Details: " instead.
var refusalCategoryKeys = [][]byte{
	[]byte(`"api_refusal_category":"`),
	[]byte(`"refusal_category":"`),
	[]byte(`"category":"`),
}

// refusalCategory extracts Anthropic's refusal category (cyber,
// reasoning_extraction, ...) from an observed response prefix. Empty when the
// response carries none — the category is telemetry, never a control signal.
func refusalCategory(b []byte) string {
	for _, key := range refusalCategoryKeys {
		if v, ok := delimitedValue(b, key, '"'); ok {
			return v
		}
	}
	if v, ok := delimitedValue(b, []byte("Details: ["), ']'); ok {
		return v
	}
	return ""
}

// delimitedValue returns the bytes between the first occurrence of prefix and
// the next end byte, when non-empty and within refusalCategoryMaxLen.
func delimitedValue(b, prefix []byte, end byte) (string, bool) {
	i := bytes.Index(b, prefix)
	if i < 0 {
		return "", false
	}
	rest := b[i+len(prefix):]
	j := bytes.IndexByte(rest, end)
	if j <= 0 || j > refusalCategoryMaxLen {
		return "", false
	}
	return string(rest[:j]), true
}

// maybeRepinOnRefusal re-pins the session off the refusing model post-turn
// so subsequent turns route to a non-refusing model.
func (s *Service) maybeRepinOnRefusal(ctx context.Context, obs *refusalObserver, sessionKey [sessionpin.SessionKeyLen]byte, role string, served router.Decision) {
	if obs == nil || !obs.refused || s.pinStore == nil {
		return
	}
	// Detection is unconditional so refusals stay measurable; the flag gates
	// only the re-pin action.
	if !s.ResolveCyberRefusalRepin(ctx) {
		return
	}
	installationID := installationIDFromContext(ctx)
	if installationID == uuid.Nil {
		return
	}
	// Hard-pinned turns (probe, compaction, title-gen) leave SessionKey zero and
	// skip normal pin read/write — never persist a pin under an empty key.
	if sessionKey == ([sessionpin.SessionKeyLen]byte{}) {
		return
	}
	// A /force-model pin is the user's explicit choice; a refusal must not silently
	// overwrite it. Prefix check covers ReasonUserForceModel and its tier_clamp suffix.
	if strings.HasPrefix(served.Reason, translate.ReasonUserForceModel) {
		return
	}
	log := observability.FromContext(ctx)
	// Prefer the scorer's runner-up (PairedModel); use context.Background() because
	// the request ctx may already be canceled when the response has been written.
	fbModel, fbProvider := s.ResolveCyberRefusalFallbackModel(ctx), ""
	if existing, found, err := s.pinStore.Get(context.Background(), sessionKey, role); err == nil && found && pinMatchesEffectiveStrategy(ctx, existing) && existing.PairedModel != "" {
		fbModel, fbProvider = existing.PairedModel, existing.PairedProvider
	}
	if fbProvider == "" {
		if m, ok := catalog.ByID(fbModel); ok && len(m.Providers) > 0 {
			fbProvider = m.Providers[0].Provider
		}
	}
	if fbModel == "" || fbProvider == "" || fbModel == served.Model {
		log.Warn("safety refusal observed but no distinct fallback model available; not re-pinning",
			"from_model", served.Model, "fallback_model", fbModel, "refusal_category", obs.category)
		return
	}
	pin := sessionpin.Pin{
		SessionKey:     sessionKey,
		Role:           role,
		InstallationID: installationID,
		Provider:       fbProvider,
		Model:          fbModel,
		Reason:         "cyber-refusal-repin",
		Strategy:       router.StrategyFromContext(ctx),
		TurnCount:      1,
		PinnedUntil:    pinExpiry("cyber-refusal-repin"),
	}
	// context.Background(): ctx may already be canceled here (response written,
	// client disconnected); a canceled ctx would drop the re-pin write.
	if err := s.pinStore.Upsert(context.Background(), pin); err != nil {
		log.Error("cyber-refusal re-pin: pin upsert failed", "err", err, "from_model", served.Model, "to_model", fbModel)
		return
	}
	log.Info("safety refusal — re-pinned session off refusing model",
		"session_key", shortSessionKey(sessionKey),
		"refusal_category", obs.category,
		"from_model", served.Model,
		"to_model", fbModel,
		"to_provider", fbProvider)
}

// anthropicPingFrame keeps a client-facing stream byte-alive during long
// upstream reasoning phases that produce no translatable frames.
var anthropicPingFrame = []byte(sseEvent("ping", `{"type":"ping"}`))

func (s *Service) ProxyMessages(ctx context.Context, body []byte, w http.ResponseWriter, r *http.Request) error {
	if managedSubscriptionEnrollmentUnavailable(ctx) {
		return ErrSubscriptionPoolUnavailable
	}
	ctx, err := s.checkUserMonthlySpendLimit(ctx, r.Header, r.URL.Path)
	if err != nil {
		return err
	}
	ctx = s.withUsageObserver(ctx, r.Header, routePathMessages)
	log := observability.FromContext(ctx)
	requestStart := time.Now()
	requestID := requestIDFor(ctx)
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
	// cleanup, matching the OpenAI chat path. The echo check must read the body
	// before the strip erases its evidence.
	footerEchoedSinceHumanTurn := translate.FeedbackFooterSinceLastHumanTurn(body)
	if echoed, _ := ctx.Value(responsesFooterEchoedContextKey{}).(bool); echoed {
		footerEchoedSinceHumanTurn = true
	}
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
	var responseBuffer *responseCostBuffer
	if !env.Stream() {
		responseBuffer = newResponseCostBuffer(w)
		w = responseBuffer
		defer func() {
			if flushErr := responseBuffer.FlushToClient(); flushErr != nil {
				log.Error("Failed to flush buffered response", "err", flushErr)
			}
		}()
	}

	apiKeyID, _ := ctx.Value(APIKeyIDContextKey{}).(string)
	externalID, _ := ctx.Value(ExternalIDContextKey{}).(string)
	installationID := installationIDFromContext(ctx)
	clientID := ClientIdentityFrom(ctx)
	agentShadowEval, agentShadowMode := AgentShadowEvalFromContext(ctx)
	bypassEval := hasEvalOverrideHeader(r) || agentShadowMode

	// Bind session_key/request_id/api_key_id/ingress onto a ctx-scoped logger
	// before stripping router-only history. The derived key is reused below to
	// avoid a second hash + divergent key if env.body mutates mid-flow.
	var sessionKey [sessionpin.SessionKeyLen]byte
	ctx, log, sessionKey = bindRequestLogger(ctx, env, apiKeyID, requestID, "anthropic_messages")
	if removed := env.StripRouterFeedbackArtifacts(); removed > 0 {
		log.Info("Stripped router-feedback artifacts from Anthropic history", "removed_messages", removed)
	}
	if removed := env.StripBetaArtifacts(); removed > 0 {
		ctx = withBetaArtifactHistory(ctx)
		log.Info("Stripped beta artifacts from Anthropic history", "removed_messages", removed)
	}

	embedFlag := s.ResolveEmbedOnlyUserMessage(ctx)
	feats := env.RoutingFeatures(embedFlag)
	promptText := feats.PromptText
	embedInput := "concatenated_stream"
	if embedFlag && feats.OnlyUserMessageText != "" {
		promptText = feats.OnlyUserMessageText
		embedInput = "only_user_message"
	}

	log.Info("ProxyMessages start",
		"requested_model", feats.Model,
		"stream", env.Stream(),
		"message_count", feats.MessageCount,
		"has_tools", feats.HasTools,
		"total_input_tokens", feats.Tokens,
		"prompt_preview", observability.Preview(promptText, 200),
	)

	// /beta toggle: handled server-side, never forwarded upstream, no post-command continuation.
	if !agentShadowMode {
		if cmd, hasCmd := env.ExtractBetaCommand(); hasCmd {
			log.Info("ProxyMessages beta command")
			return s.handleBetaCommand(ctx, w, env, cmd, installationID, sessionKey, feats.Tokens)
		}
		ctx, err = s.applySessionStrategy(ctx, installationID, sessionKey)
		if err != nil {
			return err
		}
		*r = *r.WithContext(ctx)
	}
	forceModelSessionKey := deriveForceModelSessionKeyForRequest(ctx, env, apiKeyID, sessionKey)

	// Handle /force-model and /unforce-model before routing (stripped from
	// env.body so the upstream never sees it). Session key is derived before
	// extraction: DeriveSessionKey can fall back to prompt text, and deriving
	// after the strip would mismatch subsequent turns with the unstripped message.
	agentForceModel := ""
	requestBodyChanged := false
	if !agentShadowMode && s.pinStore != nil {
		if cmd, hasCmd := env.ExtractForceModelCommand(); hasCmd {
			log.Info("ProxyMessages force-model command", "force_model_cmd", cmd)
			if cmd.FromToolResult {
				var err error
				agentForceModel, _, err = s.applyForceModelCommand(ctx, env, cmd, installationID, sessionKey, forceModelSessionKey)
				if err != nil {
					return err
				}
				requestBodyChanged = true
			} else {
				if err := s.handleForceModelCommand(ctx, w, env, cmd, installationID, sessionKey, forceModelSessionKey, feats.Tokens); err != nil {
					return err
				}
				s.grantPostCommandContinuation(ctx, installationID, sessionKey, roleForTier(catalog.TierFor(feats.Model)))
				return nil
			}
		}
	}
	if !agentShadowMode {
		if cmd, hasCmd := env.ExtractRouterFeedbackCommand(); hasCmd {
			log.Info("ProxyMessages router-feedback command")
			if err := s.handleRouterFeedbackCommand(ctx, w, env, cmd, installationID, sessionKey, feats.Tokens, !cmd.FromToolResult); err != nil {
				return err
			}
			if !cmd.FromToolResult {
				s.grantPostCommandContinuation(ctx, installationID, sessionKey, roleForTier(catalog.TierFor(feats.Model)))
				return nil
			}
			requestBodyChanged = true
		}
	}

	// Sanitize after command extraction: a skill can encode its command as a
	// plain user string after an assistant tool_use, and sanitizing first would
	// erase the provenance and leave a dangling tool_use that 400s on Together.
	if sanitized := env.SanitizeOrphanedToolCalls(); sanitized > 0 {
		log.Info("Sanitized orphaned tool calls before dispatch", "sanitized", sanitized)
		requestBodyChanged = true
	}
	if requestBodyChanged {
		feats = env.RoutingFeatures(embedFlag)
		promptText = feats.PromptText
		embedInput = "concatenated_stream"
		if embedFlag && feats.OnlyUserMessageText != "" {
			promptText = feats.OnlyUserMessageText
			embedInput = "only_user_message"
		}
	}

	// Honor the x-weave-force-model header (headless equivalent of /force-model).
	// Writes the user-forced pin and falls through to normal routing, which picks
	// the pin up and serves the requested model on this same turn.
	forceModel := agentForceModel
	forceCluster := ""
	if !agentShadowMode {
		var headerForceModel string
		var forceErr error
		ctx, headerForceModel, forceErr = s.applyForceModelHeader(ctx, r, installationID, forceModelSessionKey)
		if forceErr != nil {
			return forceErr
		}
		if headerForceModel != "" {
			forceModel = headerForceModel
		}
		forceCluster, forceErr = applyForceClusterHeader(ctx, r)
		if forceErr != nil {
			return forceErr
		}
	}

	// Wide cyclic re-read loop (same few files, no edits, dozens of turns) on a
	// cheap/mid model escalates the session to opus.
	if !agentShadowMode {
		if cyc, csig, ccount, cratio, cwin := detectCyclicToolCallLoop(env); cyc {
			loopRole := roleForTier(catalog.TierFor(feats.Model))
			s.handleLoopEscalation(ctx, csig, ccount, cratio, cwin, installationID, sessionKey, loopRole, feats.Model, forceModelSessionKey)
		}
	}

	// All consumers need the original client history. Track computation
	// separately because no threshold crossing is still a measured result.
	var inboundSpiralSignals spiralSignals
	var inboundSpiralReasons []spiralReason
	inboundSpiralComputed := s.ResolveSpiralShadowEnabled(ctx) ||
		s.ResolveStruggleEvidenceArming(ctx) ||
		s.ResolveTurnSignalCaptureEnabled(ctx)
	if inboundSpiralComputed {
		inboundSpiralSignals = computeSpiralSignals(env, feats.MessageCount)
		inboundSpiralReasons = spiralReasons(inboundSpiralSignals)
	}

	// Struggle escalation: writes a sticky pin before routing so runTurnLoop
	// dispatches the sideways target on the same turn.
	if !agentShadowMode && s.ResolveStruggleEscalationEnabled(ctx) && (turntype.DetectFromEnvelope(env, feats, "") == turntype.MainLoop || turntype.DetectFromEnvelope(env, feats, "") == turntype.ToolResult) {
		struggleRole := roleForTier(catalog.TierFor(feats.Model))
		s.handleStruggleEscalation(ctx, installationID, sessionKey, struggleRole, inboundSpiralReasons, forceModelSessionKey)
	}
	// Surface inbound tool_use / tool_result blocks the model is about to see.
	// Lets us audit whether a misbehaving turn was provoked by a malformed prior
	// tool_result or an out-of-shape tool spec, without dumping the whole body.
	logInboundRequestDiagnostics(log, env)

	// Anthropic packs sub-agent identity into metadata.user_id; the
	// x-weave-subagent-type header is for non-Anthropic ingress only.
	enabledProviders := s.enabledProvidersForRequest(ctx, providers.ProviderAnthropic, r.Header)

	// Subscription-only mode: restrict
	// routing to the providers the caller's own subscription can serve, so the
	// scorer can't pick a paid model. The post-routing guard below refuses if a
	// turn (e.g. a hard-pin or force-model) still didn't resolve onto the sub.
	if billing.SubscriptionOnlyFromContext(ctx) {
		enabledProviders = restrictToSubscriptionProviders(ctx, r.Header, enabledProviders)
	}

	// Anthropic's native web-search server tool, when no enabled provider runs
	// it. Served before routing: the scorer's only lever is picking a model,
	// and for a gateway-exclusive tenant every candidate rejects the tool.
	if s.serveNativeWebSearch(ctx, body, feats.Model, env.Stream(), feats.Tokens, enabledProviders, r.Header, w) {
		return nil
	}

	// Pre-filter models whose context window cannot fit this request.
	// FullTokenEstimate uses raw body bytes (÷5) to capture tool definitions,
	// tool calls, and tool results that feats.Tokens (text-only) misses.
	outputReserve := contextWindowOutputReserve
	if feats.MaxTokens > outputReserve {
		outputReserve = feats.MaxTokens
	}
	baseExcluded := s.excludeCodexOAuthOnlyModels(ctx, r.Header, enabledProviders, s.excludedModelsForRequest(ctx))

	// Snapshot inbound (client-sent) state BEFORE any env rewrite. The
	// compaction tracker, spiral scan, and tool-output telemetry must compare
	// what the client actually sent, not a router-shortened body — either the
	// proactive compaction just below or runTurnLoop's switch-handover rewrite.
	inboundToolCallCount := len(env.AssistantToolCallSignatures())
	inboundLastUser := env.LastUserMessage()

	// Proactive context-window compaction: shrink an over-long conversation to
	// fit the largest eligible model BEFORE routing, so a genuinely huge
	// session is compacted (à la Claude Code) instead of dead-ending in the
	// scorer with no eligible provider. Mutates env; feats is recomputed after.
	maxEligibleWindow := s.maxEligibleContextWindow(baseExcluded, enabledProviders, env.SignatureTokenSavings())
	var compRes compactionResult
	if !agentShadowMode {
		var compErr error
		compRes, compErr = s.maybeCompact(ctx, env, compactionInput{
			TurnType:       turntype.DetectFromEnvelope(env, feats, ""),
			OutputReserve:  outputReserve,
			MaxWindow:      maxEligibleWindow,
			RequestedModel: feats.Model,
			ClientApp:      ClientIdentityFrom(ctx).ClientApp,
			PreferredSummarizer: func() string {
				return s.compactionPreferredSummarizer(ctx, sessionKey, roleForTier(catalog.TierFor(feats.Model)))
			},
			Headers: r.Header,
		})
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
	}

	overflowEstimate := env.ContextOverflowTokenEstimate()
	excluded, ctxOverflowed := excludeContextOverflowModels(overflowEstimate, env.SignatureTokenSavings(), outputReserve, enabledProviders, baseExcluded, s.availableModels)
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
		RequestedModel:               feats.Model,
		ForceModel:                   forceModel,
		ForceCluster:                 forceCluster,
		EstimatedInputTokens:         feats.Tokens,
		HasTools:                     feats.HasTools,
		HasImages:                    feats.HasImages,
		TranslationRequirements:      env.TranslationRequirements(router.EndpointAnthropicMessages),
		ReasoningConfigurationSHA256: env.ReasoningConfigurationSHA256(),
		ToolConfigurationSHA256:      env.ToolConfigurationSHA256(),
		PromptText:                   promptText,
		ConversationMessages:         conversationMessagesForRouting(env),
		AvailableTools:               availableToolsForRouting(env),
		Tools:                        toolsForRouting(env),
		HistoryTruncated:             compRes.Applied,
		OrganizationID:               externalID,
		// Keep this tied to client-visible history so a later feedback command
		// can correlate with the route even if local compaction rewrites env.
		FeedbackKey:          hex.EncodeToString(sessionKey[:]),
		FeedbackRole:         roleForTier(catalog.TierFor(feats.Model)),
		ClientSessionID:      clientSessionIDForRequest(ctx, env),
		EnabledProviders:     enabledProviders,
		CustomBindings:       s.customBindingsForRequest(ctx),
		GatewayProviders:     s.gatewayProvidersForRequest(ctx),
		ExcludedModels:       excluded,
		AllowedModels:        allowedModelsForRequest(ctx),
		SafetyExcludedModels: s.safetyExcludedModels(env, outputReserve, enabledProviders),
		PreferredModels:      s.preferredModelsForRequest(ctx),
		RoutingKnobs:         routingKnobsForRequest(ctx),
		ClusterArmOverrides:  clusterArmOverridesForRequest(ctx),
	}
	if installationID != uuid.Nil {
		req.InstallationID = installationID.String()
	}
	var routeRes turnLoopResult
	var routeErr error
	routeCtx, routeSpan := startRoutingSpan(ctx, req)
	if agentShadowMode {
		routeRes, routeErr = s.runAgentShadowEvaluationRoute(routeCtx, env, feats, installationID, req, agentShadowEval)
	} else {
		routeRes, routeErr = s.runTurnLoop(routeCtx, env, feats, apiKeyID, installationID, "", r.Header, req)
	}
	finishRoutingSpan(routeSpan, routeRes.Decision, routeErr)
	if routeErr != nil {
		log.Error("Routing failed", "err", routeErr, "route_ms", time.Since(routeStart).Milliseconds(), "requested_model", feats.Model, "total_input_tokens", feats.Tokens)
		return routeErr
	}
	if len(routeRes.SessionDisabledProviders) > 0 {
		// resolveBindingsForDispatch reads excludedProvidersForRequest from ctx,
		// not req.EnabledProviders, so stash here for the failover walk too.
		ctx = context.WithValue(ctx, SessionDisabledProvidersContextKey{}, routeRes.SessionDisabledProviders)
	}

	// On a retryable 429 the bypass falls through to re-routing; rate-limit
	// headers prime the observer so the retry discounts Anthropic.
	if routeRes.UsageBypass && routeRes.Decision.Provider == providers.ProviderAnthropic {
		err := s.bypassToAnthropic(ctx, env, feats, routeRes.modelSwitched(), requestStart, requestID, externalID, routeRes.TurnType, r, w)
		if !errors.Is(err, errBypassRetryable) {
			if !agentShadowMode {
				s.firePolicyShadowForServingDecision(ctx, routeRes.Decision, req)
			}
			return err
		}

		// Subscription-only mode: the subscription just failed (e.g. 429
		// weekly-limit). Paid failover is disabled, so refuse rather than
		// reroute onto a paid model against an already-negative balance.
		if billing.SubscriptionOnlyFromContext(ctx) {
			log.Info("Subscription-only bypass hit retryable error; refusing instead of paid reroute",
				"request_id", requestID, "external_id", externalID)
			return ErrCreditsExhaustedSubscriptionUnavailable
		}

		// Bypass hit a pre-commit retryable error (e.g. Anthropic 429 weekly-limit
		// or transport error). Refresh the subsidy cost factor so the scorer
		// discounts Anthropic correctly on reroute.
		req.SubsidizedModelCostFactor = s.subsidyFactors(ctx, r.Header)

		// bypassToAnthropic returns before session pin/HMM history are loaded,
		// but modelSwitched() below needs them. Load the same switch history
		// the turn loop would have produced.
		if s.pinStore != nil {
			sessionKey := deriveSessionKeyForRequest(ctx, env, apiKeyID)
			role := roleForTier(catalog.TierFor(feats.Model))
			pin, _ := s.loadPin(ctx, sessionKey, role)
			hmmHistory := s.loadHMMHistory(ctx, sessionKey, role)
			forceHistory := s.loadForceModelHistory(ctx, sessionKey, role)
			routeRes.SessionKey = sessionKey
			routeRes.PriorServedModel, routeRes.SessionEverSwitched = switchHistoryFromPins(pin, hmmHistory, forceHistory)
		}

		routeRes.UsageBypass = false
		rerouteCtx, rerouteSpan := startRoutingSpan(ctx, req)
		decision, rerouteErr := s.routeFor(rerouteCtx, req)
		finishRoutingSpan(rerouteSpan, decision, rerouteErr)
		if rerouteErr != nil {
			log.Error("Reroute after usage-bypass failure failed", "err", rerouteErr)
			return rerouteErr
		}
		routeRes.Decision = decision
		routeRes.Fresh = decision
	}

	routeRes.SuggestionMode = r.Header.Get("x-weave-suggestion-mode") == "true"
	decision := routeRes.Decision
	if !agentShadowMode {
		s.firePolicyShadowForServingDecision(ctx, decision, req)
	}
	tt := routeRes.TurnType
	stickyHit := routeRes.StickyHit
	pinTier := routeRes.PinTier
	pinAgeSec := routeRes.PinAgeSec
	routeMs := time.Since(routeStart).Milliseconds()
	s.logPlannerOutcome(ctx, routeRes)

	// Cross-envelope no-progress detector: repeated identical fingerprints in a
	// window mean the agent is stuck — break the pin and emit a synthetic stop.
	// Gated to tool-bearing turns so a frozen marker + frozen prompt prefix
	// can't collide on healthy text-only turns.
	toolBearingTurn := inboundToolCallCount > 0 || inboundLastUser.HasToolResult
	if !agentShadowMode && !routeRes.AuthoritativePerTurn && toolBearingTurn && s.noProgress != nil {
		fp := computeNoProgressFingerprint(decision, promptText, feats.MessageCount, toolProgressMarker(env))
		role := roleForTier(catalog.TierFor(feats.Model))
		if looped, count := s.noProgress.recordAndDetect(routeRes.SessionKey, installationID, role, fp, time.Now()); looped {
			return s.handleNoProgressBreak(ctx, w, env, count, installationID, routeRes.SessionKey, role, decision.Model, decision.Provider, feats.Tokens, routeRes.Decision.Reason)
		}
	}

	// Text-repetition break: fresh tool calls each turn defeat the no-progress
	// fingerprint; repeated narration is the durable tell. See text_repetition.go.
	if !agentShadowMode && !routeRes.AuthoritativePerTurn && s.ResolveTextRepetitionBreakEnabled(ctx) && (turntype.DetectFromEnvelope(env, feats, "") == turntype.MainLoop || turntype.DetectFromEnvelope(env, feats, "") == turntype.ToolResult) {
		if looped, count, sampleHash := detectTextRepetition(env); looped {
			role := roleForTier(catalog.TierFor(feats.Model))
			return s.handleTextRepetitionBreak(ctx, w, env, count, sampleHash, installationID, routeRes.SessionKey, role, decision.Model, decision.Provider, feats.Tokens)
		}
	}

	// Shadow-mode spiral detector: log-only death-march signals (error grind,
	// same-file thrash, fuzzy repetition, monologue), once per (session,
	// reason), so fire rates/precision can be measured before any escalation
	// is armed. Main-loop / tool-result turns only — hard-pinned turn types
	// carry history shapes that mimic the signals.
	if !agentShadowMode && s.ResolveSpiralShadowEnabled(ctx) && (turntype.DetectFromEnvelope(env, feats, "") == turntype.MainLoop || turntype.DetectFromEnvelope(env, feats, "") == turntype.ToolResult) {
		if len(inboundSpiralReasons) > 0 {
			role := roleForTier(catalog.TierFor(feats.Model))
			// Use the bindRequestLogger digest, not routeRes.SessionKey (zero
			// with no pin store), so the spiral event's session_key matches the
			// telemetry row's in every mode for the offline join.
			trainingAllowed, _ := ctx.Value(PolicyTrainingAllowedContextKey{}).(bool)
			s.handleSpiralShadow(ctx, inboundSpiralSignals, inboundSpiralReasons,
				installationID, sessionKey, role, decision.Model, string(tt),
				trainingAllowed, s.effectiveCaptureMode(ctx))
		}
	}

	// Shadow-mode struggle detector: log-only, once per (session, reason).
	// Turn count and age come from the pin — no-ops on fresh (unpinned) sessions.
	if !agentShadowMode && s.ResolveStruggleShadowEnabled(ctx) && (turntype.DetectFromEnvelope(env, feats, "") == turntype.MainLoop || turntype.DetectFromEnvelope(env, feats, "") == turntype.ToolResult) {
		var wall time.Duration
		if !routeRes.PinFirstPinnedAt.IsZero() {
			wall = time.Since(routeRes.PinFirstPinnedAt)
		}
		if reasons := struggleReasons(routeRes.PinTurnCount, wall); len(reasons) > 0 {
			role := roleForTier(catalog.TierFor(feats.Model))
			// Same session_key rationale as the spiral block above.
			s.handleStruggleShadow(ctx, reasons[0], installationID, sessionKey, role,
				decision.Model, string(tt), routeRes.PinTurnCount, wall,
				routeRes.SessionEverSwitched, feats.Tokens)
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
	if !agentShadowMode && !routeRes.AuthoritativePerTurn && decision.Provider != providers.ProviderAnthropic && !routeRes.HardPinned && !routeRes.Handover.Invoked && !compRes.Applied && routeRes.PrefixTrimmed {
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
	// Subscription-only turns are excluded (like the OpenAI path): the mode is an
	// unfoldable routing signal absent from the cache key, so a stored body would
	// bypass the exhausted-sub 402 guard and the depleted-credits warning below.
	// Subscription-state conditional model lists are likewise absent from the
	// cache key, so never cache a request after one has been selected. Plan-aware
	// exclusions are also absent from the key and must bypass the cache.
	cacheEligible := s.semanticCacheAllowed(ctx) && s.semanticCache != nil && !env.Stream() && decision.Metadata != nil && externalID != "" && !bypassEval && !compactionHandoverRan && !billing.SubscriptionOnlyFromContext(ctx) && len(s.subsidyFactors(ctx, r.Header)) == 0 && !subscriptionConditionalModelsConfigured(ctx) && len(subscriptionPlanAwareExcludedModelsFromContext(ctx)) == 0 && !requestAllowedModelsPresent(ctx)
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
	w.Header().Set(HeaderRouterContextWindow, strconv.Itoa(contextWindowForRequest(decision.Model, decision.Provider)))
	if !agentShadowMode {
		s.setFeedbackLinkHeader(ctx, w, installationID, externalID, requestID, auth.UserIDFrom(ctx))
	}

	if _, err := s.provider(decision.Provider); err != nil {
		return err
	}

	reqPricing := otel.Lookup(s.baselineFor(feats.Model))
	actPricing := otel.Lookup(decision.Model)
	decisionBuilder := otel.NewAttrBuilder(45).
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
		Bool("routing.policy_fallback", routeRes.PolicyFallback).
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
	applySidecarAttrs(decisionBuilder, routeRes)
	applyPlannerAttrs(decisionBuilder, routeRes)
	applyRoutingStateAttrs(decisionBuilder, routeRes, decision.ServedIdentity(), sessionKey)
	otel.Record(ctx, otel.Span{
		Name:  "router.decision",
		Start: requestStart,
		End:   time.Now(),
		Attrs: decisionBuilder.Build(),
	})
	otel.Flush(ctx)

	opts := translate.EmitOptions{
		TargetModel:                       decision.Model,
		TargetProvider:                    decision.Provider,
		Capabilities:                      router.Lookup(decision.Model),
		IncludeStreamUsage:                s.usageRequired(),
		SessionAffinity:                   sessionAffinityHint(routeRes.SessionKey),
		ModelSwitched:                     routeRes.modelSwitched(),
		EnableExtendedContext:             shouldEnableExtendedContext(env.FullTokenEstimate(), outputReserve),
		EnableServerSideFallback:          s.ResolveAnthropicServerSideFallback(ctx),
		KeepCrossVendorOrchestrationTools: s.ccOrchToolsCrossVendor,
	}
	effortServed := s.resolveEffort(ctx, decision, opts.Capabilities, routeRes.EscalateEffort)
	effortServed.apply(&opts)

	// A caller whose Claude subscription has bound its plan window can't serve
	// another turn on it (429 until reset). Suppress the spent token so
	// resolution falls through to the deployment/BYOK key — the turn serves on
	// the Weave key (full cost) instead of hard-failing. Only fires once the
	// observer has recorded exhaustion and a fallback key exists.
	if s.claudeSubscriptionExhausted(ctx, r.Header) {
		ctx = withSuppressedClaudeSubscription(ctx)
	}
	ctx = resolveAndInjectCredentials(ctx, decision.Provider, decision.Model, r.Header)
	opts.FastMode = fastModeForAttempt(ctx, decision.Model, decision.Provider)

	// Wrap every request (not just multi-binding) in a preludeBuffer so a
	// pre-first-byte upstream error can discard the buffered prelude (marker +
	// message_start) and render an error envelope instead of stranding the
	// marker on the wire. Single-binding requests used to skip this for TTFB,
	// but the v0.58 SWE-bench bake-off traced 46/84 empty-patch failures to
	// exactly that: an api_error left Claude Code with only marker text and no
	// tool_use. Cost: one round-trip's buffered SSE bytes (~200B).
	bindings := s.resolveBindingsForDispatch(ctx, decision)

	// Subscription-only mode: a non-bypass turn (hard-pin, force-model, sticky)
	// wins before usage-bypass in runTurnLoop but can still serve free on the
	// caller's own Claude OAuth credential. Gate on whether the resolved
	// credential is that subscription (like the OpenAI path) rather than on the
	// bypass flag: refuse (402) only when the turn wouldn't run on the sub — it
	// routed to a paid model, or the subscription is observed-exhausted (a
	// doomed 429). Refusing beats billing a paid model against an already-
	// negative balance. Served-on-sub turns pin to the single Anthropic binding
	// (shouldFailover is already false with an OAuth credential in context; this
	// is belt-and-suspenders) so failover can't reroute onto a paid provider.
	if billing.SubscriptionOnlyFromContext(ctx) && !routeRes.UsageBypass {
		if (!servedOnSubscription(ctx) && !managedSubscriptionCanServe(ctx, decision.Provider, decision.Model)) || s.anthropicSubscriptionObservedExhausted(ctx, r.Header) {
			log.Info("Subscription-only request cannot be served on the subscription; refusing",
				"requested_model", feats.Model, "external_id", externalID, "decision_provider", decision.Provider)
			return ErrCreditsExhaustedSubscriptionUnavailable
		}
		bindings = []catalog.ProviderBinding{{Provider: decision.Provider}}
	}
	// Append the one-click feedback thumbs as a trailing content block,
	// wrapped below the capture layer so the footer never lands in
	// cached/logged bodies. Transparent when streaming/feedback is off.
	clientSink := w
	var streamCost *streamCostWriter
	if env.Stream() && !agentShadowMode {
		streamCost = newStreamCostWriter(clientSink)
		clientSink = streamCost
		// Innermost wrap: arms only once preludeBuffer commits, so a keepalive
		// can never strand a response the router still wants to retry.
		if s.sseKeepalive > 0 {
			keepalive := sse.NewKeepaliveWriter(clientSink, anthropicPingFrame, s.sseKeepalive)
			defer keepalive.Close()
			clientSink = keepalive
		}
		if footer := s.feedbackFooter(ctx, ClientIdentityFrom(ctx).ClientApp, routeRes.TurnType, footerEchoedSinceHumanTurn); footer != "" {
			clientSink = translate.NewAnthropicRoutingFooterWriter(clientSink, footer)
		}
	}
	contentSink, contentCap := s.maybeCaptureResponse(ctx, clientSink)
	var policyOutcomeCap *captureWriter
	if !agentShadowMode {
		contentSink, policyOutcomeCap = s.capturePolicyOutcomeResponse(ctx, contentSink, routeRes, decision)
	}
	preludeBuf := newPreludeBuffer(contentSink)
	var rootSink http.ResponseWriter = preludeBuf
	var captureW *captureWriter
	var sink http.ResponseWriter = rootSink
	if cacheEligible {
		captureW = newCaptureWriter(rootSink, semanticCacheMaxBodyBytes)
		sink = captureW
	}
	// Wrap sink to observe refusals on the native path (no translator Summary
	// here). Detection is unconditional: refusals must be measurable independently
	// of the re-pin kill switch.
	refusalObs := newRefusalObserver(sink)
	sink = refusalObs

	proxyStart := time.Now()
	inferenceParentCtx := ctx
	ctx, inferenceSpan := startInferenceSpan(ctx, decision)
	defer inferenceSpan.End()
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

	marker := suppressMarkerIfRequested(ctx, r.Header, routingMarkerFor(routeRes))
	// Subscription-only served-on-sub turn: replace the routing marker with the
	// depleted-credits warning (like the OpenAI path and the usage-bypass path),
	// not gated by the routing-marker opt-out. The pre-dispatch guard above has
	// already refused any turn that wouldn't run on the caller's own sub, so a
	// turn reaching here is served free and should carry the top-up CTA.
	if billing.SubscriptionOnlyFromContext(ctx) {
		marker = subscriptionOnlyWarningMarker
	}
	// toolValidator compiles the request's tool schemas once (LRU-cached);
	// translators validate/repair model tool calls against it. Nil if no tools.
	toolValidator := env.ToolValidator()
	setExtractor := func(e *otel.UsageExtractor) { extractor = e }
	// fastServed tracks whether the most recent attempt went out on the fast
	// tier; each attempt closure sets it before dispatch so the stream cost
	// calculator and post-dispatch billing price the winning attempt.
	fastServed := false
	setStreamCost := func(d router.Decision, inputIncludesCache bool) {
		if streamCost != nil {
			streamCost.SetCostCalculator(routerCostCalculatorFor(d.Model, d.Provider, fastServed), inputIncludesCache)
		}
	}
	anthropicTierAttemptFor := func(targetOpts translate.EmitOptions, prep providers.PreparedRequest, targetMarker string) *anthropicTierAttempt {
		return &anthropicTierAttempt{
			s:             s,
			log:           log,
			env:           env,
			r:             r,
			opts:          targetOpts,
			native:        s.anthropicNativeAttempt(env, r, prep, sink, preludeBuf, targetMarker, setExtractor, setStreamCost),
			sink:          sink,
			preludeBuf:    preludeBuf,
			marker:        targetMarker,
			setExtractor:  setExtractor,
			setStreamCost: setStreamCost,
			logBody: func(d router.Decision, body []byte) {
				logUpstreamBody(log, routeRes.SessionKey, d, feats, body)
			},
		}
	}
	recordFastServed := func(fast bool) { fastServed = fast }

	// buildAttempt dispatches by translation family so new OpenAI-compat
	// providers route automatically; a closure so in-turn model failover can
	// re-emit for a candidate in a different family.
	buildAttempt := func(target router.Decision, targetOpts translate.EmitOptions, targetMarker string) (dispatchAttempt, error) {
		switch providers.FamilyFor(target.Provider) {
		case providers.FamilyAnthropic:
			prep, emitErr := env.PrepareAnthropic(r.Header, targetOpts)
			if emitErr != nil {
				log.Error("Failed to emit Anthropic body", "err", emitErr)
				return nil, fmt.Errorf("emit body: %w", emitErr)
			}
			crossFormat = false
			logUpstreamBody(log, routeRes.SessionKey, target, feats, prep.Body)
			tiered := anthropicTierAttemptFor(targetOpts, prep, targetMarker)
			return func(actx context.Context, d router.Decision, p providers.Client) error {
				attemptOpts, err := tiered.dispatch(actx, d, p, recordFastServed)
				// Cortex documents output_config.format, so the knob goes out as
				// written; only a gateway whose relayed schema predates it rejects
				// it — re-emit once without it rather than sending every gateway
				// turn unstructured.
				if err == nil || committed(preludeBuf) || !providers.IsUpstreamOutputConfigFormatRejection(err) {
					return err
				}
				unstructuredOpts := attemptOpts
				unstructuredOpts.StripOutputConfigFormat = true
				unstructuredPrep, emitErr := env.PrepareAnthropic(r.Header, unstructuredOpts)
				if emitErr != nil {
					log.Error("Failed to re-emit Anthropic body without output_config.format", "err", emitErr)
					return err
				}
				log.Warn("Retrying Anthropic request without output_config.format after upstream rejected it",
					"model", d.Model,
					"provider", d.Provider,
					"request_id", requestID)
				if preludeBuf != nil {
					preludeBuf.Discard()
				}
				logUpstreamBody(log, routeRes.SessionKey, target, feats, unstructuredPrep.Body)
				return s.anthropicNativeAttempt(env, r, unstructuredPrep, sink, preludeBuf, targetMarker, setExtractor, setStreamCost)(actx, d, p)
			}, nil
		case providers.FamilyOpenAICompat:
			crossFormat = true
			// Prep rebuilt per attempt: targetIsOpenRouter(opts) gates four
			// OpenRouter-only body fields. On failover from Fireworks to
			// OpenRouter, the body must be re-emitted with TargetProvider =
			// openrouter so those gates fire.
			// One dispatch on the chosen surface, split into the raw upstream
			// error plus a finalize thunk so a gateway that rejects Responses can
			// be re-emitted onto chat/completions before finalize commits the
			// prelude buffer. Translators are stateful, so the retry calls again.
			dispatchOpenAICompat := func(actx context.Context, d router.Decision, p providers.Client, useResponses, stripPromptCacheKey bool) (error, func(error) error) {
				attemptOpts := targetOpts
				attemptOpts.TargetProvider = d.Provider
				attemptOpts.StripPromptCacheKey = stripPromptCacheKey
				attemptOpts.FastMode = fastModeForAttempt(actx, d.Model, d.Provider)
				fastServed = attemptOpts.FastMode
				setStreamCost(d, true)
				respSummary = translate.ResponseSummary{}
				var prep providers.PreparedRequest
				var emitErr error
				if useResponses {
					prep, emitErr = env.PrepareOpenAIResponses(r.Header, attemptOpts)
				} else {
					prep, emitErr = env.PrepareOpenAI(r.Header, attemptOpts)
				}
				if emitErr != nil {
					log.Error("Failed to translate Anthropic request to OpenAI format", "err", emitErr, "decision_provider", d.Provider, "responses_api", useResponses)
					return fmt.Errorf("translate anthropic request: %w", emitErr), func(err error) error { return err }
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
						WithRoutingMarker(targetMarker).
						WithEstimatedInputTokens(feats.Tokens).
						WithRequestHadTools(feats.HasTools).
						WithToolValidator(toolValidator)
				} else {
					translator = translate.NewAnthropicSSETranslator(sink, d.Model, usage).
						WithLogger(log).
						WithRoutingMarker(targetMarker).
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
				rawErr := p.Proxy(actx, d, prep, translator, r)
				finalize := func(err error) error {
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
				return rawErr, finalize
			}
			return func(actx context.Context, d router.Decision, p providers.Client) error {
				// Direct OpenAI serves every expressible turn on Responses;
				// gateways only the reasoning tool turn chat/completions rejects.
				gatewayKey := gatewayResponsesKey(actx, d.Provider)
				useResponses := translate.UseOpenAIResponsesAPI(translate.ResponsesRoute{
					Provider:       d.Provider,
					Capabilities:   targetOpts.Capabilities,
					HasTools:       feats.HasTools,
					ChatOnlyParams: env.RequiresChatCompletionsParams(targetOpts.Capabilities),
					Broad:          s.ResolveOpenAIResponsesBroad(actx),
				}) && !s.gatewayLacksResponses(gatewayKey)
				stripPCK := s.gatewayRejectsPromptCacheKey(gatewayKey)
				rawErr, finalize := dispatchOpenAICompat(actx, d, p, useResponses, stripPCK)
				// A gateway with no usable Responses surface answers 404, or 4xx
				// prose saying the API is off for this account. Re-emit onto
				// chat/completions once while pre-commit, and remember the answer
				// so the next turn skips the probe.
				if rawErr != nil && useResponses && !committed(preludeBuf) &&
					providers.IsUpstreamResponsesUnsupported(rawErr) {
					s.rememberGatewayLacksResponses(gatewayKey)
					log.Warn("Gateway rejected the Responses API; retrying on chat/completions",
						"model", d.Model,
						"decision_provider", d.Provider,
						"request_id", requestID)
					if preludeBuf != nil {
						preludeBuf.Discard()
					}
					useResponses = false
					rawErr, finalize = dispatchOpenAICompat(actx, d, p, false, stripPCK)
				}
				// prompt_cache_key is a spec Chat Completions field, but gateway schemas
				// that trail the spec 400 it as unknown. Re-emit once without the hint
				// while pre-commit; memoize the endpoint so later turns skip it.
				if rawErr != nil && !stripPCK && gatewayKey != "" && !committed(preludeBuf) &&
					providers.IsUpstreamPromptCacheKeyRejection(rawErr) {
					s.rememberGatewayRejectsPromptCacheKey(gatewayKey)
					log.Warn("Gateway rejected prompt_cache_key; retrying without the affinity hint",
						"model", d.Model,
						"decision_provider", d.Provider,
						"request_id", requestID)
					if preludeBuf != nil {
						preludeBuf.Discard()
					}
					rawErr, finalize = dispatchOpenAICompat(actx, d, p, useResponses, true)
				}
				return finalize(rawErr)
			}, nil
		case providers.FamilyGemini:
			prep, emitErr := env.PrepareGemini(r.Header, targetOpts)
			reqStats = prep.Stats
			if emitErr != nil {
				log.Error("Failed to translate Anthropic request to Gemini format", "err", emitErr)
				return nil, fmt.Errorf("translate anthropic request to gemini: %w", emitErr)
			}
			crossFormat = true
			logUpstreamBody(log, routeRes.SessionKey, target, feats, prep.Body)
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
				fastServed = false
				setStreamCost(d, true)
				respSummary = translate.ResponseSummary{}
				var usage otel.UsageSink
				if s.usageRequired() {
					extractor = otel.NewUsageExtractor(nil, d.Provider)
					usage = extractor
				}
				// SSE chain: Gemini → OpenAI → Anthropic.
				anthropicTr := translate.NewAnthropicSSETranslator(sink, d.Model, usage).
					WithLogger(log).
					WithRoutingMarker(targetMarker).
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
			return func(actx context.Context, d router.Decision, p providers.Client) error {
				rawErr, finalize := dispatchGemini(actx, d, p, prep)
				// VALIDATED-mode schema-grammar 400: retry once with mode=AUTO while
				// pre-commit. AUTO only drops the grammar constraint, so it can't make
				// things worse — a non-schema 400 just 400s again normally. The first
				// attempt's translators are abandoned (Discard).
				if rawErr != nil && geminiUsedValidated && !committed(preludeBuf) && upstreamStatus(rawErr) == http.StatusBadRequest {
					autoOpts := targetOpts
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
			}, nil
		default:
			return nil, fmt.Errorf("%w: %s (no translation path defined for inbound Anthropic Messages)", ErrProviderNotConfigured, target.Provider)
		}
	}
	attempt, attemptBuildErr := buildAttempt(decision, opts, marker)
	// An intrinsically-incompatible build error means the routed model provably
	// can't serve this shape — let it fall through to the baseline rescue below.
	if attemptBuildErr != nil && !translate.IsIntrinsicallyIncompatible(attemptBuildErr) {
		finishInferenceSpan(inferenceSpan, decision, decision.Provider, -1, attemptBuildErr)
		return attemptBuildErr
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
	baselineAllowed := modelPermittedByAllowlist(ctx, baselineModel) &&
		modelInRequestSubset(ctx, baselineModel)
	// baselineViable omits authoritative-per-turn: that contract governs which
	// model the policy picks, not whether a provably-unservable request can be rescued.
	baselineViable := !agentShadowMode &&
		decision.Reason != translate.ReasonUserForceModel &&
		s.shouldFailover(ctx) &&
		!anthropicExcluded &&
		baselineAllowed &&
		decision.Provider != providers.ProviderAnthropic &&
		baselineModel != decision.Model &&
		baselineKnown && baselineCatalog.PrimaryProvider() == providers.ProviderAnthropic
	baselineEligible := !routeRes.AuthoritativePerTurn && baselineViable

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
	// Suppressed in subscription-only mode: this retry serves on the Weave/BYOK
	// key at full cost, which is exactly the paid spend subscription-only mode
	// forbids — a subscription throttle there surfaces raw instead.
	subscriptionRetryEligible := decision.Provider == providers.ProviderAnthropic &&
		!agentShadowMode &&
		servedOnSubscription(ctx) &&
		!billing.SubscriptionOnlyFromContext(ctx) &&
		s.anthropicFallbackKeyAvailable(ctx)

	// Same-cluster model failover: when the routed model's only binding is dark,
	// degrade to a peer the policy already scored. Gated out for subscription-only
	// turns (a different model incurs the paid spend that mode forbids). BYOK
	// normally disables failover, but a gateway-aliased sibling uses the same
	// held credentials, so it stays eligible.
	siblingDecision, siblingFound := s.siblingFailoverDecision(ctx, decision, overflowEstimate, env.SignatureTokenSavings(), outputReserve)
	siblingViable := s.ResolveSiblingFailover(ctx) &&
		siblingFound &&
		!agentShadowMode &&
		decision.Reason != translate.ReasonUserForceModel &&
		(s.shouldFailover(ctx) || s.gatewaySiblingAllowed(ctx, siblingDecision)) &&
		!billing.SubscriptionOnlyFromContext(ctx)

	primaryProvider := decision.Provider
	// Captured before rescue: failover replaces decision.Model, so afterwards
	// decision.Model names the rescuer rather than what failed.
	primaryModel := decision.Model
	var winnerIdx int
	subscriptionPoolFailure := false
	if attemptBuildErr != nil {
		// Nothing was dispatched — enters the rescue chain as if every binding pre-committed failed.
		winnerIdx, proxyErr = -1, attemptBuildErr
	} else {
		winnerIdx, proxyErr = s.dispatchWithFallback(ctx, failoverInputs{
			// contentSink is the raw w when capture is off.
			w:                      contentSink,
			buf:                    preludeBuf,
			initialDecision:        decision,
			bindings:               bindings,
			attempt:                attempt,
			flushErr:               flushUpstreamErrorAsAnthropic,
			deferFlushOnExhaustion: baselineViable || subscriptionRetryEligible || siblingViable,
		})
		subscriptionPoolFailure = isSubscriptionPoolError(proxyErr)
	}

	// The deferred upstream error must reach the client exactly once: each
	// rescue hands ownership to the next, and whichever declines to run flushes.
	// Writing it forecloses any later rescue.
	deferredErrFlushed := false
	flushDeferredErr := func() {
		if deferredErrFlushed || subscriptionPoolFailure {
			return
		}
		deferredErrFlushed = true
		flushUpstreamErrorAsAnthropic(contentSink, proxyErr)
	}

	// The routed model's bindings all failed with a fault another model could
	// satisfy, pre-commit — re-dispatch the requested model on Anthropic.
	// crossFormat/respSummary/reqStats reset to Anthropic-native values so
	// telemetry reflects the binding that actually served.
	baselineFailoverUsed := false
	baselineAttempted := false
	// Capability rejection means the routed model cannot serve this shape at all —
	// rescue via baseline even when the policy owns per-turn selection.
	capabilityRejected := providers.IsUpstreamCapabilityRejection(proxyErr)
	// A provably-dead-arm rejection (schema/capability/intrinsically-incompatible)
	// is snapshotted before any rescue runs — the rescue nils proxyErr on
	// success, which would otherwise hide the rejection from the post-rescue
	// pin-eviction decision below.
	deadArmRejected := capabilityRejected || providers.IsUpstreamSchemaRejection(proxyErr) || translate.IsIntrinsicallyIncompatible(proxyErr)
	if capabilityRejected {
		log.Error("Upstream rejected the request as unsupported by the routed model",
			"model", decision.Model,
			"provider", primaryProvider,
			"upstream_status", upstreamStatus(proxyErr),
			"has_images", feats.HasImages)
	}
	if proxyErr != nil && !preludeBuf.Committed() &&
		((baselineEligible && (providers.IsRetryable(proxyErr) || providers.IsUpstreamModelNotFound(proxyErr))) ||
			(baselineViable && (capabilityRejected || translate.IsIntrinsicallyIncompatible(proxyErr) || providers.IsUpstreamSchemaRejection(proxyErr)))) {
		baselineDecision := decision
		baselineDecision.Model = baselineModel
		baselineDecision.Provider = providers.ProviderAnthropic
		baselineOpts := opts
		baselineOpts.TargetModel = baselineModel
		baselineOpts.TargetProvider = providers.ProviderAnthropic
		baselineOpts.Capabilities = router.Lookup(baselineModel)
		// Recompute against the model that actually serves, not the cost-routed
		// OSS id — otherwise PrepareAnthropic may leave stale signed thinking
		// blocks the baseline model rejects (400). Compare bare model IDs:
		// baselineModel carries no effort, and any effort on the prior identity
		// belonged to a different model, so the model comparison already
		// subsumes it.
		baselineOpts.ModelSwitched = baseModelOf(routeRes.PriorServedModel) != baselineModel ||
			routeRes.SessionEverSwitched
		// The arm's level named the failed model's menu; the baseline resolves
		// its own so the persisted identity matches what goes on the wire.
		baselineDecision.Effort = ""
		effortServed = s.resolveEffort(ctx, baselineDecision, baselineOpts.Capabilities, routeRes.EscalateEffort)
		effortServed.apply(&baselineOpts)
		baselineCtx := ctx
		baselineSubExhausted := s.claudeSubscriptionExhausted(ctx, r.Header)
		if baselineSubExhausted {
			baselineCtx = withSuppressedClaudeSubscription(baselineCtx)
		}
		baselineCtx = resolveAndInjectCredentials(baselineCtx, providers.ProviderAnthropic, baselineModel, r.Header)
		baselineOpts.FastMode = fastModeForAttempt(baselineCtx, baselineModel, providers.ProviderAnthropic)
		baselinePrep, baselineEmitErr := env.PrepareAnthropic(r.Header, baselineOpts)
		if baselineEmitErr != nil {
			log.Error("Baseline failover: emit Anthropic body failed; surfacing original error", "err", baselineEmitErr, "baseline_model", baselineModel)
			if !siblingViable {
				flushDeferredErr()
			}
		} else {
			log.Warn("Baseline failover: retrying requested model on Anthropic",
				"failed_model", decision.Model,
				"failed_provider", primaryProvider,
				"baseline_model", baselineModel,
				"err", proxyErr)
			if baselineSubExhausted {
				ctx = withSuppressedClaudeSubscription(ctx)
			}
			baselineBindings := s.resolveBindingsForDispatch(baselineCtx, baselineDecision)
			baselineMarker := suppressMarkerIfRequested(ctx, r.Header, baselineRoutingMarkerFor(routeRes, baselineModel))
			baselineAttempt := anthropicTierAttemptFor(baselineOpts, baselinePrep, baselineMarker).attempt(recordFastServed)
			fastServed = baselineOpts.FastMode
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
			subscriptionPoolFailure = isSubscriptionPoolError(proxyErr)
			decision = baselineDecision
			bindings = baselineBindings
			baselineAttempted = true
			// Reflect whether the baseline actually served — a failed retry must
			// not report baseline_failover=true and skew bake-off analysis.
			baselineFailoverUsed = proxyErr == nil
		}
	} else if baselineViable && !siblingViable && proxyErr != nil {
		// Baseline didn't run (mid-stream commit, or non-failoverable error);
		// surface the deferred original error now. Guard must match
		// deferFlushOnExhaustion above, or a deferred error is never flushed —
		// unless the sibling rescue below owns the deferred flush instead.
		flushDeferredErr()
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
		subCtx = resolveAndInjectCredentials(subCtx, providers.ProviderAnthropic, decision.Model, r.Header)
		// Model is unchanged, but rebuild prep so the retry gets a pristine
		// PreparedRequest under the suppressed-subscription context — which
		// now pays on the Weave key, so the fast-mode opt-in applies.
		subOpts := opts
		subOpts.FastMode = fastModeForAttempt(subCtx, decision.Model, providers.ProviderAnthropic)
		subPrep, subEmitErr := env.PrepareAnthropic(r.Header, subOpts)
		if subEmitErr != nil {
			log.Error("Subscription failover: emit Anthropic body failed; surfacing original error", "err", subEmitErr, "model", decision.Model)
			if !siblingViable {
				flushDeferredErr()
			}
		} else if subBindings := s.resolveBindingsForDispatch(subCtx, decision); len(subBindings) == 0 {
			// No usable Anthropic binding under suppression — surface the
			// original retryable error (real throttle) rather than a synthetic
			// 502 that would mask it. No Weave key attempted, so attribution
			// stays on the subscription.
			log.Warn("Subscription failover: no fallback Anthropic binding available; surfacing original error",
				"model", decision.Model,
				"err", proxyErr,
				"upstream_status", upstreamStatus(proxyErr))
			if !siblingViable {
				flushDeferredErr()
			}
		} else {
			log.Warn("Subscription failover: subscription throttled/timed out, retrying requested model on Weave key",
				"model", decision.Model,
				"err", proxyErr,
				"upstream_status", upstreamStatus(proxyErr))
			subAttempt := anthropicTierAttemptFor(subOpts, subPrep, marker).attempt(recordFastServed)
			fastServed = subOpts.FastMode
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
				// A failed retry keeps the same dark model; hold the error so
				// the sibling rescue below can still serve the turn.
				deferFlushOnExhaustion: siblingViable,
			})
			subscriptionPoolFailure = isSubscriptionPoolError(proxyErr)
			bindings = subBindings
			subscriptionFailoverUsed = proxyErr == nil
		}
	}
	// The subscription retry didn't run (mid-stream commit, or non-retryable
	// error); surface the deferred original error now so it's never dropped.
	if subscriptionRetryEligible && !siblingViable && !baselineAttempted && !subscriptionRetryRan && proxyErr != nil && !preludeBuf.Committed() {
		flushDeferredErr()
	}

	// Same-cluster failover: all bindings exhausted pre-commit — re-dispatch
	// the next policy candidate. Last in the rescue chain.
	siblingFailoverUsed := false
	siblingRescueRan := false
	// Keyed off the flush, not off whether an earlier rescue ran: a failed
	// subscription retry keeps the same dark model, so a cluster peer can still
	// serve — but only before the deferred error reaches the wire.
	siblingRescueOwed := siblingViable && !baselineAttempted && !deferredErrFlushed
	if siblingRescueOwed && proxyErr != nil && !preludeBuf.Committed() &&
		(providers.IsRetryable(proxyErr) ||
			providers.IsUpstreamModelNotFound(proxyErr) ||
			providers.IsUpstreamProviderBillingBlocked(proxyErr) ||
			providers.IsUpstreamSchemaRejection(proxyErr)) {
		siblingOpts := opts
		siblingOpts.TargetModel = siblingDecision.Model
		siblingOpts.TargetProvider = siblingDecision.Provider
		siblingOpts.Capabilities = router.Lookup(siblingDecision.Model)
		// The turn now serves a model the session hasn't seen, so signed
		// thinking blocks from the prior model must not be replayed verbatim.
		siblingOpts.ModelSwitched = true
		effortServed = s.resolveEffort(ctx, siblingDecision, siblingOpts.Capabilities, routeRes.EscalateEffort)
		effortServed.apply(&siblingOpts)
		siblingCtx := resolveAndInjectCredentials(ctx, siblingDecision.Provider, siblingDecision.Model, r.Header)
		siblingOpts.FastMode = fastModeForAttempt(siblingCtx, siblingDecision.Model, siblingDecision.Provider)
		siblingBindings := s.resolveBindingsForDispatch(siblingCtx, siblingDecision)
		siblingMarker := suppressMarkerIfRequested(ctx, r.Header, siblingRoutingMarkerFor(routeRes, siblingDecision.Model))
		siblingAttempt, siblingBuildErr := buildAttempt(siblingDecision, siblingOpts, siblingMarker)
		switch {
		case siblingBuildErr != nil:
			log.Error("Sibling failover: preparing the candidate request failed; surfacing original error",
				"err", siblingBuildErr,
				"sibling_model", siblingDecision.Model)
		case len(siblingBindings) == 0:
			log.Warn("Sibling failover: candidate has no usable binding; surfacing original error",
				"sibling_model", siblingDecision.Model,
				"sibling_provider", siblingDecision.Provider,
				"err", proxyErr)
		default:
			log.Warn("Sibling failover: routed model exhausted, retrying a same-cluster candidate",
				"failed_model", decision.Model,
				"failed_provider", primaryProvider,
				"sibling_model", siblingDecision.Model,
				"sibling_provider", siblingDecision.Provider,
				"upstream_status", upstreamStatus(proxyErr),
				"err", proxyErr)
			siblingRescueRan = true
			respSummary = translate.ResponseSummary{}
			reqStats = providers.RequestMutationStats{}
			winnerIdx, proxyErr = s.dispatchWithFallback(siblingCtx, failoverInputs{
				w:               contentSink,
				buf:             preludeBuf,
				initialDecision: siblingDecision,
				bindings:        siblingBindings,
				attempt:         siblingAttempt,
				flushErr:        flushUpstreamErrorAsAnthropic,
			})
			subscriptionPoolFailure = isSubscriptionPoolError(proxyErr)
			decision = siblingDecision
			bindings = siblingBindings
			marker = siblingMarker
			siblingFailoverUsed = proxyErr == nil
		}
	}
	// The sibling rescue didn't run; surface the deferred original error now so
	// it's never dropped.
	if siblingRescueOwed && !siblingRescueRan && proxyErr != nil && !preludeBuf.Committed() {
		flushDeferredErr()
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
	ctx = resolveAndInjectCredentials(ctx, finalProvider, decision.Model, r.Header)

	// Re-resolve pricing for the binding that actually served: the
	// pre-dispatch lookup always returns the catalog's PRIMARY binding price,
	// which would misreport cost after a successful failover to a different
	// binding's rate — or after a fast-tier dispatch, billed at the fast rate.
	if actBindingPricing, ok := servedPricing(finalProvider, decision.Model, fastServed); ok {
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
	finishInferenceSpan(inferenceSpan, decision, finalProvider, winnerIdx, proxyErr)
	ctx = restoreParentSpan(ctx, inferenceParentCtx)

	// On the native path there is no translator Summary, so a refusal would
	// otherwise never reach the completion log or the routing_decisions row.
	if refusalObs.refused && respSummary.StopReason == "" {
		respSummary.StopReason = anthropicRefusalStopReason
	}

	in, out := extractor.Tokens()
	cacheCreation, cacheRead := extractor.CacheTokens()
	if responseBuffer != nil && proxyErr == nil {
		setRouterCostHeaders(w.Header(), routerResponseCostFromPricing(actPricing, decision.Provider, in, out, cacheCreation, cacheRead))
	}
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
		Bool("upstream.refusal", refusalObs.refused).
		String("upstream.refusal_category", refusalObs.category).
		Int64("usage.input_tokens", int64(in)).
		Int64("usage.output_tokens", int64(out)).
		Int64("usage.cache_creation_input_tokens", int64(cacheCreation)).
		Int64("usage.cache_read_input_tokens", int64(cacheRead)).
		Float64("cost.requested_input_usd", catalog.EffectiveInputCost(in, cacheCreation, cacheRead, reqPricing.InputUSDPer1M, reqPricing, decision.Provider)).
		Float64("cost.requested_output_usd", catalog.EffectiveOutputCost(out, reqPricing.OutputUSDPer1M)).
		Float64("cost.actual_input_usd", catalog.EffectiveInputCost(in, cacheCreation, cacheRead, actPricing.InputUSDPer1M, actPricing, decision.Provider)).
		Float64("cost.actual_output_usd", catalog.EffectiveOutputCost(out, actPricing.OutputUSDPer1M)).
		Bool("cost.subscription_served", servedOnSubscription(ctx)).
		Bool("cost.fast_mode", fastServed).
		Int64("latency.upstream_ms", proxyMs).
		Int64("latency.total_ms", time.Since(requestStart).Milliseconds()).
		Int64("upstream.status_code", int64(upstreamStatus(proxyErr))).
		Bool("routing.cross_format", crossFormat).
		String("dispatch.primary_provider", primaryProvider).
		String("dispatch.primary_model", primaryModel).
		String("dispatch.final_provider", finalProvider).
		Int64("dispatch.fallback_attempts", int64(winnerIdx)).
		Bool("dispatch.failover_used", finalProvider != primaryProvider || subscriptionFailoverUsed || siblingFailoverUsed).
		Bool("dispatch.baseline_failover", baselineFailoverUsed).
		Bool("dispatch.subscription_failover", subscriptionFailoverUsed).
		Bool("dispatch.sibling_failover", siblingFailoverUsed)
	applyPlannerAttrs(upstreamBuilder, routeRes)
	applyRoutingStateAttrs(upstreamBuilder, routeRes, decision.ServedIdentity(), sessionKey)
	applyEffortAttrs(upstreamBuilder, effortServed)
	addTimingAttrs(ctx, upstreamBuilder)

	obs := buildObservationContext(ctx, decision, routeRes.Fresh, s.effectiveCaptureMode(ctx))
	obs.applySpanAttrs(upstreamBuilder)

	otel.Record(ctx, otel.Span{
		Name:  "router.upstream",
		Start: proxyStart,
		End:   time.Now(),
		Attrs: upstreamBuilder.Build(),
	})
	respBody, respTrunc := capturedResponse(contentCap)
	// Eval bodies are captured offline; exclude them from call-log so they are not mistaken for serving traffic.
	if !agentShadowMode {
		s.recordCallLog(ctx, upstreamBuilder.Build(), proxyErr != nil, body, respBody, respTrunc)
	}
	otel.Flush(ctx)

	if !agentShadowMode {
		s.recordTurnUsage(routeRes, finalProvider, decision.ServedIdentity(), in, out, cacheCreation, cacheRead)
	}

	// Eval rows must not enter serving telemetry; they would corrupt offline policy analysis.
	if !agentShadowMode && installationID != uuid.Nil {
		credentialKeyPrefix, credentialKeySuffix, credSource := s.credentialKeyParts(ctx)
		// Same-provider subscription->Weave retries keep finalProvider ==
		// primaryProvider, so OR in subscriptionFailoverUsed to match the OTel
		// span + completion log.
		failoverUsed := finalProvider != primaryProvider || subscriptionFailoverUsed || siblingFailoverUsed
		degShadow := proxyErr == nil && isDegenerateResponse(out, respSummary.ToolUseBlocks, respSummary.StopReason, respSummary.StopReasonDemoted)
		if degShadow && !agentShadowMode {
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
			s.evictPinAfterDegenerateResponse(ctx, stickyHit, decision.Reason, installationID, routeRes.SessionKey, stickyStateRole(routeRes))
		}
		tel := InsertTelemetryParams{
			InstallationID:         installationID.String(),
			APIKeyID:               apiKeyIDFromContext(ctx),
			RequestID:              requestID,
			SpanType:               "router.upstream",
			TraceID:                requestID,
			Timestamp:              requestStart,
			RequestedModel:         feats.Model,
			DecisionModel:          decision.Model,
			DecisionProvider:       decision.Provider,
			DecisionReason:         telemetryDecisionReason(ctx, decision.Reason),
			RequestedAllowedModels: requestedAllowedModelsForTelemetry(ctx),
			EstimatedInputTokens:   int32(feats.Tokens),
			StickyHit:              stickyHit,
			PinTier:                routeRes.PinTier,
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
			Strategy:               obs.Strategy,
			RouteID:                obs.RouteID,
			PolicyRouteKey:         obs.PolicyRouteKey,
			PolicyArtifactID:       obs.PolicyArtifactID,
			PolicyArtifactSHA256:   obs.PolicyArtifactSHA256,
			RosterVersion:          obs.RosterVersion,
			SidecarSchemaVersion:   obs.SidecarSchemaVersion,
			TrainingAllowed:        obs.TrainingAllowed,
			CaptureMode:            obs.CaptureMode,
			DebugRef:               obs.DebugRef,
			TTFTMs:                 obs.TTFTMs,
			CacheCreationTokens:    cacheTokenPtr(cacheCreation),
			CacheReadTokens:        cacheTokenPtr(cacheRead),
			DeviceID:               clientID.DeviceID,
			SessionID:              clientID.SessionID,
			RouterUserID:           auth.UserIDFrom(ctx),
			ClientApp:              clientID.ClientApp,
			TurnType:               string(routeRes.TurnType),
			RolloutID:              obs.RolloutID,
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
			// Phase 0 instrumentation — Anthropic only; see unified_limit_capture.go.
			UnifiedLimitHeaders: unifiedLimitHeadersJSON(ctx),
		}
		applyPlannerTelemetry(&tel, routeRes)
		applyAuthorityShadowTelemetry(&tel, routeRes)
		// Hard-pinned turn types carry history shapes that mimic failure signals,
		// so only the detector's trusted turn types enter the training corpus.
		signalTurn := tt == turntype.MainLoop || tt == turntype.ToolResult
		applyTurnSignalTelemetry(&tel, inboundSpiralSignals, inboundSpiralReasons,
			inboundSpiralComputed && signalTurn,
			s.ResolveTurnSignalCaptureEnabled(ctx),
			obs.TrainingAllowed,
			s.effectiveCaptureMode(ctx))
		s.fireTelemetry(tel)
	}

	// No-op when billing is unwired (selfhosted); only reached on a real
	// upstream call since the cache-hit branch above already returned.
	if proxyErr == nil && !agentShadowMode {
		s.emitBilling(ctx, requestID, externalID, decision, actPricing, routeRes, in, out, cacheCreation, cacheRead)
		if compRes.Summarized {
			s.billCompactionSummary(ctx, requestID, externalID, compRes.SummaryUsage)
		}
		if compactionHandoverOutcome.Invoked && !compactionHandoverOutcome.FallbackToFullHistory {
			s.billAuxiliaryInference(ctx, requestID, auxSuffixCompactionHandoverSummry, externalID, compactionHandoverOutcome.SummaryUsage)
		}
	}

	// Two-strike eviction: a session pinned to a model returning non-retryable
	// 4xx wedges until manually /force-model'd out. Expires the pin after a
	// persistent counter hits threshold; successful turns reset it.
	if !agentShadowMode {
		s.maybeEvictPinAfterUpstreamErr(ctx, stickyHit, proxyErr, decision.Reason, installationID, routeRes.SessionKey, stickyStateRole(routeRes))

		// A schema/capability/incompatible rejection marks the pinned arm provably
		// dead for this request shape — the pin must not stay on it even when a
		// rescue served the turn (which would nill proxyErr and reset the counter).
		s.maybeExpireDeadArmPin(ctx, deadArmRejected, decision.Reason, installationID, routeRes.SessionKey, stickyStateRole(routeRes))

		// Two-strike provider disable: complements the 4xx eviction above;
		// 529 is retryable in-turn so it never trips that counter.
		// Skipped when baseline rescue ran: finalProvider is the rescue
		// provider, not the sticky pin's, so disabling it evicts the wrong pin.
		if !baselineAttempted {
			s.maybeDisableProviderAfterOverload(ctx, stickyHit, proxyErr, finalProvider, decision.Reason, installationID, routeRes.SessionKey, stickyStateRole(routeRes), routeRes.PinRole)
		}

		// Re-pin the session off the refusing model if a safety refusal was observed.
		s.maybeRepinOnRefusal(ctx, refusalObs, routeRes.SessionKey, stickyStateRole(routeRes), decision)
	}

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

	log.Info("ProxyMessages complete", append([]any{"requested_model", feats.Model, "baseline_model", s.baselineFor(feats.Model), "decision_model", decision.Model, "decision_provider", decision.Provider, "primary_provider", primaryProvider, "primary_model", primaryModel, "fallback_attempts", winnerIdx, "failover_used", finalProvider != primaryProvider || subscriptionFailoverUsed || siblingFailoverUsed, "subscription_failover", subscriptionFailoverUsed, "decision_reason", decision.Reason, "requested_tier", routeRes.RequestedTier.String(), "decision_tier", catalog.TierFor(decision.Model).String(), "embedded_tokens", len(promptText) / 4, "total_input_tokens", feats.Tokens, "has_tools", feats.HasTools, "message_count", feats.MessageCount, "last_kind", feats.LastKind, "last_preview", feats.LastPreview, "embed_input", embedInput, "cross_format", crossFormat, "sticky_hit", stickyHit, "route_ms", routeMs, "proxy_ms", proxyMs, "proxy_err", proxyErr, "upstream_err_body", providers.UpstreamErrorBodyMessage(proxyErr), "upstream_status", upstreamStatus(proxyErr), "upstream_finish_reason", respSummary.UpstreamFinishReason, "resp_stop_reason", respSummary.StopReason, "resp_refusal", refusalObs.refused, "resp_refusal_category", refusalObs.category, "stop_reason_promoted", respSummary.StopReasonPromoted, "tool_use_blocks", respSummary.ToolUseBlocks, "invalid_tool_args_blocks", respSummary.InvalidToolArgsBlocks, "text_only_turn_nudged", respSummary.TextOnlyTurnNudged, "stop_reason_demoted", respSummary.StopReasonDemoted, "suppressed_tool_calls", respSummary.SuppressedToolCalls, "tool_call_invalid_blocks", len(respSummary.ToolCallIssues), "cc_only_tools_stripped", reqStats.CCOnlyToolsStripped, "gemini_reminder_injected", reqStats.GeminiReminderInjected, "gemini_validated_tool_mode", reqStats.GeminiValidatedToolMode, "resp_output_tokens", respSummary.OutputTokens, "prelude_committed", preludeBuf.Committed(), "routing_marker", marker, "prior_served_model", routeRes.PriorServedModel, "hard_pinned", routeRes.HardPinned}, plannerLogFields(routeRes)...)...)
	policyRespBody, policyRespTrunc := capturedResponse(policyOutcomeCap)
	var policyResp *policyOutcomeResponse
	if policyOutcomeCap != nil {
		policyResp = &policyOutcomeResponse{Body: policyRespBody, Truncated: policyRespTrunc}
	}
	if !agentShadowMode {
		s.reportPolicyOutcome(ctx, routeRes, decision, effortServed, finalProvider, fastServed, feats.Tokens, in, out, cacheCreation, cacheRead, routeMs, proxyMs, proxyErr, policyResp)
	}
	return proxyErr
}

// applyPlannerAttrs stamps planner and handover attributes onto a span
// attribute builder. Planner details are omitted when the planner did not run
// so downstream nullable columns do not turn zero values into false evidence.
func applyPlannerAttrs(b *otel.AttrBuilder, res turnLoopResult) *otel.AttrBuilder {
	if res.PlannerDecision.Reason != "" {
		b.String("planner.outcome", plannerOutcomeAttr(res)).
			String("planner.reason", res.PlannerDecision.Reason).
			Float64("planner.expected_savings_usd", res.PlannerDecision.ExpectedSavingsUSD).
			Float64("planner.eviction_cost_usd", res.PlannerDecision.EvictionCostUSD).
			Float64("planner.threshold_usd", res.PlannerDecision.ThresholdUSD).
			String("planner.pin_model", res.PinModel).
			String("planner.fresh_model", res.Fresh.Model).
			String("planner.chosen_model", res.Decision.Model).
			Bool("planner.pin_cache_warm", !res.PlannerDecision.PinCacheCold).
			String("cache.pin_provider", res.PinProvider).
			Bool("cache.prefix_stable", !res.PrefixBroken).
			Bool("planner.pin_price_fallback", res.PlannerDecision.PinPriceFallback).
			Bool("planner.fresh_price_fallback", res.PlannerDecision.FreshPriceFallback)
		if res.PriorTurnGapMS != nil {
			b.Int64("cache.prior_turn_gap_ms", *res.PriorTurnGapMS)
		}
		if res.PlannerDecision.ShadowComputed {
			b.String("planner.shadow_outcome", plannerOutcome(res.PlannerDecision.ShadowOutcome)).
				Float64("planner.shadow_expected_savings_usd", res.PlannerDecision.ShadowExpectedSavingsUSD).
				Float64("planner.shadow_stay_cost_usd", res.PlannerDecision.ShadowStayCostUSD).
				Float64("planner.shadow_switch_cost_usd", res.PlannerDecision.ShadowSwitchCostUSD)
		}
	}
	b.Bool("handover.invoked", res.Handover.Invoked).
		Int64("handover.latency_ms", res.Handover.LatencyMS).
		Int64("handover.summary_tokens", int64(res.Handover.SummaryTokens)).
		Bool("handover.fallback_to_full_history", res.Handover.FallbackToFullHistory)
	return b
}

func plannerOutcome(outcome planner.Outcome) string {
	if outcome == planner.OutcomeSwitch {
		return "switch"
	}
	return "stay"
}

// applySidecarAttrs reads Fresh.Metadata, not the served decision:
// STAY replaces the decision with a pin (nil Metadata) even when the sidecar ran this turn.
// Timings and serving stats are independent, separately-nilable payloads on the same
// Metadata, so each gets its own guard below.
func applySidecarAttrs(b *otel.AttrBuilder, res turnLoopResult) *otel.AttrBuilder {
	if res.Fresh.Metadata == nil {
		return b
	}
	if st := res.Fresh.Metadata.SidecarTimings; st != nil {
		if st.EmbedMs != nil {
			b.Int64("latency.embed_ms", int64(math.Round(*st.EmbedMs)))
		}
		if st.SelectMs != nil {
			b.Int64("latency.sidecar_select_ms", int64(math.Round(*st.SelectMs)))
		}
		if st.OtherMs != nil {
			b.Int64("latency.sidecar_other_ms", int64(math.Round(*st.OtherMs)))
		}
	}
	if ss := res.Fresh.Metadata.SidecarStats; ss != nil {
		if ss.EmbedCacheHits != nil {
			b.Int64("routing.embed_cache_hits", *ss.EmbedCacheHits)
		}
		if ss.EmbedCacheMisses != nil {
			b.Int64("routing.embed_cache_misses", *ss.EmbedCacheMisses)
		}
		if ss.EmbedCacheEvictions != nil {
			b.Int64("routing.embed_cache_evictions", *ss.EmbedCacheEvictions)
		}
		if ss.RoutesInflight != nil {
			b.Int64("routing.sidecar_inflight", *ss.RoutesInflight)
		}
		if ss.OverrunsLive != nil {
			b.Int64("routing.sidecar_overruns_live", *ss.OverrunsLive)
		}
	}
	return b
}

// applyRoutingStateAttrs stamps thread identity and transition attrs; it
// distinguishes real model changes from first selection and sub-agent calls.
// servedIdentity, not the bare model: PriorServedModel is a serving identity
// ("gpt-5.6-luna:xhigh"), so comparing it against a bare id reports a model
// change on every effort-qualified turn.
func applyRoutingStateAttrs(
	b *otel.AttrBuilder,
	res turnLoopResult,
	servedIdentity string,
	sessionKey [sessionpin.SessionKeyLen]byte,
) *otel.AttrBuilder {
	return b.String("routing.session_key", sessionKeyHex(sessionKey)).
		String("routing.pin_role", res.PinRole).
		String("routing.prior_served_model", res.PriorServedModel).
		Bool("routing.model_changed", res.PriorServedModel != "" && res.PriorServedModel != servedIdentity)
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

func plannerLogFields(res turnLoopResult) []any {
	var pinCacheWarm any
	if res.PlannerDecision.Reason != "" {
		pinCacheWarm = !res.PlannerDecision.PinCacheCold
	}
	return []any{
		"planner_outcome", plannerOutcomeAttr(res),
		"planner_reason", res.PlannerDecision.Reason,
		"planner_expected_savings_usd", res.PlannerDecision.ExpectedSavingsUSD,
		"planner_eviction_cost_usd", res.PlannerDecision.EvictionCostUSD,
		"planner_threshold_usd", res.PlannerDecision.ThresholdUSD,
		"planner_pin_model", res.PinModel,
		"planner_fresh_model", res.Fresh.Model,
		"planner_chosen_model", res.Decision.Model,
		"planner_pin_cache_warm", pinCacheWarm,
		"cache_pin_provider", res.PinProvider,
		"cache_prefix_stable", !res.PrefixBroken,
		"cache_prior_turn_gap_ms", res.PriorTurnGapMS,
		"planner_pin_price_fallback", res.PlannerDecision.PinPriceFallback,
		"planner_fresh_price_fallback", res.PlannerDecision.FreshPriceFallback,
	}
}

func stickyStateRole(res turnLoopResult) string {
	if res.StickyHit && res.StickyRole != "" {
		return res.StickyRole
	}
	return res.PinRole
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
	message := "router stayed on pinned model"
	if isHMMDecision(res.Fresh) {
		message = "router stayed on previous HMM model"
	}
	log.Info(message,
		"model", res.Decision.Model,
		"pin_model", res.PinModel,
		"reason", res.PlannerDecision.Reason,
		"expected_savings_usd", res.PlannerDecision.ExpectedSavingsUSD,
		"eviction_cost_usd", res.PlannerDecision.EvictionCostUSD,
		"threshold_usd", res.PlannerDecision.ThresholdUSD,
		"pin_cache_warm", !res.PlannerDecision.PinCacheCold,
	)
}

func (s *Service) recordTurnUsage(res turnLoopResult, servedProvider, servedModel string, in, out, cacheCreation, cacheRead int) {
	if s.pinStore == nil || res.HardPinned {
		return
	}
	if isHMMTurn(res) {
		s.recordHMMTurnHistory(res, servedProvider, servedModel, in, out, cacheCreation, cacheRead)
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
		Strategy:            strategyForTurnLoopResult(res),
		InputTokens:         in,
		CachedReadTokens:    cacheRead,
		CachedWriteTokens:   cacheCreation,
		OutputTokens:        out,
		EndedAt:             time.Now(),
		ServedModel:         servedModel,
		ServedProvider:      servedProvider,
		PriorServedModel:    res.PriorServedModel,
		SessionEverSwitched: res.SessionEverSwitched,
	}
	role := res.PinRole
	if isUserForcedReason(res.Decision.Reason) {
		role = forceModelHistoryRole(role)
	}
	if role == "" {
		role = sessionpin.DefaultRole
	}
	if err := s.pinStore.UpdateUsage(context.Background(), res.SessionKey, role, usage); err != nil {
		observability.Get().Error("session pin usage writeback failed", "err", err)
	}
}

func isHMMTurn(res turnLoopResult) bool {
	return isHMMDecision(res.Decision) || isHMMDecision(res.Fresh)
}

func (s *Service) recordHMMTurnHistory(res turnLoopResult, servedProvider, servedModel string, in, out, cacheCreation, cacheRead int) {
	if servedModel == "" || res.InstallationID == uuid.Nil {
		return
	}
	var zeroKey [sessionpin.SessionKeyLen]byte
	if res.SessionKey == zeroKey {
		return
	}
	hasUsage := in != 0 || out != 0 || cacheCreation != 0 || cacheRead != 0
	strategyCtx := strategyContext(strategyForTurnLoopResult(res))
	historyProvider := servedProvider
	if !hasUsage {
		// A failed turn has no usage writeback; preserve the prior provider to
		// avoid an invalid model/provider pair on the next HMM stay.
		if prior := s.loadHMMHistory(strategyCtx, res.SessionKey, res.PinRole); prior.Provider != "" {
			historyProvider = prior.Provider
		}
	}
	role := hmmHistoryRole(res.PinRole)
	// The upsert only refreshes the row's TTL/turn_count/provider (ON CONFLICT
	// leaves the usage columns untouched), so it is always safe to run.
	s.upsertPin(strategyCtx, sessionpin.Pin{
		SessionKey:     res.SessionKey,
		Role:           role,
		InstallationID: res.InstallationID,
		Provider:       historyProvider,
		Reason:         hmmHistoryStoredReason(res),
		Strategy:       router.StrategyFromContext(strategyCtx),
		TurnCount:      1,
		PinnedUntil:    pinExpiry(hmmHistoryReason),
	})
	// Zero tokens means a failed/empty upstream turn — don't clobber prior
	// usage counters; the TTL-refreshing upsert above already ran.
	if !hasUsage {
		return
	}
	now := time.Now()
	if err := s.pinStore.UpdateUsage(context.Background(), res.SessionKey, role, sessionpin.Usage{
		Strategy:            router.StrategyFromContext(strategyCtx),
		InputTokens:         in,
		CachedReadTokens:    cacheRead,
		CachedWriteTokens:   cacheCreation,
		OutputTokens:        out,
		EndedAt:             now,
		ServedModel:         servedModel,
		ServedProvider:      servedProvider,
		PriorServedModel:    res.PriorServedModel,
		SessionEverSwitched: res.SessionEverSwitched,
	}); err != nil {
		observability.Get().Error("HMM switch-history writeback failed", "err", err)
	}
}

func hmmHistoryStoredReason(res turnLoopResult) string {
	if isHMMDecision(res.Fresh) {
		return res.Fresh.Reason
	}
	if isHMMDecision(res.Decision) {
		return res.Decision.Reason
	}
	return hmmHistoryReason
}

func (s *Service) policyOutcomeRoute(res turnLoopResult, decision router.Decision) (router.Decision, *router.RoutingMetadata, policy.OutcomeReporter, bool) {
	for _, routeDecision := range []router.Decision{decision, res.Fresh} {
		routeMetadata := routeDecision.Metadata
		if routeMetadata == nil || routeMetadata.Strategy == "" || routeMetadata.RouteID == "" {
			continue
		}
		registered, ok := s.strategies[router.Strategy(routeMetadata.Strategy)]
		if !ok || registered.outcomes == nil {
			continue
		}
		return routeDecision, routeMetadata, registered.outcomes, true
	}
	return router.Decision{}, nil, nil, false
}

func (s *Service) capturePolicyOutcomeResponse(ctx context.Context, w http.ResponseWriter, res turnLoopResult, decision router.Decision) (http.ResponseWriter, *captureWriter) {
	trainingAllowed, _ := ctx.Value(PolicyTrainingAllowedContextKey{}).(bool)
	if !trainingAllowed {
		return w, nil
	}
	if _, _, _, ok := s.policyOutcomeRoute(res, decision); !ok {
		return w, nil
	}
	capture := newCaptureWriter(w, policyOutcomeResponseMaxBytes)
	return capture, capture
}

func (s *Service) reportPolicyOutcome(ctx context.Context, res turnLoopResult, decision router.Decision, effort effortResolution, finalProvider string, servedFast bool, estimatedInputTokens, inputTokens, outputTokens, cacheCreation, cacheRead int, routeMs, proxyMs int64, proxyErr error, response *policyOutcomeResponse) {
	routeDecision, routeMetadata, reporter, ok := s.policyOutcomeRoute(res, decision)
	if !ok {
		return
	}
	organizationID, _ := ctx.Value(ExternalIDContextKey{}).(string)
	installationID, _ := ctx.Value(InstallationIDContextKey{}).(string)
	trainingAllowed, _ := ctx.Value(PolicyTrainingAllowedContextKey{}).(bool)
	clientIdentity := ClientIdentityFrom(ctx)
	selectedServedModelMatch := routeDecision.Model == decision.Model
	authoritativeModelMismatch := routeMetadata.AuthoritativePerTurnSelection &&
		!selectedServedModelMatch
	if authoritativeModelMismatch {
		trainingAllowed = false
		observability.FromContext(ctx).Error(
			"Authoritative policy model did not match served model",
			"route_id", routeMetadata.RouteID,
			"selected_model", routeDecision.Model,
			"served_model", decision.Model,
		)
	}
	// An effort-qualified arm is only a label for what was bought when the
	// arm's own level is the level that went on the wire; training on a clamped
	// or overridden turn credits the arm with another level's outcome.
	effortMismatch := effort.Mismatch()
	if effortMismatch {
		trainingAllowed = false
		observability.FromContext(ctx).Warn(
			"Selected effort did not match the effort sent upstream",
			"route_id", routeMetadata.RouteID,
			"served_model", decision.Model,
			"arm_effort", effort.Arm,
			"selected_effort", effort.Selected,
			"sent_effort", effort.Sent,
			"effort_source", effort.Source,
		)
	}
	payload := map[string]interface{}{
		"route_id":                         routeMetadata.RouteID,
		"strategy":                         routeMetadata.Strategy,
		"organization_id":                  organizationID,
		"installation_id":                  installationID,
		"client_app":                       clientIdentity.ClientApp,
		"rollout_id":                       policyRolloutIDFromContext(ctx),
		"training_allowed":                 trainingAllowed,
		"capture_mode":                     s.effectiveCaptureMode(ctx).String(),
		"policy_route_key":                 routeMetadata.PolicyRouteKey,
		"policy_artifact_id":               routeMetadata.PolicyArtifactID,
		"policy_artifact_sha256":           routeMetadata.PolicyArtifactSHA256,
		"roster_version":                   routeMetadata.RosterVersion,
		"sidecar_schema_version":           routeMetadata.SidecarSchemaVersion,
		"selected_model":                   routeDecision.Model,
		"selected_provider":                routeDecision.Provider,
		"served_model":                     decision.Model,
		"served_provider":                  finalProvider,
		"decision_model":                   routeDecision.Model,
		"decision_provider":                routeDecision.Provider,
		"selected_served_model_match":      selectedServedModelMatch,
		"arm_effort":                       effort.Arm,
		"selected_effort":                  effort.Selected,
		"sent_effort":                      effort.Sent,
		"effort_source":                    effort.Source,
		"selected_sent_effort_match":       !effortMismatch,
		"authoritative_per_turn_selection": routeMetadata.AuthoritativePerTurnSelection,
		"status":                           upstreamStatus(proxyErr),
		"error":                            "",
		"estimated_input_tokens":           estimatedInputTokens,
		"input_tokens":                     inputTokens,
		"output_tokens":                    outputTokens,
		"cache_creation_tokens":            cacheCreation,
		"cache_read_tokens":                cacheRead,
		"route_latency_ms":                 routeMs,
		"upstream_latency_ms":              proxyMs,
		"turn_type":                        string(res.TurnType),
		"sticky_hit":                       res.StickyHit,
	}
	switch {
	case authoritativeModelMismatch:
		payload["training_exclusion_reason"] = "selected_served_model_mismatch"
	case effortMismatch:
		payload["training_exclusion_reason"] = "selected_sent_effort_mismatch"
	}
	if trainingAllowed && response != nil {
		payload["response_body_truncated"] = response.Truncated
		if !response.Truncated {
			payload["response_text"] = translate.AnthropicClientResponseText(response.Body)
		}
	}
	if proxyErr != nil {
		payload["error"] = proxyErr.Error()
	}
	if price, ok := servedPricing(finalProvider, decision.Model, servedFast); ok {
		inputCost := catalog.EffectiveInputCost(inputTokens, cacheCreation, cacheRead, price.InputUSDPer1M, price, finalProvider)
		outputCost := catalog.EffectiveOutputCost(outputTokens, price.OutputUSDPer1M)
		payload["cost_usd"] = inputCost + outputCost
	}
	log := observability.FromContext(ctx).With("route_id", routeMetadata.RouteID)
	if err := ctx.Err(); err != nil {
		log.Debug("Skipping policy outcome report for canceled request", "err", err)
		return
	}
	observability.SafeGo(log, policyOutcomeReportTimeout, "reportPolicyOutcome", func(reportCtx context.Context) {
		if err := reporter.ReportOutcome(reportCtx, payload); err != nil {
			log.Error("Policy outcome report failed", "strategy", routeMetadata.Strategy, "err", err)
		}
	})
}

// pinDecision rehydrates a router.Decision from a stored pin. Metadata is nil
// (embedding isn't persisted, acceptable since the pin short-circuits routing).
func pinDecision(p sessionpin.Pin) router.Decision {
	return router.Decision{
		Provider: p.Provider,
		Model:    p.Model,
		Effort:   p.Effort,
		Reason:   p.Reason,
	}
}

// policyDeadlineDefaultDecision resolves ROUTER_POLICY_DEADLINE_DEFAULT_MODEL to a
// dispatchable Decision, honouring this turn's eligibility (enabled providers and
// excluded models). Reports false when unset, hard-excluded, or without a live binding.
func (s *Service) policyDeadlineDefaultDecision(req router.Request) (router.Decision, bool) {
	if s.policyDeadlineDefaultModel == "" {
		return router.Decision{}, false
	}
	if _, excluded := req.ExcludedModels[s.policyDeadlineDefaultModel]; excluded {
		return router.Decision{}, false
	}
	if _, excluded := req.SafetyExcludedModels[s.policyDeadlineDefaultModel]; excluded {
		return router.Decision{}, false
	}

	// The static deadline fallback is an automatic choice, but it is also the
	// last-resort response after policy has already failed. A soft exclusion may
	// not turn this degraded path into a 503; hard exclusions above still win.

	// nil EnabledProviders means unrestricted, so fall back to everything this
	// deployment registered; otherwise only providers this turn can authenticate.
	providerSet := make(map[string]struct{}, len(s.providers))
	for provider := range s.providers {
		if req.EnabledProviders != nil {
			if _, enabled := req.EnabledProviders[provider]; !enabled {
				continue
			}
		}
		providerSet[provider] = struct{}{}
	}
	binding, ok := catalog.ResolveBinding(s.policyDeadlineDefaultModel, providerSet)
	if !ok {
		return router.Decision{}, false
	}
	return router.Decision{
		Provider: binding.Provider,
		Model:    s.policyDeadlineDefaultModel,
		Reason:   policyDeadlineDefaultReason,
	}, true
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
		// Swapping to the paired member is an automatic choice, so a
		// deployment-wide disable rules it out even though the anchor stands.
		if _, disabled := s.globalAutomaticExcludedModels(ctx)[served.Model]; disabled {
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
	a := pinDecision(pin)
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
	// A BYOK gateway is the tenant's own endpoint and displaces every other
	// upstream: routing through a vendor would send their traffic outside the
	// gateway they mandated. Applied last so no enrollment path above can
	// re-admit a vendor, and their provider exclusions become moot.
	if gateways := s.gatewayProvidersForRequest(ctx); len(gateways) > 0 {
		return gateways
	}
	return out
}

// hasOpenAIInfrastructureCredential reports whether an OpenAI model can be
// served without the caller's ChatGPT OAuth: deployment key, installation
// BYOK, or a non-OAuth client credential on an unkeyed passthrough request.
func (s *Service) hasOpenAIInfrastructureCredential(ctx context.Context, headers http.Header) bool {
	if byokServedForProvider(ctx, providers.ProviderOpenAI) {
		return true
	}
	if !s.byokOnly {
		if s.deploymentKeyedProviders == nil {
			_, registered := s.providers[providers.ProviderOpenAI]
			if registered {
				return true
			}
		} else if _, keyed := s.deploymentKeyedProviders[providers.ProviderOpenAI]; keyed {
			return true
		}
	}
	if installationIDFromContext(ctx) == uuid.Nil {
		if client := ExtractClientCredentials(providers.ProviderOpenAI, headers); client != nil && !client.OAuth {
			return true
		}
	}
	return false
}

// excludeCodexOAuthOnlyModels keeps model eligibility aligned with credential
// resolution. When ChatGPT OAuth is the only way OpenAI became eligible (or
// billing has restricted the turn to subscriptions), only the exact native
// Codex family may select the OpenAI binding. Infrastructure-backed requests
// retain the full catalog and route other OpenAI models normally.
func (s *Service) excludeCodexOAuthOnlyModels(
	ctx context.Context,
	headers http.Header,
	enabledProviders map[string]struct{},
	excluded map[string]struct{},
) map[string]struct{} {
	codex, _ := presentSubscriptionTokens(ctx, headers)
	if codex == "" || (!billing.SubscriptionOnlyFromContext(ctx) && s.hasOpenAIInfrastructureCredential(ctx, headers)) {
		return excluded
	}
	for _, model := range catalog.Models {
		if codexSubscriptionCoversModel(model.ID) {
			continue
		}
		// Match catalog binding resolution: the first enabled binding is the one
		// this model would dispatch through. If that binding is OAuth-only OpenAI,
		// the whole model is ineligible for this request.
		for _, binding := range model.Providers {
			if enabledProviders != nil {
				if _, enabled := enabledProviders[binding.Provider]; !enabled {
					continue
				}
			}
			if binding.Provider == providers.ProviderOpenAI {
				excluded = excludingModel(excluded, model.ID)
			}
			break
		}
	}
	return excluded
}

// resolveAndInjectCredentials resolves credentials for the selected provider
// and model and stashes them on ctx. Claude OAuth applies to Anthropic models;
// Codex OAuth applies only to the explicit native Codex model family. All other
// selections fall through to BYOK, a client API key, or the deployment key.
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
func resolveAndInjectCredentials(ctx context.Context, provider, model string, headers http.Header) context.Context {
	routerKeyed := installationIDFromContext(ctx) != (uuid.UUID{})
	// Skip subscription OAuth (fall through to BYOK / deployment key):
	// exhausted (Anthropic-only, avoid re-429), toggle off (provider-wide), or
	// an OpenAI-provider model outside the native Codex OAuth family.
	subDisabled := subscriptionRoutingDisabledForRequest(ctx)
	suppressClaudeSub := claudeSubscriptionSuppressed(ctx) || subDisabled
	suppressCodexSub := subDisabled || !codexSubscriptionCoversModel(model)
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
	if provider == providers.ProviderOpenAI && !suppressCodexSub {
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
		// A suppressed subscription must not slip back in as the inbound OAuth
		// bearer off the router-key path, undoing the skip above.
		if client != nil && client.OAuth &&
			((provider == providers.ProviderAnthropic && suppressClaudeSub) ||
				(provider == providers.ProviderOpenAI && suppressCodexSub)) {
			client = nil
		}
		creds = client
	}
	if creds != nil {
		return context.WithValue(ctx, CredentialsContextKey{}, creds)
	}
	// Clear explicitly: router-keyed / no-BYOK ctx still carries the subscription credential from an earlier attempt;
	// provider client only falls back to the deployment key when ctx carries NO credential.
	if (suppressClaudeSub && provider == providers.ProviderAnthropic) ||
		(suppressCodexSub && provider == providers.ProviderOpenAI) {
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
		SubscriptionServed: routeRes.UsageBypass || servedOnSubscription(ctx),
		ByokServed:         servedOnBYOK(ctx),
		APIKeyID:           apiKeyID,
		RouterUserID:       auth.UserIDFrom(ctx),
	})

	// The handover summary runs on the deployment/BYOK key, never the subscription
	// token. If a BYOK key was used, that spend hit the customer's account —
	// so bill the fee rather than full cost.
	if routeRes.Handover.Invoked && !routeRes.Handover.FallbackToFullHistory {
		s.billAuxiliaryInference(ctx, requestID, auxSuffixHandoverSummary, externalID, routeRes.Handover.SummaryUsage)
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
		observability.FromContext(ctx).Debug("Billing debit skipped: no organization_id on request")
		return
	}
	dbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	balance, err := s.billing.DebitForInference(dbCtx, p)
	if err == nil {
		observability.FromContext(ctx).Debug("Billing debit complete",
			"organization_id", p.OrganizationID,
			"router_request_id", p.RouterRequestID,
			"model", p.Model,
			"balance_usd_micros", balance,
			"override", p.HasOverride,
			"subscription_served", p.SubscriptionServed,
			"byok_served", p.ByokServed,
		)
		return
	}
	logBillingDebitFailure(ctx, p, err)
}

// logBillingDebitFailure emits a structured Error log so on-call alerting can
// fire on the resulting log rate without a new prometheus dependency.
func logBillingDebitFailure(ctx context.Context, p billing.DebitInferenceParams, err error) {
	observability.FromContext(ctx).Error("router_billing_debit_failed",
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
		"byok_served", p.ByokServed,
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

// openAISurface names which OpenAI endpoint an attempt POSTs to and in what
// representation; the three cases differ in both emit and response handling.
type openAISurface int

const (
	// surfaceChat is /v1/chat/completions with the client's own format.
	surfaceChat openAISurface = iota
	// surfaceResponsesNative is /v1/responses with a Responses caller's
	// original bytes, streamed back verbatim.
	surfaceResponsesNative
	// surfaceResponsesTranslated is /v1/responses emitted from a
	// chat/completions request, with the response translated back to chat.
	surfaceResponsesTranslated
)

// ProxyOpenAIChatCompletion routes an OpenAI Chat Completion request,
// translating cross-format when the decision picks a non-OpenAI provider.
func (s *Service) ProxyOpenAIChatCompletion(ctx context.Context, body []byte, w http.ResponseWriter, r *http.Request) error {
	if managedSubscriptionEnrollmentUnavailable(ctx) {
		return ErrSubscriptionPoolUnavailable
	}
	ctx, err := s.checkUserMonthlySpendLimit(ctx, r.Header, r.URL.Path)
	if err != nil {
		return err
	}
	ctx = s.withUsageObserver(ctx, r.Header, routePathChatCompletions)
	log := observability.FromContext(ctx)
	requestStart := time.Now()
	requestID := requestIDFor(ctx)
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
	// cleanup, matching the Anthropic Messages path. The echo check must read
	// the body before the strip erases its evidence.
	footerEchoedSinceHumanTurn := translate.FeedbackFooterSinceLastHumanTurn(body)
	if echoed, _ := ctx.Value(responsesFooterEchoedContextKey{}).(bool); echoed {
		footerEchoedSinceHumanTurn = true
	}
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
	var responseBuffer *responseCostBuffer
	_, isResponsesWriter := w.(*translate.ResponsesWriter)
	if !env.Stream() && !isResponsesWriter {
		responseBuffer = newResponseCostBuffer(w)
		w = responseBuffer
		defer func() {
			if flushErr := responseBuffer.FlushToClient(); flushErr != nil {
				log.Error("Failed to flush buffered response", "err", flushErr)
			}
		}()
	}

	// Bind session-scoped logger before stripping router-only history; see the
	// matching Anthropic block for why the raw client session shape owns pins.
	var sessionKey [sessionpin.SessionKeyLen]byte
	ctx, log, sessionKey = bindRequestLogger(ctx, env, apiKeyID, requestID, "openai_chat_completions")
	if removed := env.StripRouterFeedbackArtifacts(); removed > 0 {
		log.Info("Stripped router-feedback artifacts from OpenAI history", "removed_messages", removed)
	}
	if removed := env.StripBetaArtifacts(); removed > 0 {
		ctx = withBetaArtifactHistory(ctx)
		log.Info("Stripped beta artifacts from OpenAI history", "removed_messages", removed)
	}
	embedFlag := s.ResolveEmbedOnlyUserMessage(ctx)
	feats := env.RoutingFeatures(embedFlag)
	promptText := feats.PromptText
	embedInput := "concatenated_stream"
	if embedFlag && feats.OnlyUserMessageText != "" {
		promptText = feats.OnlyUserMessageText
		embedInput = "only_user_message"
	}

	bypassEval := hasEvalOverrideHeader(r)

	log.Info("ProxyOpenAIChatCompletion start",
		"requested_model", feats.Model,
		"stream", env.Stream(),
		"message_count", feats.MessageCount,
		"has_tools", feats.HasTools,
		"total_input_tokens", feats.Tokens,
		"prompt_preview", observability.Preview(promptText, 200),
	)

	// /beta toggle: handled server-side before other routing commands; no post-command continuation.
	if cmd, hasCmd := env.ExtractBetaCommand(); hasCmd {
		log.Info("ProxyOpenAIChatCompletion beta command")
		return s.handleBetaCommand(ctx, w, env, cmd, installationID, sessionKey, feats.Tokens)
	}
	ctx, err = s.applySessionStrategy(ctx, installationID, sessionKey)
	if err != nil {
		return err
	}
	*r = *r.WithContext(ctx)
	forceModelSessionKey := deriveForceModelSessionKeyForRequest(ctx, env, apiKeyID, sessionKey)

	// Handle /force-model and /unforce-model before routing (stripped from
	// env.body so the upstream never sees it). Session key is derived before
	// extraction: DeriveSessionKey can fall back to prompt text, and deriving
	// after the strip would mismatch subsequent turns with the unstripped message.
	agentForceModel := ""
	requestBodyChanged := false
	if s.pinStore != nil {
		if cmd, hasCmd := env.ExtractForceModelCommand(); hasCmd {
			log.Info("ProxyOpenAIChatCompletion force-model command", "force_model_cmd", cmd)
			if cmd.FromToolResult {
				var err error
				agentForceModel, _, err = s.applyForceModelCommand(ctx, env, cmd, installationID, sessionKey, forceModelSessionKey)
				if err != nil {
					return err
				}
				requestBodyChanged = true
			} else {
				if err := s.handleForceModelCommand(ctx, w, env, cmd, installationID, sessionKey, forceModelSessionKey, feats.Tokens); err != nil {
					return err
				}
				s.grantPostCommandContinuation(ctx, installationID, sessionKey, roleForTier(catalog.TierFor(feats.Model)))
				return nil
			}
		}
	}
	if cmd, hasCmd := env.ExtractRouterFeedbackCommand(); hasCmd {
		log.Info("ProxyOpenAIChatCompletion router-feedback command")
		skillFeedback, _ := ctx.Value(codexFeedbackSkillContextKey{}).(bool)
		synthetic := !cmd.FromToolResult || skillFeedback
		if err := s.handleRouterFeedbackCommand(ctx, w, env, cmd, installationID, sessionKey, feats.Tokens, synthetic); err != nil {
			return err
		}
		if synthetic {
			s.grantPostCommandContinuation(ctx, installationID, sessionKey, roleForTier(catalog.TierFor(feats.Model)))
			return nil
		}
		requestBodyChanged = true
	}

	// Sanitize after command extraction: a skill can encode its command as a
	// plain user string after an assistant tool_use, and sanitizing first would
	// erase the provenance and leave a dangling tool_use that 400s on Together.
	if sanitized := env.SanitizeOrphanedToolCalls(); sanitized > 0 {
		log.Info("Sanitized orphaned tool calls before dispatch", "sanitized", sanitized)
		requestBodyChanged = true
	}
	if requestBodyChanged {
		feats = env.RoutingFeatures(embedFlag)
		promptText = feats.PromptText
		embedInput = "concatenated_stream"
		if embedFlag && feats.OnlyUserMessageText != "" {
			promptText = feats.OnlyUserMessageText
			embedInput = "only_user_message"
		}
	}

	// Honor the x-weave-force-model header (headless equivalent of /force-model).
	// Writes the user-forced pin and falls through to normal routing, which picks
	// the pin up and serves the requested model on this same turn.
	forceModel := agentForceModel
	ctx, headerForceModel, forceErr := s.applyForceModelHeader(ctx, r, installationID, forceModelSessionKey)
	if forceErr != nil {
		return forceErr
	}
	if headerForceModel != "" {
		forceModel = headerForceModel
	}
	forceCluster, forceErr := applyForceClusterHeader(ctx, r)
	if forceErr != nil {
		return forceErr
	}

	// Wide cyclic re-read loop → escalate to opus (same path as the Anthropic
	// ingress). See detectCyclicToolCallLoop / handleLoopEscalation.
	if cyc, csig, ccount, cratio, cwin := detectCyclicToolCallLoop(env); cyc {
		loopRole := roleForTier(catalog.TierFor(feats.Model))
		s.handleLoopEscalation(ctx, csig, ccount, cratio, cwin, installationID, sessionKey, loopRole, feats.Model, forceModelSessionKey)
	}

	logInboundRequestDiagnostics(log, env)

	// OpenAI signals sub-agent identity via x-weave-subagent-type (no metadata.user_id).
	subAgentHint := r.Header.Get("x-weave-subagent-type")

	enabledProviders := s.enabledProvidersForRequest(ctx, providers.ProviderOpenAI, r.Header)

	// Subscription-only mode: restrict
	// routing to the providers the caller's own subscription can serve, so the
	// scorer can't pick a paid model. Mirrors the Anthropic path's forced
	// usage-bypass; the post-routing guard below refuses if it still can't serve
	// on the subscription.
	if billing.SubscriptionOnlyFromContext(ctx) {
		enabledProviders = restrictToSubscriptionProviders(ctx, r.Header, enabledProviders)
	}

	// Codex (ChatGPT) subscription passthrough: ProxyOpenAIResponses stashed the
	// caller's original Responses body. Such turns skip the routing marker +
	// semantic cache below, and dispatch the verbatim body to the Codex
	// backend when routed to an OpenAI model (see responsesPassthrough branch).
	//
	// Deliberately not forcing OpenAI-only routing: enabledProviders already
	// scopes to providers the caller can pay for, so a dual Codex+Claude
	// subscription routes freely across both, each billing its own plan.
	// Subscriptions are credentials scoped to the routed model, not a pinned
	// provider.
	responsesBody, _ := ctx.Value(codexResponsesBodyContextKey{}).([]byte)
	responsesPassthrough := len(responsesBody) > 0
	reasoningConfigurationHash := env.ReasoningConfigurationSHA256()
	if nativeResponsesHash, ok := ctx.Value(nativeResponsesReasoningHashContextKey{}).(string); ok {
		reasoningConfigurationHash = nativeResponsesHash
	}
	toolConfigurationHash := env.ToolConfigurationSHA256()
	if nativeResponsesHash, ok := ctx.Value(nativeResponsesToolHashContextKey{}).(string); ok {
		toolConfigurationHash = nativeResponsesHash
	}

	// Pre-filter models whose context window cannot fit this request.
	outputReserveOAI := contextWindowOutputReserve
	if feats.MaxTokens > outputReserveOAI {
		outputReserveOAI = feats.MaxTokens
	}
	baseExcludedOAI := s.excludeCodexOAuthOnlyModels(ctx, r.Header, enabledProviders, s.excludedModelsForRequest(ctx))

	// Snapshot the inbound tool-output size before any env rewrite (proactive
	// compaction below, or runTurnLoop's switch handover); see toolResultBytesPtr.
	inboundLastUser := env.LastUserMessage()

	// Proactive context-window compaction, as in ProxyMessages. Skipped for
	// Codex passthrough bodies, which are forwarded verbatim.
	var compResOAI compactionResult
	if !responsesPassthrough {
		maxEligibleWindowOAI := s.maxEligibleContextWindow(baseExcludedOAI, enabledProviders, env.SignatureTokenSavings())
		var compErrOAI error
		compResOAI, compErrOAI = s.maybeCompact(ctx, env, compactionInput{
			TurnType:       turntype.DetectFromEnvelope(env, feats, subAgentHint),
			OutputReserve:  outputReserveOAI,
			MaxWindow:      maxEligibleWindowOAI,
			RequestedModel: feats.Model,
			ClientApp:      ClientIdentityFrom(ctx).ClientApp,
			PreferredSummarizer: func() string {
				return s.compactionPreferredSummarizer(ctx, sessionKey, roleForTier(catalog.TierFor(feats.Model)))
			},
			Headers: r.Header,
		})
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
	excludedOAI, ctxOverflowedOAI := excludeContextOverflowModels(overflowEstimateOAI, env.SignatureTokenSavings(), outputReserveOAI, enabledProviders, baseExcludedOAI, s.availableModels)
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

	routeRequest := router.Request{
		RequestedModel:               feats.Model,
		ForceModel:                   forceModel,
		ForceCluster:                 forceCluster,
		EstimatedInputTokens:         feats.Tokens,
		HasTools:                     feats.HasTools,
		HasImages:                    feats.HasImages,
		TranslationRequirements:      env.TranslationRequirements(router.EndpointOpenAIChat),
		ReasoningConfigurationSHA256: reasoningConfigurationHash,
		ToolConfigurationSHA256:      toolConfigurationHash,
		PromptText:                   promptText,
		ConversationMessages:         conversationMessagesForRouting(env),
		AvailableTools:               availableToolsForRouting(env),
		Tools:                        toolsForRouting(env),
		HistoryTruncated:             compResOAI.Applied,
		// Keep this tied to client-visible history so a later feedback command
		// can correlate with the route even if local compaction rewrites env.
		FeedbackKey:          hex.EncodeToString(sessionKey[:]),
		FeedbackRole:         roleForTier(catalog.TierFor(feats.Model)),
		ClientSessionID:      clientSessionIDForRequest(ctx, env),
		EnabledProviders:     enabledProviders,
		CustomBindings:       s.customBindingsForRequest(ctx),
		GatewayProviders:     s.gatewayProvidersForRequest(ctx),
		ExcludedModels:       excludedOAI,
		AllowedModels:        allowedModelsForRequest(ctx),
		SafetyExcludedModels: s.safetyExcludedModels(env, outputReserveOAI, enabledProviders),
		PreferredModels:      s.preferredModelsForRequest(ctx),
		RoutingKnobs:         routingKnobsForRequest(ctx),
		ClusterArmOverrides:  clusterArmOverridesForRequest(ctx),
	}
	routeStart := time.Now()
	routeCtx, routeSpan := startRoutingSpan(ctx, routeRequest)
	routeRes, err := s.runTurnLoop(routeCtx, env, feats, apiKeyID, installationID, subAgentHint, r.Header, routeRequest)
	finishRoutingSpan(routeSpan, routeRes.Decision, err)
	routeMs := time.Since(routeStart).Milliseconds()
	if err != nil {
		log.Error("Routing failed for OpenAI request", "err", err, "route_ms", routeMs, "requested_model", feats.Model, "total_input_tokens", feats.Tokens)
		return err
	}
	if len(routeRes.SessionDisabledProviders) > 0 {
		ctx = context.WithValue(ctx, SessionDisabledProvidersContextKey{}, routeRes.SessionDisabledProviders)
	}
	routeRes.SuggestionMode = r.Header.Get("x-weave-suggestion-mode") == "true"
	decision := routeRes.Decision
	s.firePolicyShadowForServingDecision(ctx, decision, routeRequest)
	tt := routeRes.TurnType
	stickyHit := routeRes.StickyHit
	pinTier := routeRes.PinTier
	pinAgeSec := routeRes.PinAgeSec
	s.logPlannerOutcome(ctx, routeRes)

	// See the ProxyMessages cache-eligibility note: subsidized, subscription-state-
	// conditional, and plan-aware requests bypass the semantic cache because the
	// key does not capture headroom-dependent model eligibility.
	cacheEligible := s.semanticCacheAllowed(ctx) && s.semanticCache != nil && !env.Stream() && decision.Metadata != nil && externalID != "" && !bypassEval && !responsesPassthrough && !billing.SubscriptionOnlyFromContext(ctx) && len(s.subsidyFactors(ctx, r.Header)) == 0 && !subscriptionConditionalModelsConfigured(ctx) && len(subscriptionPlanAwareExcludedModelsFromContext(ctx)) == 0 && !requestAllowedModelsPresent(ctx)
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
	w.Header().Set(HeaderRouterContextWindow, strconv.Itoa(contextWindowForRequest(decision.Model, decision.Provider)))
	s.setFeedbackLinkHeader(ctx, w, installationID, externalID, requestID, auth.UserIDFrom(ctx))

	reqPricing := otel.Lookup(s.baselineFor(feats.Model))
	actPricing := otel.Lookup(decision.Model)
	openaiDecisionBuilder := otel.NewAttrBuilder(45).
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
		Bool("routing.policy_fallback", routeRes.PolicyFallback).
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
	applySidecarAttrs(openaiDecisionBuilder, routeRes)
	applyPlannerAttrs(openaiDecisionBuilder, routeRes)
	applyRoutingStateAttrs(openaiDecisionBuilder, routeRes, decision.ServedIdentity(), sessionKey)
	otel.Record(ctx, otel.Span{
		Name:  "router.decision",
		Start: requestStart,
		End:   time.Now(),
		Attrs: openaiDecisionBuilder.Build(),
	})
	otel.Flush(ctx)

	opts := translate.EmitOptions{
		TargetModel:              decision.Model,
		TargetProvider:           decision.Provider,
		Capabilities:             router.Lookup(decision.Model),
		IncludeStreamUsage:       s.usageRequired(),
		SessionAffinity:          sessionAffinityHint(routeRes.SessionKey),
		EnableServerSideFallback: s.ResolveAnthropicServerSideFallback(ctx),
		ModelSwitched:            routeRes.modelSwitched(),
		EnableExtendedContext:    shouldEnableExtendedContext(env.FullTokenEstimate(), outputReserveOAI),
	}
	effortServed := s.resolveEffort(ctx, decision, opts.Capabilities, routeRes.EscalateEffort)
	effortServed.apply(&opts)

	ctx = resolveAndInjectCredentials(ctx, decision.Provider, decision.Model, r.Header)
	opts.FastMode = fastModeForAttempt(ctx, decision.Model, decision.Provider)
	// fastServed tracks whether the most recent attempt went out on the fast
	// tier so post-dispatch billing prices the winning attempt.
	fastServed := false

	// See ProxyMessages for the preludeBuffer rationale — wrap unconditionally
	// so single-binding upstream errors don't strand the routing-marker chunk
	// on the wire when the upstream never produces a first byte.
	bindings := s.resolveBindingsForDispatch(ctx, decision)

	// Subscription-only mode: the turn must serve on the caller's own
	// subscription (Codex/Claude OAuth). If routing didn't resolve to a
	// subscription-served credential, refuse (402) rather than dispatch to a
	// paid model against an already-negative balance. When it did, pin dispatch
	// to that single binding so failover can't reroute onto a paid provider.
	if billing.SubscriptionOnlyFromContext(ctx) {
		if !servedOnSubscription(ctx) && !managedSubscriptionCanServe(ctx, decision.Provider, decision.Model) {
			log.Info("Subscription-only request cannot be served on the subscription; refusing",
				"requested_model", feats.Model, "external_id", externalID, "decision_provider", decision.Provider)
			return ErrCreditsExhaustedSubscriptionUnavailable
		}
		bindings = []catalog.ProviderBinding{{Provider: decision.Provider}}
	}
	// Append the one-click feedback thumbs as a trailing chunk (see
	// ProxyMessages). Skipped on the Responses-API path (w is a
	// *ResponsesWriter): wrapping it would defeat maybeCaptureResponse's
	// special-casing — /v1/responses footers are a follow-up.
	clientSink := w
	if _, isResponses := w.(*translate.ResponsesWriter); env.Stream() && !isResponses {
		if footer := s.feedbackFooter(ctx, ClientIdentityFrom(ctx).ClientApp, routeRes.TurnType, footerEchoedSinceHumanTurn); footer != "" {
			clientSink = translate.NewOpenAIRoutingFooterWriter(w, footer)
		}
	}
	contentSink, contentCap := s.maybeCaptureResponse(ctx, clientSink)
	preludeBuf := newPreludeBuffer(contentSink)
	var rootSink http.ResponseWriter = preludeBuf

	marker := suppressMarkerIfRequested(ctx, r.Header, routingMarkerFor(routeRes))
	if billing.SubscriptionOnlyFromContext(ctx) {
		// Always surface the depleted-credits warning (not gated by the
		// routing-marker opt-out): a billing state change the caller must see.
		marker = subscriptionOnlyWarningMarkerCodex
	}

	// gpt-5.6 applies its own effort on chat/completions, so a /v1/responses
	// caller's original bytes serve it natively — preserving reasoning the chat
	// projection drops. Skip when compaction or a handover rewrote the envelope
	// (stale bytes); pre-routing readers of responsesPassthrough already ran.
	responsesEndpointKey := EffectiveBaseURL(ctx, decision.Provider)
	promotedToResponses := false
	if !responsesPassthrough && !compResOAI.Applied && !routeRes.Handover.Invoked &&
		decision.Provider == providers.ProviderOpenAI &&
		translate.UseOpenAIResponsesAPI(translate.ResponsesRoute{
			Provider:       decision.Provider,
			Capabilities:   opts.Capabilities,
			HasTools:       feats.HasTools,
			ChatOnlyParams: env.RequiresChatCompletionsParams(opts.Capabilities),
			Broad:          s.ResolveOpenAIResponsesBroad(ctx),
		}) &&
		!s.gatewayLacksResponses(responsesEndpointKey) {
		if native, ok := ctx.Value(nativeResponsesBodyContextKey{}).([]byte); ok && len(native) > 0 {
			responsesBody = native
			responsesPassthrough = true
			promotedToResponses = true
		}
	}

	// Previously gated on policy debug; ordinary Codex turns fell through to
	// ResponsesWriter's legacy badge that ignored suppression and never showed the routing reason.
	verbatimPassthrough := responsesPassthrough && decision.Provider == providers.ProviderOpenAI
	if rw, ok := w.(*translate.ResponsesWriter); ok {
		if marker != "" && !verbatimPassthrough {
			rw.SetBadgeText(marker)
		}
		if footer := s.feedbackFooter(ctx, clientID.ClientApp, routeRes.TurnType, footerEchoedSinceHumanTurn); footer != "" {
			rw.SetFooterText(footer)
		}
	}
	// Keep a stable copy for a possible chat/completions fallback after a native
	// Responses endpoint rejects the request.
	translatedMarker := marker

	// Responses entry point delegates the eager response.created emit to
	// this layer because it has the post-routing binding count. Fire only
	// when single-binding so multi-binding requests stay failover-safe
	// (Codex client sees response.created via ResponsesWriter's lazy
	// emitCreated on the first upstream byte instead).
	if rw, ok := w.(*translate.ResponsesWriter); ok {
		// A native OpenAI Responses route streams the original Responses
		// bytes verbatim; cross-family routes stay in translation mode.
		//
		// Set once here (before Prelude), not per-attempt: response.created
		// suppression depends on passthrough being engaged before the first
		// write. Safe because decision.Provider == OpenAI is always a
		// single-binding GPT model with no cross-format fallback to retry
		// into. If a GPT model ever gains a fallback, gate this per-attempt.
		if verbatimPassthrough {
			// marker already carries the depleted-credits warning in
			// subscription-only mode, which overrides the opt-out above.
			// Parse native SSE when Codex needs a badge and/or footer.
			if clientID.ClientApp == ClientAppCodex && (marker != "" || s.feedbackFooter(ctx, clientID.ClientApp, routeRes.TurnType, footerEchoedSinceHumanTurn) != "") {
				if marker != "" {
					rw.SetBadgeText(marker)
				}
				rw.SetPassthroughBadge()
			} else {
				rw.SetPassthrough()
			}
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
		if isResponses || verbatimPassthrough {
			return sink
		}
		mw := translate.NewOpenAIRoutingMarkerWriter(sink, decision.Model, marker)
		if err := mw.Prelude(env.Stream()); err != nil {
			log.Error("OpenAI routing-marker prelude failed", "err", err)
		}
		return mw
	}

	// Chat caller: emit onto Responses and translate back; skipped for Responses-ingress (handled above).
	translateToResponses := !isResponses && !responsesPassthrough &&
		decision.Provider == providers.ProviderOpenAI &&
		translate.UseOpenAIResponsesAPI(translate.ResponsesRoute{
			Provider:       decision.Provider,
			Capabilities:   opts.Capabilities,
			HasTools:       feats.HasTools,
			ChatOnlyParams: env.RequiresChatCompletionsParams(opts.Capabilities),
			Broad:          s.ResolveOpenAIResponsesBroad(ctx),
		}) &&
		!s.gatewayLacksResponses(responsesEndpointKey)
	// nil when the request has no tools; the translator treats nil as syntax-check-only.
	toolValidator := env.ToolValidator()

	proxyStart := time.Now()
	inferenceParentCtx := ctx
	ctx, inferenceSpan := startInferenceSpan(ctx, decision)
	defer inferenceSpan.End()
	var proxyErr error
	crossFormat := false
	var extractor *otel.UsageExtractor

	var attempt dispatchAttempt
	// Overwritten per attempt, so it holds the winning attempt's signals.
	var respSummary translate.ResponseSummary
	// Dispatch keys off the provider's translation family, not a hardcoded name
	// list, so a new OpenAI-compat provider routes here as soon as it has a
	// ProviderFamilies entry (see internal/providers/provider.go).
	switch providers.FamilyFor(decision.Provider) {
	case providers.FamilyOpenAICompat:
		// Prep rebuilt per attempt: targetIsOpenRouter(opts) gates four
		// OpenRouter-only body fields that Fireworks/Bedrock/Makora/Together
		// should not see. On failover to OpenRouter the body must be re-emitted.
		// Split from attempt so a native dispatch that finds no Responses surface
		// can re-emit onto chat/completions while still pre-commit.
		dispatchOpenAI := func(actx context.Context, d router.Decision, p providers.Client, surface openAISurface, stripPromptCacheKey bool) error {
			var prep providers.PreparedRequest
			switch surface {
			case surfaceResponsesNative:
				// Dispatch the caller's ORIGINAL Responses body (untranslated) to
				// the OpenAI Responses endpoint, rewriting only the model. This keeps
				// native Responses extensions lossless.
				outBody, setErr := sjson.SetBytes(responsesBody, "model", d.Model)
				if setErr != nil {
					log.Error("Failed to set routed model on Codex Responses body", "err", setErr, "decision_model", d.Model)
					return fmt.Errorf("set codex model: %w", setErr)
				}
				nativeOpts := opts
				nativeOpts.TargetProvider = d.Provider
				nativeOpts.FastMode = fastModeForAttempt(actx, d.Model, d.Provider)
				fastServed = nativeOpts.FastMode
				outBody, setErr = translate.ApplyOpenAIFastMode(outBody, nativeOpts)
				if setErr != nil {
					return fmt.Errorf("set codex service_tier: %w", setErr)
				}
				// The caller's own effort would otherwise serve an effort-qualified
				// arm, so the policy learns from a level it never bought.
				outBody, setErr = translate.ApplyOpenAIResponsesEffort(outBody, nativeOpts)
				if setErr != nil {
					return fmt.Errorf("set codex reasoning effort: %w", setErr)
				}
				prep = providers.PreparedRequest{
					Body:     outBody,
					Endpoint: providers.EndpointResponses,
					Headers:  make(http.Header),
					Stats: providers.RequestMutationStats{
						Transformations: responseTransformationsFromContext(actx),
					},
				}
			default:
				attemptOpts := opts
				attemptOpts.TargetProvider = d.Provider
				attemptOpts.StripPromptCacheKey = stripPromptCacheKey
				attemptOpts.FastMode = fastModeForAttempt(actx, d.Model, d.Provider)
				fastServed = attemptOpts.FastMode
				var emitErr error
				if surface == surfaceResponsesTranslated {
					prep, emitErr = env.PrepareOpenAIResponses(r.Header, attemptOpts)
				} else {
					prep, emitErr = env.PrepareOpenAI(r.Header, attemptOpts)
				}
				if emitErr != nil {
					log.Error("Failed to emit OpenAI body", "err", emitErr,
						"decision_provider", d.Provider, "endpoint", prep.Endpoint)
					return fmt.Errorf("emit body: %w", emitErr)
				}
			}
			attemptSink := makeMarkerSink()
			proxyWriter := attemptSink
			// A translated attempt reads Responses SSE, which the chat-shaped
			// usage extractor can't parse — the translator records usage instead.
			var translator *translate.ResponsesToOpenAIChatWriter
			// A native attempt has no translator, so its terminal Responses event
			// is the only source for the turn's finish reason.
			var nativeTerminal *responsesTerminalObserver
			switch {
			case surface == surfaceResponsesTranslated:
				var usage otel.UsageSink
				if s.usageRequired() {
					extractor = otel.NewUsageExtractor(nil, d.Provider)
					usage = extractor
				}
				translator = translate.NewResponsesToOpenAIChatWriter(attemptSink, d.Model, usage).
					WithLogger(log).
					WithToolValidator(toolValidator)
				if err := translator.Prelude(env.Stream()); err != nil {
					log.Error("chat/completions prelude failed (Responses upstream)", "err", err)
				}
				proxyWriter = translator
			case surface == surfaceResponsesNative:
				nativeTerminal = newResponsesTerminalObserver(attemptSink)
				proxyWriter = nativeTerminal
				if s.usageRequired() {
					extractor = otel.NewUsageExtractor(nativeTerminal, d.Provider)
					proxyWriter = extractor
				}
			case s.usageRequired():
				extractor = otel.NewUsageExtractor(attemptSink, d.Provider)
				proxyWriter = extractor
			}
			if preludeBuf != nil {
				preludeBuf.Seal()
			}
			err := p.Proxy(actx, d, prep, proxyWriter, r)
			// Post-commit: bytes already on the wire, render as an in-stream
			// frame instead of a corrupting envelope (pre-commit goes through
			// dispatchWithFallback). Gate on THIS attempt being native: a non-native
			// request through the translating ResponsesWriter still needs its own
			// error frame; a native attempt already delivered the upstream's.
			if err != nil && surface != surfaceResponsesNative && env.Stream() && preludeBuf.Committed() {
				err = emitOpenAISSEErrorEvent(sink, err)
			}
			if translator != nil {
				finalErr := finalizeAfterProxy(err, translator.Finalize)
				respSummary = translator.Summary()
				return finalErr
			}
			if nativeTerminal != nil {
				nativeTerminal.Finalize()
				respSummary = translate.ResponseSummary{UpstreamFinishReason: nativeTerminal.finishReason}
			}
			return err
		}
		attempt = func(actx context.Context, d router.Decision, p providers.Client) error {
			surface := surfaceChat
			if d.Provider == providers.ProviderOpenAI {
				switch {
				case responsesPassthrough:
					surface = surfaceResponsesNative
				case translateToResponses:
					surface = surfaceResponsesTranslated
				}
			}
			gatewayKey := gatewayResponsesKey(actx, d.Provider)
			stripPCK := s.gatewayRejectsPromptCacheKey(gatewayKey)
			err := dispatchOpenAI(actx, d, p, surface, stripPCK)
			// Same prompt_cache_key unknown-field class as ProxyMessages' OpenAI-compat
			// path: re-emit once without the hint while pre-commit; memoize the endpoint.
			if err != nil && !stripPCK && gatewayKey != "" && !committed(preludeBuf) &&
				providers.IsUpstreamPromptCacheKeyRejection(err) {
				s.rememberGatewayRejectsPromptCacheKey(gatewayKey)
				stripPCK = true
				log.Warn("Gateway rejected prompt_cache_key; retrying without the affinity hint",
					"model", d.Model,
					"decision_provider", d.Provider,
					"request_id", requestID)
				if preludeBuf != nil {
					preludeBuf.Discard()
				}
				err = dispatchOpenAI(actx, d, p, surface, true)
			}
			// Retried once pre-commit on chat/completions; memoized for later turns.
			// A native attempt also needs promotedToResponses — a Codex passthrough has none.
			if err == nil || surface == surfaceChat ||
				committed(preludeBuf) || !providers.IsUpstreamResponsesUnsupported(err) {
				return err
			}
			if surface == surfaceResponsesNative {
				rw, ok := w.(*translate.ResponsesWriter)
				if !promotedToResponses || !ok || !rw.ClearPassthrough() {
					return err
				}
				responsesPassthrough = false
				if translatedMarker != "" {
					rw.SetBadgeText(translatedMarker)
				}
			}
			translateToResponses = false
			s.rememberGatewayLacksResponses(responsesEndpointKey)
			log.Warn("OpenAI endpoint rejected the Responses API; retrying on chat/completions",
				"model", d.Model,
				"decision_provider", d.Provider,
				"request_id", requestID)
			if preludeBuf != nil {
				preludeBuf.Discard()
			}
			return dispatchOpenAI(actx, d, p, surfaceChat, stripPCK)
		}
	case providers.FamilyGemini:
		crossFormat = true
		prep, emitErr := env.PrepareGemini(r.Header, opts)
		if emitErr != nil {
			log.Error("Failed to translate OpenAI request to Gemini format", "err", emitErr)
			proxyErr = fmt.Errorf("translate openai request to gemini: %w", emitErr)
			finishInferenceSpan(inferenceSpan, decision, decision.Provider, -1, proxyErr)
			return proxyErr
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
			proxyErr = fmt.Errorf("translate openai request: %w", emitErr)
			finishInferenceSpan(inferenceSpan, decision, decision.Provider, -1, proxyErr)
			return proxyErr
		}
		// One send on the given tier, split into the raw upstream error plus a
		// finalize thunk so a fast send refused for lack of fast-mode allocation
		// can be re-sent at standard speed before finalize commits the prelude.
		dispatchAnthropic := func(actx context.Context, d router.Decision, p providers.Client, fast bool) (error, func(error) error) {
			fastServed = fast
			attemptPrep := prep
			if fast != opts.FastMode {
				attemptOpts := opts
				attemptOpts.TargetProvider = d.Provider
				attemptOpts.FastMode = fast
				var attemptEmitErr error
				attemptPrep, attemptEmitErr = env.PrepareAnthropic(r.Header, attemptOpts)
				if attemptEmitErr != nil {
					log.Error("Failed to re-translate OpenAI request to Anthropic format for fast-tier change", "err", attemptEmitErr)
					return fmt.Errorf("translate openai request: %w", attemptEmitErr), func(err error) error { return err }
				}
			}
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
			rawErr := p.Proxy(actx, d, attemptPrep, translator, r)
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
			fast := fastModeForAttempt(actx, d.Model, d.Provider)
			rawErr, finalize := dispatchAnthropic(actx, d, p, fast)
			if rawErr != nil && fast && !committed(preludeBuf) && providers.IsAnthropicFastModeQuotaRejection(rawErr) {
				log.Warn("Retrying Anthropic request at standard speed after fast-mode quota rejection",
					"model", d.Model,
					"request_id", requestID)
				if preludeBuf != nil {
					preludeBuf.Discard()
				}
				rawErr, finalize = dispatchAnthropic(actx, d, p, false)
			}
			return finalize(rawErr)
		}
	default:
		proxyErr = fmt.Errorf("%w: %s (no translation path defined)", ErrProviderNotConfigured, decision.Provider)
		finishInferenceSpan(inferenceSpan, decision, decision.Provider, -1, proxyErr)
		return proxyErr
	}

	primaryProvider := decision.Provider
	primaryModel := decision.Model
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
	ctx = resolveAndInjectCredentials(ctx, finalProvider, decision.Model, r.Header)

	// Re-resolve pricing for the binding that actually served (see ProxyMessages).
	if actBindingPricing, ok := servedPricing(finalProvider, decision.Model, fastServed); ok {
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
	finishInferenceSpan(inferenceSpan, decision, finalProvider, winnerIdx, proxyErr)
	ctx = restoreParentSpan(ctx, inferenceParentCtx)

	in, out := extractor.Tokens()
	cacheCreation, cacheRead := extractor.CacheTokens()
	if !env.Stream() && proxyErr == nil {
		setRouterCostHeaders(w.Header(), routerResponseCostFromPricing(actPricing, decision.Provider, in, out, cacheCreation, cacheRead))
	}
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
		Bool("cost.fast_mode", fastServed).
		Int64("latency.upstream_ms", proxyMs).
		Int64("latency.total_ms", time.Since(requestStart).Milliseconds()).
		Int64("upstream.status_code", int64(upstreamStatus(proxyErr))).
		Bool("routing.cross_format", crossFormat).
		String("dispatch.primary_provider", primaryProvider).
		String("dispatch.primary_model", primaryModel).
		String("dispatch.final_provider", finalProvider).
		Int64("dispatch.fallback_attempts", int64(winnerIdx)).
		Bool("dispatch.failover_used", finalProvider != primaryProvider)
	applyPlannerAttrs(openaiUpstreamBuilder, routeRes)
	applyRoutingStateAttrs(openaiUpstreamBuilder, routeRes, decision.ServedIdentity(), sessionKey)
	applyEffortAttrs(openaiUpstreamBuilder, effortServed)
	addTimingAttrs(ctx, openaiUpstreamBuilder)

	openaiObs := buildObservationContext(ctx, decision, routeRes.Fresh, s.effectiveCaptureMode(ctx))
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

	s.recordTurnUsage(routeRes, finalProvider, decision.ServedIdentity(), in, out, cacheCreation, cacheRead)

	if proxyErr == nil {
		s.emitBilling(ctx, requestID, externalID, decision, actPricing, routeRes, in, out, cacheCreation, cacheRead)
		if compResOAI.Summarized {
			s.billCompactionSummary(ctx, requestID, externalID, compResOAI.SummaryUsage)
		}
	}

	// See ProxyMessages for the two-strike eviction rationale.
	s.maybeEvictPinAfterUpstreamErr(ctx, stickyHit, proxyErr, decision.Reason, installationIDFromContext(ctx), routeRes.SessionKey, stickyStateRole(routeRes))
	// See ProxyMessages for the two-strike provider-disable rationale.
	s.maybeDisableProviderAfterOverload(ctx, stickyHit, proxyErr, finalProvider, decision.Reason, installationIDFromContext(ctx), routeRes.SessionKey, stickyStateRole(routeRes), routeRes.PinRole)

	installationIDOAI, _ := ctx.Value(InstallationIDContextKey{}).(string)
	if installationIDOAI != "" {
		credentialKeyPrefix, credentialKeySuffix, credSource := s.credentialKeyParts(ctx)
		telOAI := InsertTelemetryParams{
			InstallationID:         installationIDOAI,
			APIKeyID:               apiKeyIDFromContext(ctx),
			RequestID:              requestID,
			SpanType:               "router.upstream",
			TraceID:                requestID,
			Timestamp:              requestStart,
			RequestedModel:         feats.Model,
			DecisionModel:          decision.Model,
			DecisionProvider:       decision.Provider,
			DecisionReason:         telemetryDecisionReason(ctx, decision.Reason),
			RequestedAllowedModels: requestedAllowedModelsForTelemetry(ctx),
			EstimatedInputTokens:   int32(feats.Tokens),
			StickyHit:              stickyHit,
			PinTier:                routeRes.PinTier,
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
			Strategy:               openaiObs.Strategy,
			RouteID:                openaiObs.RouteID,
			PolicyRouteKey:         openaiObs.PolicyRouteKey,
			PolicyArtifactID:       openaiObs.PolicyArtifactID,
			PolicyArtifactSHA256:   openaiObs.PolicyArtifactSHA256,
			RosterVersion:          openaiObs.RosterVersion,
			SidecarSchemaVersion:   openaiObs.SidecarSchemaVersion,
			TrainingAllowed:        openaiObs.TrainingAllowed,
			CaptureMode:            openaiObs.CaptureMode,
			DebugRef:               openaiObs.DebugRef,
			TTFTMs:                 openaiObs.TTFTMs,
			CacheCreationTokens:    cacheTokenPtr(cacheCreation),
			CacheReadTokens:        cacheTokenPtr(cacheRead),
			DeviceID:               clientID.DeviceID,
			SessionID:              clientID.SessionID,
			RouterUserID:           auth.UserIDFrom(ctx),
			ClientApp:              clientID.ClientApp,
			TurnType:               string(routeRes.TurnType),
			RolloutID:              openaiObs.RolloutID,
			UpstreamFinishReason:   stringPtrOrEmpty(respSummary.UpstreamFinishReason),
			StopReason:             stringPtrOrEmpty(respSummary.StopReason),
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
		}
		applyPlannerTelemetry(&telOAI, routeRes)
		applyAuthorityShadowTelemetry(&telOAI, routeRes)
		s.fireTelemetry(telOAI)
	}

	// One event per tool call that failed toolcheck validation, mirroring the
	// Anthropic path's per-model tool-calling-quality signal.
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

	log.Info("ProxyOpenAIChatCompletion complete", append([]any{"requested_model", feats.Model, "baseline_model", s.baselineFor(feats.Model), "decision_model", decision.Model, "decision_provider", decision.Provider, "primary_provider", primaryProvider, "primary_model", primaryModel, "fallback_attempts", winnerIdx, "failover_used", finalProvider != primaryProvider, "decision_reason", decision.Reason, "requested_tier", routeRes.RequestedTier.String(), "decision_tier", catalog.TierFor(decision.Model).String(), "embedded_tokens", len(promptText) / 4, "total_input_tokens", feats.Tokens, "has_tools", feats.HasTools, "embed_input", embedInput, "cross_format", crossFormat, "sticky_hit", stickyHit, "pin_tier", pinTier, "turn_type", string(tt), "route_ms", routeMs, "proxy_ms", proxyMs, "proxy_err", proxyErr, "upstream_err_body", providers.UpstreamErrorBodyMessage(proxyErr), "upstream_status", upstreamStatus(proxyErr), "upstream_finish_reason", respSummary.UpstreamFinishReason, "resp_stop_reason", respSummary.StopReason, "routing_marker", marker, "prior_served_model", routeRes.PriorServedModel, "hard_pinned", routeRes.HardPinned}, plannerLogFields(routeRes)...)...)
	s.reportPolicyOutcome(ctx, routeRes, decision, effortServed, finalProvider, fastServed, feats.Tokens, in, out, cacheCreation, cacheRead, routeMs, proxyMs, proxyErr, nil)

	// Subscription-only mode disables paid failover by pinning dispatch to the
	// single subscription binding above, so a dispatch failure here is the
	// caller's own subscription failing (e.g. a 429 weekly-limit) with nowhere
	// to reroute — its raw upstream envelope is the honest, accurate response.
	// The controlled 402 is reserved for the pre-dispatch case (turn can't run
	// on the sub at all); rewriting a served-sub runtime error to it would both
	// mislabel non-billing failures and be moot once the envelope is flushed.
	return proxyErr
}

// ProxyOpenAIResponses routes an OpenAI Responses API request. The Responses
// wire format is translated to Chat Completions on entry, dispatched through
// the existing chat-completions path, then the chat-completions response is
// re-emitted as Responses-shaped SSE / JSON. This keeps the turn loop, cache,
// pricing, and translation matrix unchanged.
func (s *Service) ProxyOpenAIResponses(ctx context.Context, body []byte, w http.ResponseWriter, r *http.Request) error {
	ctx = s.withUsageObserver(ctx, r.Header, routePathResponses)
	clientAppCodex := ClientIdentityFrom(ctx).ClientApp == ClientAppCodex
	if translate.FeedbackFooterSinceLastHumanTurnInResponses(body) {
		ctx = context.WithValue(ctx, responsesFooterEchoedContextKey{}, true)
	}
	conversion, err := translate.ConvertResponsesToChatCompletionsWithOptions(body, translate.ResponsesConversionOptions{
		PortableCodex: clientAppCodex,
	})
	if err != nil {
		return fmt.Errorf("translate responses request: %w", err)
	}
	chatBody, model := conversion.Body, conversion.Model
	if conversion.CodexFeedbackSkill {
		ctx = context.WithValue(ctx, codexFeedbackSkillContextKey{}, true)
	}
	codexNativeRequest := codexResponsesRequest(ctx, r.Header)
	nativeBody := conversion.OriginalBody
	if clientAppCodex {
		nativeBody, err = translate.StripRouterCommandsFromResponsesInput(nativeBody)
		if err != nil {
			return fmt.Errorf("strip Responses router command: %w", err)
		}
		// Codex records response.output_item.done as conversation history and
		// sends it back in the next native request. Remove only the badge this
		// client opted into so router text never reaches the selected model.
		nativeBody, err = translate.StripRoutingBadgeFromResponsesInput(nativeBody)
		if err != nil {
			return fmt.Errorf("strip native Responses routing badge: %w", err)
		}
		nativeBody, err = translate.StripFeedbackFooterFromResponsesInput(nativeBody)
		if err != nil {
			return fmt.Errorf("strip native Responses feedback footer: %w", err)
		}
	}
	// Every Responses turn stashes its original bytes for post-routing native
	// dispatch; NativeOnly and Codex-subscription turns also dispatch verbatim now.
	if conversion.Requirements.NativeOnly || codexNativeRequest {
		ctx = context.WithValue(ctx, codexResponsesBodyContextKey{}, nativeBody)
	}
	ctx = context.WithValue(ctx, nativeResponsesBodyContextKey{}, nativeBody)
	// Routing and sticky-state hashes must describe the exact native payload
	// that an OpenAI/Codex decision will receive, even when the portable Codex
	// projection lets HMM consider other deployed providers.
	if conversion.Requirements.NativeOnly || (clientAppCodex && codexNativeRequest) {
		originalEnvelope, parseErr := translate.ParseOpenAI(conversion.OriginalBody)
		if parseErr != nil {
			return fmt.Errorf("parse native Responses request: %w", parseErr)
		}
		ctx = context.WithValue(ctx, nativeResponsesReasoningHashContextKey{}, originalEnvelope.ReasoningConfigurationSHA256())
		ctx = context.WithValue(ctx, nativeResponsesToolHashContextKey{}, originalEnvelope.ToolConfigurationSHA256())
	}
	ctx = context.WithValue(ctx, responsesRequirementsContextKey{}, conversion.Requirements)
	ctx = context.WithValue(ctx, responsesTransformsContextKey{}, conversion.Report)
	// Routing, billing, and telemetry are reused via
	// ProxyOpenAIChatCompletion; chatBody is used only for routing features.
	wrapper := translate.NewResponsesWriter(w, model)
	if clientAppCodex {
		wrapper.EnableCodexBadgeProvenance()
	}
	wrapper.SetToolMappings(conversion.ToolMappings)
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
