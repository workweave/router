package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"weave-os/router/internal/observability"
	"weave-os/router/internal/observability/apm"
	"weave-os/router/internal/observability/otel"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/router/cluster"
	"weave-os/router/internal/router/handover"
	"weave-os/router/internal/router/hmm"
	"weave-os/router/internal/router/planner"
	"weave-os/router/internal/router/policy"
	"weave-os/router/internal/router/sessionpin"
	"weave-os/router/internal/router/turntype"
	"weave-os/router/internal/translate"

	"github.com/google/uuid"
)

// addToSet returns set with model added. Copies before mutating so a
// caller-shared or nil map is never modified in place.
func addToSet(set map[string]struct{}, model string) map[string]struct{} {
	out := make(map[string]struct{}, len(set)+1)
	for k := range set {
		out[k] = struct{}{}
	}
	out[model] = struct{}{}
	return out
}

// mergeDisabledProviders unions two pins' DisabledProviders (deduped): either
// the active pin or its HMM history row can carry overload strikes independently.
func mergeDisabledProviders(a, b []string) []string {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, p := range a {
		if _, ok := seen[p]; !ok {
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	for _, p := range b {
		if _, ok := seen[p]; !ok {
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	return out
}

// installationIDFromContext reads the installation ID stashed by auth
// middleware. Returns uuid.Nil (which skips the async pin upsert) if missing or invalid.
func installationIDFromContext(ctx context.Context) uuid.UUID {
	raw, _ := ctx.Value(InstallationIDContextKey{}).(string)
	if raw == "" {
		return uuid.Nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil
	}
	return id
}

// cacheWarm reports whether the pin's upstream prompt cache is still warm
// (prior turn ended within the provider's cache TTL). Cold pins get no
// cache-read discount in the planner's EV math.
func cacheWarm(pin sessionpin.Pin) bool {
	if pin.LastTurnEndedAt.IsZero() {
		return false
	}
	return time.Since(pin.LastTurnEndedAt) < providers.CacheTTLFor(pin.Provider)
}

// pinCacheCold keeps ordinary and HMM paths on the same cache-economics rule.
func pinCacheCold(pin sessionpin.Pin, prefixBroken bool) bool {
	return !cacheWarm(pin) || prefixBroken
}

func applyPinEvidence(res *turnLoopResult, pin sessionpin.Pin) {
	res.PinModel = pin.Model
	res.PinProvider = pin.Provider
	res.PinAgeSec = pinAge(pin)
	if !pin.LastTurnEndedAt.IsZero() {
		gapMS := time.Since(pin.LastTurnEndedAt).Milliseconds()
		res.PriorTurnGapMS = &gapMS
	}
}

func clearPinEvidence(res *turnLoopResult) {
	res.PinModel = ""
	res.PinProvider = ""
	res.PinAgeSec = 0
	res.PriorTurnGapMS = nil
}

// cacheablePrefixTokens projects the pin's previous-turn cache-hit share
// onto this turn's prompt. The share is a ratio of two measured counters, not
// measured cached tokens over an estimated current total — the latter biases k
// toward 1. Reports false when the pin carries no usage telemetry.
func cacheablePrefixTokens(pin sessionpin.Pin, total int, prefixBroken bool) (int, bool) {
	if prefixBroken {
		return 0, true // a client trim really did evict the prefix
	}
	cached := pin.LastCachedReadTokens + pin.LastCachedWriteTokens
	// input_tokens is fresh-only on Anthropic (disjoint from read/write) but is
	// prompt_tokens — already cache-inclusive — everywhere else. Mirrors
	// catalog.EffectiveInputCost's provider branch.
	prior := pin.LastInputTokens
	if pin.Provider == providers.ProviderAnthropic {
		prior += cached
	}
	if prior <= 0 {
		return 0, false
	}
	share := min(1.0, float64(cached)/float64(prior))
	return int(share * float64(total)), true
}

// plannerInputTokens returns the planner's prompt-size estimate from
// env.FullTokenEstimate. feats.Tokens is text-only and runs low; do NOT replace
// it at its call sites — it is an HMM sidecar feature calibrated on those
// values and seeds client-visible input_tokens.
func plannerInputTokens(env *translate.RequestEnvelope, feats translate.RoutingFeatures) int {
	if env == nil {
		return feats.Tokens
	}
	if full := env.FullTokenEstimate(); full > feats.Tokens {
		return full
	}
	return feats.Tokens
}

// plannerTokensFor keeps the corrected estimate behind the flag. Legacy EV
// scales linearly with token count against a fixed dollar threshold, so feeding
// it a bigger number would move STAY/SWITCH on deploy.
func (s *Service) plannerTokensFor(env *translate.RequestEnvelope, feats translate.RoutingFeatures) int {
	if !s.planner.CorrectedEconomics {
		return feats.Tokens
	}
	return plannerInputTokens(env, feats)
}

// turnLoopResult bundles the routing decision and pin/planner state.
type turnLoopResult struct {
	Decision       router.Decision
	SessionKey     [sessionpin.SessionKeyLen]byte
	InstallationID uuid.UUID
	// Strategy is the effective request strategy, carried through the response
	// path so async pin writes stay strategy-bound after ctx is cancelled.
	Strategy   router.Strategy
	TurnType   turntype.TurnType
	StickyHit  bool
	HardPinned bool
	// AuthoritativePerTurn is true only for eligible main/tool-result turns
	// whose active policy declared model-authoritative dispatch.
	AuthoritativePerTurn bool
	// UsageBypass is true when the caller's own subscription has headroom:
	// ProxyMessages must serve the requested model straight through with no
	// billing debit, bypassing Decision's normal dispatch.
	UsageBypass bool
	PinTier     string
	PinAgeSec   int64
	// ForcedPinDropped records that a /force-model pin existed but could not be
	// served (provider not enabled, excluded, or not image-capable); surfaced so
	// the turn does not silently contradict the "force-model applied" ack.
	ForcedPinDropped    bool
	ForcedPinDropReason string
	ForcedPinModel      string
	// PinProvider, PrefixBroken, and PriorTurnGapMS preserve the cache-state
	// evidence used by the planner for span-level shadow analysis.
	PinProvider    string
	PrefixBroken   bool
	PriorTurnGapMS *int64
	// PolicyFallback is true when the decision came from degrading a policy sidecar
	// deadline to a session pin or tier-3 default. Exclude from bandit training and
	// flag on the OTel span so degraded-mode spend is distinguishable.
	PolicyFallback bool
	// RequestedTier drives the session-pin role split (roleForTier) so a
	// low-tier background turn and a high-tier main turn never share a pin.
	RequestedTier catalog.Tier
	// PinRole is the session-pin role used for this turn, preventing a
	// low-tier background turn and a high-tier main turn from sharing a pin.
	PinRole string
	// StickyRole is the stored state role that backed a sticky decision. It is
	// PinRole for active pins and _hmm_history for HMM EV stays.
	StickyRole string
	// Fresh is the scorer's recommendation for this turn when the scorer ran.
	Fresh router.Decision
	// PlannerDecision holds the planner's verdict and EV math when the planner ran.
	PlannerDecision planner.Decision
	// PinModel is stamped independently of PlannerDecision so log lines can
	// name the from-model even on stay outcomes.
	PinModel string
	// PriorServedModel is the pin's LastServedModel, independent of PinModel
	// (a /force-model write changes PinModel but not this). Compared against
	// the decision model to detect a mid-session switch, so the Anthropic
	// emit path can strip thinking blocks the new model would reject.
	PriorServedModel string
	// SessionEverSwitched is true once the session has ever served two
	// different models. PriorServedModel only flags the single switch-back
	// turn, but stale-signed thinking blocks from that excursion persist in
	// the client transcript on every later turn, so the emit path ORs this
	// in to keep stripping them for the life of the session.
	SessionEverSwitched bool
	// StripThinkingBlocks forces signature removal when switch history is unavailable.
	StripThinkingBlocks bool
	// Handover captures the summarize-or-trim step when the planner switched.
	Handover handoverOutcome
	// SuggestionMode suppresses the routing-marker badge for requests carrying
	// the x-weave-suggestion-mode header.
	SuggestionMode bool
	// PrefixTrimmed is true when the compaction tracker detected a client-side
	// history trim this turn. Set before routing so the planner can price the
	// pin's cache as cold; ProxyMessages also reads it post-routing for the
	// compaction handover without re-recording the tracker.
	PrefixTrimmed bool
	// EscalateEffort is true when the pin's prior turn looked like an
	// observable failure (no output, or a consecutive upstream error).
	// Reflects the loaded pin regardless of same-turn pin-drop guards below;
	// the escalate-on-failure policy (Service.effortEscalation) reads it to
	// bump a gpt-5.x turn from low to high effort, and is a no-op when disabled.
	EscalateEffort bool
	// PinTurnCount and PinFirstPinnedAt are session age + turn count from the
	// loaded pin; zero when no pin exists, so the detector no-ops on fresh sessions.
	PinTurnCount     int
	PinFirstPinnedAt time.Time
	// SessionDisabledProviders are providers struck out by repeated 529
	// exhaustion. Stashed on ctx so resolveBindingsForDispatch's failover
	// walk also honors the exclusion, not just this turn's scorer.
	SessionDisabledProviders []string
	// AuthorityShadow is the counterfactual HMM cache-gate verdict on an
	// authoritative-per-turn turn. Observation only: it never touches Decision.
	AuthorityShadow authorityCacheShadow
}

// authorityCacheShadow records what hmmCostGatedDecision would have returned on
// an authoritative-per-turn turn. Computed=false keeps every column NULL rather
// than zero, which would read as "the gate ran and found nothing".
type authorityCacheShadow struct {
	Computed bool
	Decision planner.Decision
	// StayModel/StayProvider are the pin the shadow priced against, empty when
	// no eligible pin existed (Decision.Reason is no_pin in that case).
	StayModel    string
	StayProvider string
	// Sticky is the gate's own verdict that it would have served StayModel instead
	// of the authoritative fresh pick. Persisted rather than re-derived in SQL:
	// StayModel carries ":effort" while decision_model is a bare catalog ID, so a
	// string compare reports a false divergence on every effort-bearing pin.
	Sticky bool
	// StayScore/FreshScore are the router's preference-adjusted candidate scores.
	// Nil when the active roster cannot score the arm; that nil rate is useful.
	StayScore  *float64
	FreshScore *float64
}

// Reason returns the gate's reason string, or "" when the shadow did not run.
func (a authorityCacheShadow) Reason() string {
	if !a.Computed {
		return ""
	}
	return a.Decision.Reason
}

// EVRan reports whether the gate reached planner.Decide's cost arithmetic.
// ShadowComputed is set immediately after that block; every early return
// (no_pin, no_prior_usage, same_model, pricing_missing) exits above it, so
// the flag is an exact witness for "the EV terms mean something".
func (a authorityCacheShadow) EVRan() bool {
	return a.Computed && a.Decision.ShadowComputed
}

// modelSwitched reports whether the Anthropic emit path must strip historical
// thinking blocks: true on the transition turn itself, or any turn after a
// session has ever switched. Claude Code re-sends the full transcript every
// turn, so stale-signed blocks from an earlier cross-model excursion would
// otherwise 400 with "Invalid signature in thinking block" on every later turn.
func (r turnLoopResult) modelSwitched() bool {
	// Compare serving identities: a same-model effort change reshapes the
	// prompt-cache prefix and invalidates thinking-block signatures.
	transition := r.PriorServedModel != "" && r.PriorServedModel != r.Decision.ServedIdentity()
	return transition || r.SessionEverSwitched || r.StripThinkingBlocks
}

func isHMMDecision(dec router.Decision) bool {
	if dec.Metadata != nil && router.IsHMMStrategy(router.Strategy(dec.Metadata.Strategy)) {
		return true
	}
	return strings.HasPrefix(strings.TrimSpace(dec.Reason), "hmm_policy")
}

const hmmHistoryReason = "hmm_history"
const defaultHMMUpgradeConfidenceThreshold = 0.85

const (
	hmmReasonConfidentUpgrade     = "hmm_confident_upgrade"
	hmmReasonUpgradeConfidenceLow = "hmm_upgrade_confidence_low"
	hmmReasonPhaseChange          = "hmm_phase_change"
)

// decisionPolicyGroup returns the policy cluster/group a decision was drawn
// from, or "" for reconstructed pins and routers that report no group.
func decisionPolicyGroup(dec router.Decision) string {
	if dec.Metadata == nil {
		return ""
	}
	return dec.Metadata.PolicyGroup
}

// policyDeadlineFallbackReason is set as PinTier when a policy sidecar deadline degrades to a pin or tier-3 default.
const policyDeadlineFallbackReason = "policy_deadline_last_known_good"

// policyDeadlineDefaultReason is the Decision.Reason when a deadline miss with no pin falls to the tier-3 default.
const policyDeadlineDefaultReason = "policy_deadline_default_model"

// isPolicyDeadlineErr reports whether err is a policy sidecar deadline/transport
// failure (safe to degrade) rather than a contract violation (must fail closed).
// Both context.DeadlineExceeded/Canceled and hmm.ErrHMMUnavailable must be present —
// sidecar_router.go also wraps contract violations with ErrHMMUnavailable, so the
// deadline/cancel check is load-bearing.
func isPolicyDeadlineErr(err error) bool {
	if err == nil {
		return false
	}
	if !errors.Is(err, hmm.ErrHMMUnavailable) {
		return false
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

func hmmHistoryRole(role string) string {
	if role == "" {
		role = sessionpin.DefaultRole
	}
	return role + "_hmm_history"
}

// handoverOutcome describes the synchronous handover step.
type handoverOutcome struct {
	Invoked       bool
	LatencyMS     int64
	SummaryTokens int
	// FallbackToFullHistory is set when handover was invoked but no summary
	// was applied (unwired, tenant-boundary skip, timeout, error, or empty
	// summary), so the full body passes through unchanged. No ledger row billed.
	FallbackToFullHistory bool
	// SummaryUsage is the summarizer call's upstream usage, so fireBilling can
	// debit it as a separate "_summary" ledger row. Zero on fallback/error paths.
	SummaryUsage handover.Usage
}

// hasSubAgentOverride reports whether both sub-agent override fields are set;
// a partial override is treated as unconfigured.
func (s *Service) hasSubAgentOverride() bool {
	return s.subAgentProvider != "" && s.subAgentModel != ""
}

// isHardPinnedTurn reports whether a turn type bypasses pin lookup/write,
// planner, and scorer entirely via the boot-time hard pin. These turns are
// also skipped by proactive compaction: they are either tiny (probe/title-gen/
// classifier) or carry their own dedicated flow (Claude Code's compaction turn,
// whose request the router must not rewrite). SubAgentDispatch hard-pins when
// the legacy hardPinExplore is on OR a per-sub-agent override is configured;
// the HMM strategy keeps its own sub-agent handling path so it overrides both.
func (s *Service) isHardPinnedTurn(ctx context.Context, tt turntype.TurnType) bool {
	switch tt {
	case turntype.Compaction, turntype.Probe, turntype.TitleGen, turntype.Classifier:
		return true
	case turntype.SubAgentDispatch:
		if router.IsHMMStrategy(router.StrategyFromContext(ctx)) {
			return false
		}
		return s.hardPinExplore || s.hasSubAgentOverride()
	default:
		return false
	}
}

func authoritativePolicyTurn(tt turntype.TurnType) bool {
	return tt == turntype.MainLoop || tt == turntype.ToolResult
}

func isUserForcedReason(reason string) bool {
	return strings.HasPrefix(reason, translate.ReasonUserForceModel)
}

// pinServesImages reports whether a pinned model can carry an image-bearing
// turn. Forced pins outrank every automatic gate, so without this check the
// pasted-screenshot turn dispatches to a text-only model and the upstream
// rejects the whole request.
func pinServesImages(pin sessionpin.Pin, req router.Request) bool {
	return !req.HasImages || catalog.AcceptsImages(pin.Model)
}

// excludingModel returns excluded plus model, copying instead of mutating the
// caller's map.
func excludingModel(excluded map[string]struct{}, model string) map[string]struct{} {
	if _, ok := excluded[model]; ok {
		return excluded
	}
	out := make(map[string]struct{}, len(excluded)+1)
	for k := range excluded {
		out[k] = struct{}{}
	}
	out[model] = struct{}{}
	return out
}

func pinEligible(pin sessionpin.Pin, req router.Request) bool {
	if pin.Model == "" || pin.Provider == "" {
		return false
	}
	if _, excluded := req.ExcludedModels[pin.Model]; excluded {
		return false
	}
	if !pinServesImages(pin, req) {
		return false
	}
	if req.EnabledProviders == nil {
		return true
	}
	_, ok := req.EnabledProviders[pin.Provider]
	return ok
}

// automaticallyDisabled reports whether a deployment-wide disable withdrew this
// model from automatic selection. Every reuse of an automatically-chosen model
// consults it, so a disable takes effect on sessions already pinned to that
// model instead of only on sessions that route fresh.
func automaticallyDisabled(req router.Request, model string) bool {
	_, disabled := req.AutomaticExcludedModels[model]
	return disabled
}

// automaticPinEligible gates reuse of a pin the router chose on the session's
// behalf. forcedPinEligible deliberately omits the deployment-wide check: an
// explicit /force-model is the escape hatch that keeps a disabled model
// reachable for debugging and evaluation.
func automaticPinEligible(pin sessionpin.Pin, req router.Request) bool {
	return pinEligible(pin, req) && !automaticallyDisabled(req, pin.Model)
}

// withAutomaticExclusions folds the deployment-wide soft set into the hard-pin
// resolver's own exclusions, which is the only denylist that selector reads.
func withAutomaticExclusions(req HardPinRequest, automaticExcluded map[string]struct{}) HardPinRequest {
	req.ExcludedModels = mergeExcludedModels(req.ExcludedModels, automaticExcluded)
	return req
}

func forcedPinEligible(pin sessionpin.Pin, req router.Request) bool {
	return pinEligible(pin, req)
}

func forcedPinIneligibilityReason(pin sessionpin.Pin, req router.Request) string {
	if req.EnabledProviders != nil {
		if _, enabled := req.EnabledProviders[pin.Provider]; !enabled {
			return "provider_not_enabled"
		}
	}
	if !pinServesImages(pin, req) {
		return "not_image_capable"
	}
	return "excluded"
}

// runTurnLoop is the format-agnostic routing orchestrator: detect turn type,
// short-circuit hard pins, load pin, run scorer, hand to planner, and on
// switch attempt bounded-cost handover.
//
// installationID == uuid.Nil skips the async pin upsert (rows need one); the
// rest of the path runs normally.
func (s *Service) runTurnLoop(
	ctx context.Context,
	env *translate.RequestEnvelope,
	feats translate.RoutingFeatures,
	apiKeyID string,
	installationID uuid.UUID,
	subAgentHint string,
	reqHeaders http.Header,
	req router.Request,
) (turnLoopResult, error) {
	log := observability.FromContext(ctx)
	if requirements, ok := translationRequirementsFromContext(ctx); ok {
		req.TranslationRequirements = requirements
	} else if req.TranslationRequirements.IsZero() {
		req.TranslationRequirements = env.TranslationRequirements(translationEndpointFor(env))
	}
	threadSessionKey := deriveSessionKeyForRequest(ctx, env, apiKeyID)
	req.TranslationRequirements = s.scopeSearchRequirement(threadSessionKey, env, req.TranslationRequirements)
	var compatibilityErr error
	req, compatibilityErr = s.applyTranslationPlan(ctx, req)
	if compatibilityErr != nil {
		return turnLoopResult{}, compatibilityErr
	}
	ctx = context.WithValue(ctx, translationPlanAppliedContextKey{}, true)
	// The turn-loop has to load this before any automatic pin or utility hard-pin
	// branch; routeFor receives a copy and cannot populate the caller's request.
	req.AutomaticExcludedModels = s.globalAutomaticExcludedModels(ctx)
	if transforms, ok := ctx.Value(responsesTransformsContextKey{}).([]translate.ResponseTransform); ok {
		for _, transform := range transforms {
			apm.RecordTranslationTransform(
				ctx,
				transform.Code,
				transform.Action,
				string(req.TranslationRequirements.SourceFormat),
				string(s.translationCompatibilityMode),
			)
			otel.RecordLog(ctx, otel.LogRecord{
				Name: "translation.transform",
				Time: time.Now(),
				Attrs: otel.NewAttrBuilder(5).
					String("translation.code", transform.Code).
					String("translation.action", transform.Action).
					String("translation.source_format", string(req.TranslationRequirements.SourceFormat)).
					String("translation.mode", string(s.translationCompatibilityMode)).
					String("translation.path", transform.Path).
					Build(),
			})
		}
	}
	req.OrganizationID, _ = ctx.Value(ExternalIDContextKey{}).(string)
	if installationID != uuid.Nil {
		req.InstallationID = installationID.String()
	}
	res := turnLoopResult{
		InstallationID:      installationID,
		Strategy:            router.StrategyFromContext(ctx),
		TurnType:            turntype.DetectFromEnvelope(env, feats, subAgentHint),
		PinTier:             "miss",
		RequestedTier:       catalog.TierFor(feats.Model),
		StripThinkingBlocks: betaArtifactHistoryFromContext(ctx),
	}
	res.AuthoritativePerTurn = authoritativePolicyTurn(res.TurnType) &&
		s.authoritativePerTurnSelection(ctx)
	res.PinRole = roleForTier(res.RequestedTier)
	log.Info("turnloop classified",
		"turn_type", string(res.TurnType),
		"requested_tier", res.RequestedTier.String(),
		"pin_role", res.PinRole,
		"sub_agent_hint", subAgentHint,
	)

	// Force state is session-scoped so sub-agents inherit the parent choice.
	forceModelSessionKey := deriveForceModelSessionKeyForRequest(ctx, env, apiKeyID, threadSessionKey)
	forceModelPin := sessionpin.Pin{}
	forceModelFound := false
	forceModelCleared := false
	if req.ForceModel != "" {
		canonicalModel, provider, known, effort := resolveForceModelWithEffort(req.ForceModel)
		if !known {
			return res, &ForcedModelUnknownError{Model: req.ForceModel}
		}
		forceModelPin = sessionpin.Pin{
			SessionKey:     forceModelSessionKey,
			Role:           forceModelSessionRole,
			InstallationID: installationID,
			Provider:       provider,
			Model:          canonicalModel,
			Effort:         effort,
			Reason:         translate.ReasonUserForceModel,
			PinnedUntil:    pinNeverExpires,
		}
		forceModelFound = true
	}
	if !forceModelFound {
		forceModelPin, forceModelFound, forceModelCleared = s.loadForceModelSessionPin(ctx, forceModelSessionKey)
	}
	if forceModelFound {
		binding, reason := s.forcedModelBinding(ctx, forceModelPin.Model, forceModelPin.Provider)
		if reason != "" {
			return res, &ForcedModelExcludedError{Model: forceModelPin.Model, Reason: reason}
		}
		forceModelPin.Provider = binding
		req.ExcludedModels = s.readmitForcedModel(ctx, req, env, feats, forceModelPin)
	}
	sessionForceControlFound := forceModelFound

	// Discounts covered models' cost term by the caller's observed subscription
	// headroom. nil (feature off / no headroom yet) leaves scoring unchanged.
	req.SubsidizedModelCostFactor = s.subsidyFactors(ctx, reqHeaders)

	// Explicit user force outranks every automatic fast path, including hard
	// pins. Legacy thread-scoped forces keep their original thread boundary.
	hardPinnedTurn := s.isHardPinnedTurn(ctx, res.TurnType)
	if s.pinStore != nil && hardPinnedTurn && !forceModelFound && !forceModelCleared {
		legacyPin, found := s.loadPin(ctx, threadSessionKey, res.PinRole)
		if found && isUserForcedReason(legacyPin.Reason) {
			binding, reason := s.forcedModelBinding(ctx, legacyPin.Model, legacyPin.Provider)
			if reason != "" {
				return res, &ForcedModelExcludedError{Model: legacyPin.Model, Reason: reason}
			}
			legacyPin.Provider = binding
			forceModelPin, forceModelFound = legacyPin, true
			req.ExcludedModels = s.readmitForcedModel(ctx, req, env, feats, forceModelPin)
		}
	}
	if forceModelFound && hardPinnedTurn {
		if forcedPinEligible(forceModelPin, req) {
			threadPin, hmmHistory, forceHistory := sessionpin.Pin{}, sessionpin.Pin{}, sessionpin.Pin{}
			if s.pinStore != nil {
				threadPin, _ = s.loadPin(ctx, threadSessionKey, res.PinRole)
				hmmHistory = s.loadHMMHistory(ctx, threadSessionKey, res.PinRole)
				forceHistory = s.loadForceModelHistory(ctx, threadSessionKey, res.PinRole)
			}
			res.SessionKey = threadSessionKey
			res.PinModel = forceModelPin.Model
			res.PinAgeSec = pinAge(forceModelPin)
			res.PriorServedModel, res.SessionEverSwitched = switchHistoryFromPins(threadPin, hmmHistory, forceHistory)
			res.EscalateEffort = !forceHistory.LastTurnEndedAt.IsZero() &&
				(forceHistory.LastOutputTokens == 0 || forceHistory.ConsecutiveUpstreamErrors > 0)
			res.Decision = pinDecision(forceModelPin)
			res.Decision.Reason = translate.ReasonUserForceModel
			res.StickyHit = true
			res.PinTier = translate.ReasonUserForceModel
			s.anchorForceModelHistory(ctx, installationID, threadSessionKey, res.PinRole, forceModelPin)
			return res, nil
		}
		res.SessionKey = threadSessionKey
		res.ForcedPinDropped = true
		res.ForcedPinModel = forceModelPin.Model
		res.ForcedPinDropReason = forcedPinIneligibilityReason(forceModelPin, req)
		log.Info("Forced session pin dropped for hard-pinned turn",
			"pin_model", forceModelPin.Model,
			"pin_provider", forceModelPin.Provider,
			"drop_reason", res.ForcedPinDropReason,
			"role", res.PinRole,
		)
	}

	// Automatic hard pins bypass pin lookup/write, planner, and scorer entirely.
	// Probes and title-gen must never create a session pin: the Anthropic SDK fires
	// probes before the first real turn, and Claude Code fires title-gen
	// ~25ms before the real-conv call — an anchored pin would leak the
	// cheap-model decision into the conversation that follows.
	if hardPinnedTurn {
		provider, model := s.hardPinProvider, s.hardPinModel
		// Sub-agent override is explicit operator config (mirrors ROUTER_HARD_PIN_MODEL
		// semantics), so it skips hardPinResolver rather than being resolved dynamically.
		useSubAgentOverride := res.TurnType == turntype.SubAgentDispatch && s.hasSubAgentOverride()
		if useSubAgentOverride {
			provider, model = s.subAgentProvider, s.subAgentModel
		}
		// The boot-time hard-pin was computed over every registered provider,
		// but a BYOK request may only authenticate to a subset. Resolve
		// per-request against enabled-providers, and apply ExcludedModels
		// here too — this path bypasses the scorer, the only other place
		// exclusions are honored.
		// Claude Code's own compaction turn summarizes the whole session, so
		// it goes to the session's Anthropic model (warm cache) or the
		// Sonnet-class compaction model rather than the cheapest utility pin.
		compactionProvider, compactionModel, compactionPin := "", "", false
		if s.compactionHardPinEnabled && res.TurnType == turntype.Compaction && !useSubAgentOverride {
			compactionProvider, compactionModel, compactionPin = s.compactionHardPin(ctx, threadSessionKey, res.PinRole, req)
		}
		switch {
		case compactionPin:
			provider, model = compactionProvider, compactionModel
			log.Info("Hard-pin: compaction turn on compaction model", "hard_pin_model", model, "hard_pin_provider", provider)
		case s.hardPinResolver != nil && !useSubAgentOverride:
			hardPinReq := HardPinRequest{
				EnabledProviders: req.EnabledProviders,
				ExcludedModels:   req.ExcludedModels,
				CustomBindings:   req.CustomBindings,
				GatewayProviders: req.GatewayProviders,
			}
			p, m, ok := s.hardPinResolver(withAutomaticExclusions(hardPinReq, req.AutomaticExcludedModels))
			if !ok {
				// Utility turns are automatic, so they honor the deployment-wide
				// disable — but softly: retry unrestricted rather than 503 a
				// probe or title-gen turn the operator never meant to break.
				p, m, ok = s.hardPinResolver(hardPinReq)
				if ok {
					log.Warn("Hard-pin: automatic-routing exclusions left no candidate; ignoring them for this turn",
						"turn_type", string(res.TurnType),
						"hard_pin_model", m,
						"hard_pin_provider", p,
					)
				}
			}
			if !ok {
				// Gateway empty result = no aliases configured (customer fix), not a router fault.
				hardPinErr := cluster.ErrClusterUnavailable
				if len(req.GatewayProviders) > 0 {
					hardPinErr = policy.ErrGatewayServesNoDeployedModel
				}
				log.Warn(
					"Hard-pin: no eligible provider for request",
					"turn_type", string(res.TurnType),
					"enabled_providers", sortedEnabledKeys(req.EnabledProviders),
					"err", hardPinErr,
				)
				return res, fmt.Errorf("hard-pin: no eligible provider for %s: %w", res.TurnType, hardPinErr)
			}
			provider, model = p, m
		default:
			if req.EnabledProviders != nil {
				if _, enabled := req.EnabledProviders[provider]; !enabled {
					return res, fmt.Errorf("hard-pin provider %q is ineligible for %s: %w", provider, res.TurnType, cluster.ErrNoEligibleProvider)
				}
			}
			if _, excluded := req.ExcludedModels[model]; excluded {
				return res, fmt.Errorf("hard-pin model %q is ineligible for %s: %w", model, res.TurnType, cluster.ErrNoEligibleProvider)
			}
			if automaticallyDisabled(req, model) {
				// A statically configured hard pin has no alternative to fall
				// back to, and a soft exclusion must not fail a turn.
				log.Warn("Hard-pin model is disabled for automatic routing; serving it anyway",
					"turn_type", string(res.TurnType),
					"hard_pin_model", model,
					"hard_pin_provider", provider,
				)
			}
		}
		// Operator hard-pins (ROUTER_HARD_PIN_MODEL) bypass the tier ceiling
		// by design; clamping would silently defeat an explicit operator opt-in.
		hardDecision := router.Decision{
			Provider: provider,
			Model:    model,
			Reason:   string(res.TurnType) + "_hard_pin",
		}
		res.Decision = hardDecision
		res.StickyHit = true
		res.HardPinned = true
		res.PinTier = string(res.TurnType) + "_hard_pin"
		return res, nil
	}

	// res.SessionKey must stay zero in no-pin-store mode, but trim detection
	// needs the key either way.
	sessionKey := threadSessionKey

	// Runs before routing so the planner can price the pin's cache as dead on
	// the turn the client rewrote the prompt prefix; env isn't rewritten yet
	// so counts match what the client sent.
	res.PrefixTrimmed = s.compaction.checkAndRecord(
		sessionKey, installationID, res.PinRole,
		feats.MessageCount, len(env.AssistantToolCallSignatures()),
	)
	// prefixTrimFreeSwitch gates actions only; detection stays unconditional
	// so the compaction handover keeps working when the lever is off.
	prefixBroken := s.ResolvePrefixTrimFreeSwitch(ctx) && res.PrefixTrimmed
	res.PrefixBroken = prefixBroken
	if res.PrefixTrimmed {
		log.Info("turnloop detected client history trim",
			"message_count", feats.MessageCount,
			"free_switch_armed", prefixBroken,
		)
	}

	// Without a pin store, run the scorer and return its decision. The usage
	// bypass intercepts the fresh scorer decision here too (no pins to honor).
	if s.pinStore == nil {
		if forceModelFound && forcedPinEligible(forceModelPin, req) {
			res.Decision = pinDecision(forceModelPin)
			res.Decision.Reason = translate.ReasonUserForceModel
			res.StickyHit = true
			res.PinTier = translate.ReasonUserForceModel
			return res, nil
		}
		req.PolicyTurnContext = buildPolicyTurnContext(req, res, sessionpin.Pin{}, sessionpin.Pin{})
		if dec, ok := s.usageBypassDecision(ctx, reqHeaders, req); ok {
			res.Decision = dec
			res.UsageBypass = true
			return res, nil
		}
		decision, err := s.routeFor(ctx, req)
		if err != nil {
			return res, err
		}
		res.Decision = decision
		res.Fresh = decision
		return res, nil
	}

	res.SessionKey = sessionKey

	pin, pinFound := s.loadPin(ctx, res.SessionKey, res.PinRole)
	// A deliberate clear is stored as an expired, blank pin with a reason.
	// Natural expiry retains the model/provider and may still use a renewed
	// one-shot command continuation, but an explicit clear must never be
	// undone by a stale continuation row.
	clearedPinReason := ""
	if !pinFound && pin.Model == "" && pin.Provider == "" && pin.Reason != "" {
		clearedPinReason = pin.Reason
	}
	hmmHistory := s.loadHMMHistory(ctx, res.SessionKey, res.PinRole)
	forceHistory := sessionpin.Pin{}
	if forceModelFound || forceModelCleared {
		forceHistory = s.loadForceModelHistory(ctx, res.SessionKey, res.PinRole)
	}
	if !forceModelFound && !forceModelCleared && pinFound && isUserForcedReason(pin.Reason) {
		binding, reason := s.forcedModelBinding(ctx, pin.Model, pin.Provider)
		if reason != "" {
			return res, &ForcedModelExcludedError{Model: pin.Model, Reason: reason}
		}
		pin.Provider = binding
		forceModelPin, forceModelFound = pin, true
		req.ExcludedModels = s.readmitForcedModel(ctx, req, env, feats, forceModelPin)
	}
	if forceModelCleared && pinFound && isUserForcedReason(pin.Reason) {
		pinFound = false
		pin = sessionpin.Pin{}
	}
	commandContinuation := sessionpin.Pin{}
	commandContinuationFound := false
	if res.TurnType == turntype.MainLoop {
		commandContinuation, commandContinuationFound = s.consumePostCommandContinuation(ctx, res.SessionKey, res.PinRole)
	}
	// Applied regardless of pinFound: eviction sets PinnedUntil in the past
	// (routing miss) but DisabledProviders must still steer the scorer away
	// from the struck-out provider this same turn; HMM-sticky strikes write
	// to _hmm_history, not PinRole, so either row can carry evidence.
	// Before the strike exemption below, so the exemption covers the binding
	// the pin is remapped to, not the (possibly excluded) stored provider.
	if pinFound && isUserForcedReason(pin.Reason) {
		binding, reason := s.forcedModelBinding(ctx, pin.Model, pin.Provider)
		if reason != "" {
			log.Warn("turnloop: forced pin refers to an excluded model",
				"pin_model", pin.Model,
				"pin_provider", pin.Provider,
				"reason", reason,
			)
			return res, &ForcedModelExcludedError{Model: pin.Model, Reason: reason}
		}
		// The model survives on another binding, so follow it there instead of
		// letting the eligibility check below drop the pin.
		pin.Provider = binding
	}
	disabledProviders := mergeDisabledProviders(pin.DisabledProviders, hmmHistory.DisabledProviders)
	// Explicit force exempts its own provider from session-level breaker state.
	forcedProvider := ""
	if forceModelFound {
		forcedProvider = forceModelPin.Provider
	} else if pinFound && isUserForcedReason(pin.Reason) {
		forcedProvider = pin.Provider
	}
	if forcedProvider != "" {
		filtered := make([]string, 0, len(disabledProviders))
		for _, provider := range disabledProviders {
			if provider != forcedProvider {
				filtered = append(filtered, provider)
			}
		}
		disabledProviders = filtered
	}
	if len(disabledProviders) > 0 {
		res.SessionDisabledProviders = disabledProviders
		// nil EnabledProviders means "unrestricted"; skip rather than
		// produce an empty map that reads as "every provider excluded."
		if req.EnabledProviders != nil {
			filtered := make(map[string]struct{}, len(req.EnabledProviders))
			for p := range req.EnabledProviders {
				filtered[p] = struct{}{}
			}
			for _, p := range disabledProviders {
				delete(filtered, p)
			}
			req.EnabledProviders = filtered
		}
	}
	res.PriorServedModel, res.SessionEverSwitched = switchHistoryFromPins(pin, hmmHistory, forceHistory)
	req.PolicyTurnContext = buildPolicyTurnContext(req, res, pin, hmmHistory)
	// Computed before any same-turn pin-drop guards below so it reflects the
	// prior turn's outcome; Service.effortEscalation gates whether it's acted on.
	res.EscalateEffort = pinFound && !pin.LastTurnEndedAt.IsZero() &&
		(pin.LastOutputTokens == 0 || pin.ConsecutiveUpstreamErrors > 0)
	if pinFound {
		// Stamped before pin-drop guards (context-window eviction, exclusion)
		// so the detector still sees them on the turn the pin is dropped.
		// +1: stored count is completed turns; this in-flight turn is the next,
		// so thresholds fire ON 30/80 (matching Phase 0's inclusive mining).
		res.PinTurnCount = pin.TurnCount + 1
		res.PinFirstPinnedAt = pin.FirstPinnedAt
		applyPinEvidence(&res, pin)
		log.Info("turnloop pin lookup hit",
			"pin_model", pin.Model,
			"pin_provider", pin.Provider,
			"pin_reason", pin.Reason,
			"pin_age_s", res.PinAgeSec,
			"pin_cache_warm", cacheWarm(pin),
			"last_output_tokens", pin.LastOutputTokens,
			"session_ever_switched", pin.HasEverSwitched,
		)
	} else {
		log.Info("turnloop pin lookup miss", "role", res.PinRole)
	}

	// User-forced pins (/force-model) are immutable stickies with a never-expires
	// PinnedUntil, so they skip scorer/planner until /unforce-model expires them.
	// Still enforced per-request: (1) exclusion policy — a newly-excluded forced
	// model falls through to normal routing; (2) provider eligibility — a BYOK
	// request missing the pinned provider's creds falls through rather than
	// guaranteeing a 401; (3) image capability — a text-only forced model falls
	// through rather than guaranteeing an upstream 400 on a screenshot turn.
	//
	// forcedTierFloor preserves the user's tier intent when the forced pin gets
	// dropped below (usually the session outgrew the model's context window):
	// the scorer call further down constrains the fresh decision to this tier
	// instead of collapsing to the cheap tier-default. TierUnknown = no constraint.
	forcedTierFloor := catalog.TierUnknown
	if forceModelFound {
		_, excluded := req.ExcludedModels[forceModelPin.Model]
		_, providerEnabled := req.EnabledProviders[forceModelPin.Provider]
		providerEligible := req.EnabledProviders == nil || providerEnabled
		imageCapable := pinServesImages(forceModelPin, req)
		if !excluded && providerEligible && imageCapable {
			res.PinModel = forceModelPin.Model
			res.PinAgeSec = pinAge(forceModelPin)
			res.EscalateEffort = !forceHistory.LastTurnEndedAt.IsZero() &&
				(forceHistory.LastOutputTokens == 0 || forceHistory.ConsecutiveUpstreamErrors > 0)
			res.Decision = pinDecision(forceModelPin)
			res.Decision.Reason = translate.ReasonUserForceModel
			res.StickyHit = true
			res.PinTier = translate.ReasonUserForceModel
			s.anchorForceModelHistory(ctx, installationID, res.SessionKey, res.PinRole, forceModelPin)
			return res, nil
		}
		res.ForcedPinDropped = true
		res.ForcedPinDropReason = forcedPinIneligibilityReason(forceModelPin, req)
		res.ForcedPinModel = forceModelPin.Model
		log.Info("Forced session pin dropped for this turn",
			"pin_model", forceModelPin.Model,
			"pin_provider", forceModelPin.Provider,
			"drop_reason", res.ForcedPinDropReason,
			"role", res.PinRole,
		)
		if excluded || !imageCapable {
			forcedTierFloor = catalog.TierFor(forceModelPin.Model)
		}
		if !imageCapable {
			req.ExcludedModels = excludingModel(req.ExcludedModels, forceModelPin.Model)
		}
		if !sessionForceControlFound || isUserForcedReason(pin.Reason) {
			pinFound = false
			pin = sessionpin.Pin{}
		}
	}
	if pinFound && (isUserForcedReason(pin.Reason) || pin.Reason == translate.ReasonLoopEscalation || pin.Reason == translate.ReasonStruggleEscalation) {
		_, excluded := req.ExcludedModels[pin.Model]
		_, providerEnabled := req.EnabledProviders[pin.Provider]
		providerEligible := req.EnabledProviders == nil || providerEnabled
		imageCapable := pinServesImages(pin, req)
		// Loop and struggle escalation are router-chosen rescues, so a
		// deployment-wide disable applies to them; only the user's own
		// /force-model outranks it.
		autoDisabled := !isUserForcedReason(pin.Reason) && automaticallyDisabled(req, pin.Model)
		if !excluded && !autoDisabled && providerEligible && imageCapable {
			decision := pinDecision(pin)
			decision.Reason = pin.Reason
			res.PinTier = pin.Reason
			res.Decision = decision
			res.StickyHit = true
			s.refreshPin(ctx, installationID, res.SessionKey, pin, res.PinRole, pinDecision(pin))
			return res, nil
		}
		// A forced pin is an explicit user instruction; dropping it silently reverts
		// to the scorer while the user still trusts the "force-model applied" ack.
		dropReason := "excluded"
		switch {
		case autoDisabled:
			dropReason = "automatic_routing_disabled"
		case !providerEligible:
			dropReason = "provider_not_enabled"
		case !imageCapable:
			dropReason = "not_image_capable"
		}
		log.Info("Forced session pin dropped for this turn",
			"pin_model", pin.Model,
			"pin_provider", pin.Provider,
			"pin_reason", pin.Reason,
			"drop_reason", dropReason,
			"role", res.PinRole,
		)
		if isUserForcedReason(pin.Reason) {
			res.ForcedPinDropped = true
			res.ForcedPinDropReason = dropReason
			res.ForcedPinModel = pin.Model
			if excluded || !imageCapable {
				// User still asked for this tier; constrain the fresh decision
				// to it below rather than losing the intent entirely.
				forcedTierFloor = catalog.TierFor(pin.Model)
			}
		} else if excluded || autoDisabled {
			// Auto-escalation carries no user tier intent. An excluded escalation
			// pin can never serve, so expire it instead of re-dropping it every
			// turn until TTL.
			evictReason := "escalation_pin_excluded"
			if autoDisabled {
				evictReason = "escalation_pin_automatic_routing_disabled"
			}
			if err := s.expireSessionPin(ctx, installationID, res.SessionKey, res.PinRole, evictReason); err != nil {
				log.Error("ineligible escalation pin eviction failed", "err", err, "pin_model", pin.Model, "role", res.PinRole, "evict_reason", evictReason)
			}
		}
		if !imageCapable {
			// The scorer's own image filter fails open when no image-capable
			// candidate survives, so make the drop explicit here instead of
			// letting the same text-only model be re-picked.
			req.ExcludedModels = excludingModel(req.ExcludedModels, pin.Model)
		}
		// Treat as missing so downstream sticky branches don't dispatch to an
		// unauthorized provider. The row stays in storage — a later request
		// with the forced provider enabled resumes serving it.
		pinFound = false
		pin = sessionpin.Pin{}
	}

	// A pin the router chose itself stops being reusable the moment Weave
	// disables its model deployment-wide. Dropping it here — before the
	// tool-result, planner-disabled, EV-stay, and re-anchor branches — is what
	// makes the setting reach sessions that are already pinned, instead of only
	// sessions that route fresh. The row is left in storage: the next turn
	// re-pins whatever the scorer picks, and re-enabling the model restores it.
	if pinFound && !isUserForcedReason(pin.Reason) && automaticallyDisabled(req, pin.Model) {
		reason, _ := s.globalAutomaticExclusionReason(ctx, pin.Model)
		log.Info("Session pin model is disabled for automatic routing; falling through to scorer",
			"pin_model", pin.Model,
			"pin_provider", pin.Provider,
			"pin_reason", pin.Reason,
			"disable_reason", reason,
			"role", res.PinRole,
		)
		pinFound = false
		pin = sessionpin.Pin{}
	}

	// Previous-turn-maxed-out guard: when an OSS model's tool-call tokens fail
	// to parse server-side (kimi/qwen3), the upstream emits them as content and
	// generates to the output cap, triggering Claude Code's auto-continue to
	// re-pin the same broken model in a loop. Exclude it and treat the pin as
	// missing so sticky branches (ToolResult, !plannerEnabled) can't re-anchor
	// it before the scorer runs.
	if pinFound && pin.LastOutputTokens >= prevTurnMaxedOutThreshold {
		// Key off LastServedModel, not pin.Model: with band swap the served
		// model can be the paired member, so pin.Model could name the wrong
		// (healthy) model and leave the broken one eligible. Fall back to
		// pin.Model for older rows written before LastServedModel existed.
		maxedModel := maxedOutServedModel(pin)
		log.Info("Session pin maxed out on previous turn; excluding for this turn",
			"pin_model", pin.Model,
			"pin_provider", pin.Provider,
			"maxed_model", maxedModel,
			"last_output_tokens", pin.LastOutputTokens,
		)
		// Defensive copy: callers may share the ExcludedModels map across requests.
		excluded := make(map[string]struct{}, len(req.ExcludedModels)+1)
		for k := range req.ExcludedModels {
			excluded[k] = struct{}{}
		}
		excluded[maxedModel] = struct{}{}
		req.ExcludedModels = excluded
		// Also bar it from usage bypass — the maxed-out exclusion is a hard
		// loop-breaking constraint; without it, an auto-continue turn re-requesting
		// the saturated model would bypass back to it and reopen the loop.
		req.SafetyExcludedModels = addToSet(req.SafetyExcludedModels, maxedModel)
		pinFound = false
		pin = sessionpin.Pin{}
	}
	if maxedModel := maxedOutServedModel(hmmHistory); maxedModel != "" {
		// No expiry gate: match the active-pin maxed path so the degenerate
		// auto-continue loop cannot re-select a saturated model after TTL lapses.
		log.Info("HMM history maxed out on previous turn; excluding for this turn",
			"history_provider", hmmHistory.Provider,
			"maxed_model", maxedModel,
			"last_output_tokens", hmmHistory.LastOutputTokens,
		)
		excluded := make(map[string]struct{}, len(req.ExcludedModels)+1)
		for k := range req.ExcludedModels {
			excluded[k] = struct{}{}
		}
		excluded[maxedModel] = struct{}{}
		req.ExcludedModels = excluded
		// See the active-pin path above: the maxed-out model must also block usage
		// bypass, or an auto-continue turn re-requesting it reopens the loop.
		req.SafetyExcludedModels = addToSet(req.SafetyExcludedModels, maxedModel)
	}

	// If the pre-filter excluded the pinned model for context overflow,
	// re-verify with a direct fit-check before evicting the pin. Must reuse
	// the pre-filter's estimate (ContextOverflowTokenEstimate, ÷4) rather than
	// the looser ÷6 FullTokenEstimate — otherwise a dense body the pre-filter
	// correctly excluded could be judged to fit here, un-excluding the pin and
	// hitting the same context-overflow 400 the pre-filter prevents.
	if pinFound {
		if _, overCapacity := req.ExcludedModels[pin.Model]; overCapacity {
			outputReserveForPin := contextWindowOutputReserve
			if feats.MaxTokens > outputReserveForPin {
				outputReserveForPin = feats.MaxTokens
			}
			pinTokenEstimate := env.ContextOverflowTokenEstimate()
			if modelStripsAnthropicSignatures(pin.Model) {
				pinTokenEstimate -= env.SignatureTokenSavings()
			}
			needed := pinTokenEstimate + outputReserveForPin
			modelCW := contextWindowForRequest(pin.Model, pin.Provider)
			if needed > modelCW {
				log.Info("Session pin excluded by context-window pre-filter; falling through to scorer",
					"pin_model", pin.Model,
					"pin_provider", pin.Provider,
					"token_estimate", pinTokenEstimate,
					"needed", needed,
					"model_context_window", modelCW,
				)
				pinFound = false
				pin = sessionpin.Pin{}
			} else {
				// Pre-filter was overly conservative — pin fits. Only lift
				// the exclusion if it came from the context filter, not an
				// operator/installation policy exclusion (a hard constraint
				// that must not be bypassed just because context happens to fit).
				policyExcluded := s.excludedModelsForRequest(ctx)
				_, policyExcludes := policyExcluded[pin.Model]
				compatibilityExcludes := req.TranslationRequirements.Images && !catalog.AcceptsImages(pin.Model)
				if !policyExcludes && !compatibilityExcludes {
					if len(req.ExcludedModels) > 0 {
						pruned := make(map[string]struct{}, len(req.ExcludedModels)-1)
						for k := range req.ExcludedModels {
							if k != pin.Model {
								pruned[k] = struct{}{}
							}
						}
						req.ExcludedModels = pruned
					}
					// Keep SafetyExcludedModels consistent: the fit-check just
					// cleared this model's context-overflow exclusion, so it must
					// not linger in the safety set and block usage bypass.
					delete(req.SafetyExcludedModels, pin.Model)
				}
				log.Info("Session pin preserved despite context-window pre-filter exclusion",
					"pin_model", pin.Model,
					"token_estimate", pinTokenEstimate,
					"needed", needed,
					"model_context_window", modelCW,
				)
			}
		}
	}

	// A request-level allowlist narrows the pool for this turn only; a pin
	// outside it reroutes inside the subset instead of serving through.
	if pinFound && !modelInRequestSubset(ctx, pin.Model) {
		log.Info("Session pin outside request allowed-models subset; falling through to scorer",
			"pin_model", pin.Model,
			"pin_provider", pin.Provider,
		)
		pinFound = false
	}

	// If the pinned provider is no longer in this request's enabled set
	// (installation/env exclusion, or BYOK without that provider's creds),
	// treat the pin as missing so sticky branches below can't keep serving
	// through it. Mirrors the providerEligible check on the forced-pin path above.
	if pinFound && req.EnabledProviders != nil {
		if _, ok := req.EnabledProviders[pin.Provider]; !ok {
			log.Info("Session pin provider not in enabled set; falling through to scorer",
				"pin_model", pin.Model,
				"pin_provider", pin.Provider,
			)
			pinFound = false
			pin = sessionpin.Pin{}
		}
	}

	// If this turn carries images but the pinned model is text-only, drop the
	// pin so the scorer picks an image-capable model. Deliberately not added
	// to ExcludedModels: that's a hard filter that errors on an empty pool,
	// which would break the soft fallback for OSS-only deploys with no
	// image-capable candidate. Without this guard a text-pinned session would
	// 4xx the moment the user pastes a screenshot.
	if pinFound && req.HasImages && !catalog.AcceptsImages(pin.Model) {
		log.Info("Session pin is text-only for image-bearing turn; falling through to scorer",
			"pin_model", pin.Model,
			"pin_provider", pin.Provider,
		)
		pinFound = false
		pin = sessionpin.Pin{}
	}
	if !pinFound {
		clearPinEvidence(&res)
	}

	// A router slash command is synthetic and intentionally skips upstream
	// dispatch. Its next normal turn must keep the current automatic model once
	// instead of treating the command boundary as a fresh HMM decision.
	if commandContinuationFound && clearedPinReason != "" {
		log.Info("discarding post-command continuation after source pin clear",
			"clear_reason", clearedPinReason,
			"pin_model", commandContinuation.Model,
			"pin_provider", commandContinuation.Provider,
		)
		commandContinuationFound = false
	}
	if commandContinuationFound && automaticPinEligible(commandContinuation, req) {
		decision := pinDecision(commandContinuation)
		res.Decision = decision
		res.StickyHit = true
		res.PinTier = "post_command_continuation"
		res.PinModel = commandContinuation.Model
		res.PinAgeSec = pinAge(commandContinuation)
		if forceModelCleared {
			res.PriorServedModel, res.SessionEverSwitched = switchHistoryFromPins(commandContinuation, hmmHistory, forceHistory, forceModelPin)
		} else {
			res.PriorServedModel, res.SessionEverSwitched = switchHistoryFromPins(commandContinuation, hmmHistory, forceHistory)
		}
		res.EscalateEffort = !commandContinuation.LastTurnEndedAt.IsZero() &&
			(commandContinuation.LastOutputTokens == 0 || commandContinuation.ConsecutiveUpstreamErrors > 0)
		log.Info("turnloop used one-shot post-command continuation",
			"pin_model", commandContinuation.Model,
			"pin_provider", commandContinuation.Provider,
		)
		s.refreshPin(ctx, installationID, res.SessionKey, commandContinuation, res.PinRole, decision)
		return res, nil
	}

	// Positioned after hard-pin/forced-pin (higher precedence) and after all
	// pin-drop guards (context overflow, provider disabled, images, maxed-out),
	// but before the tool-result/planner-disabled stickies and scorer, so a
	// stale pin from a prior routed stretch can't make a tool_result
	// continuation diverge from the bypassed tool_use turn. The pin itself is
	// untouched and resumes once utilization crosses the threshold.
	//
	// Bypass settles whether the turn is routed at all (caller's prepaid quota,
	// not a routing-quality opinion) — AuthoritativePerTurn controls which model
	// is chosen for a routed turn, so the gate must not apply here.
	if dec, ok := s.usageBypassDecision(ctx, reqHeaders, req); ok {
		res.Decision = dec
		res.UsageBypass = true
		return res, nil
	}

	// Tool-result turns: by default, fall through to the scorer + planner for
	// MainLoop parity. Kill switch preserves the legacy #82 verbatim-reuse path.
	// The #82 noisy-embedding concern is stale under only_user_message embed mode:
	// translate.userPromptTextGJSON strips tool_result blocks from the embed input.
	// Switches degrade safely — handover.RewriteEnvelope strips orphaned tool_results.
	if !res.AuthoritativePerTurn &&
		!s.ResolveScoreToolResultTurns(ctx) &&
		res.TurnType == turntype.ToolResult &&
		pinFound {
		decision := pinDecision(pin)
		res.Decision = decision
		res.StickyHit = true
		res.PinTier = "postgres_tool_result_sc"
		s.refreshPin(ctx, installationID, res.SessionKey, pin, res.PinRole, decision)
		return res, nil
	}

	// Planner-disabled + pin found: preserve first-decision-wins behavior.
	if !res.AuthoritativePerTurn && !s.ResolvePlannerEnabled(ctx) && pinFound {
		decision := pinDecision(pin)
		res.Decision = decision
		res.StickyHit = true
		res.PinTier = "postgres"
		s.refreshPin(ctx, installationID, res.SessionKey, pin, res.PinRole, decision)
		return res, nil
	}

	// If a user-forced pin was just evicted, route constrained to its tier so
	// we pick the next-best model instead of silently downgrading the user's
	// directive. Fall back to the unconstrained scorer if no in-tier model
	// survives the request's other filters.
	var fresh router.Decision
	routed := false
	if forcedTierFloor != catalog.TierUnknown {
		if constrained, ok := s.restrictToTier(req.ExcludedModels, forcedTierFloor); ok {
			tierReq := req
			tierReq.ExcludedModels = constrained
			if dec, derr := s.routeFor(ctx, tierReq); derr == nil {
				fresh, routed = dec, true
				log.Info("user-forced model evicted; rerouted to next-best in same tier",
					"forced_tier", forcedTierFloor.String(),
					"fresh_model", dec.Model,
					"fresh_provider", dec.Provider,
				)
			} else if res.AuthoritativePerTurn {
				return res, derr
			} else {
				log.Info("tier-constrained reroute found no candidate; using unconstrained scorer",
					"forced_tier", forcedTierFloor.String(), "err", derr)
			}
		}
	}
	if !routed {
		dec, err := s.routeFor(ctx, req)
		if err != nil {
			// Deadline != correctness failure: all candidates were dispatchable; only
			// ranking is lost. Contract violations still fail closed via isPolicyDeadlineErr.
			if s.policyDeadlineFallback && isPolicyDeadlineErr(err) {
				if pinFound && pin.Model != "" {
					decision := pinDecision(pin)
					// Use a distinct Reason so degraded-mode turns don't
					// read identically to genuine policy-chosen STAYs in analytics.
					decision.Reason = policyDeadlineFallbackReason
					res.Decision = decision
					res.StickyHit = true
					res.PinTier = policyDeadlineFallbackReason
					res.PolicyFallback = true
					log.Warn("policy sidecar missed its deadline; serving session pin",
						"err", err,
						"pin_model", pin.Model,
						"pin_provider", pin.Provider,
						"pin_policy_group", pin.PolicyGroup,
						"requested_model", req.RequestedModel,
					)
					// Persist the pin's own reason, not the degraded-mode one:
					// isHMMPinReason gates later HMM stickiness on it.
					refreshed := decision
					refreshed.Reason = pin.Reason
					s.refreshPin(ctx, installationID, res.SessionKey, pin, res.PinRole, refreshed)
					return res, nil
				}
				if decision, ok := s.policyDeadlineDefaultDecision(req); ok {
					res.Decision = decision
					res.PinTier = policyDeadlineFallbackReason
					res.PolicyFallback = true
					log.Warn("policy sidecar missed its deadline; no session pin, serving tier-3 default",
						"err", err,
						"default_model", decision.Model,
						"default_provider", decision.Provider,
						"requested_model", req.RequestedModel,
					)
					s.writeNewPin(ctx, installationID, res.SessionKey, res.PinRole, decision)
					return res, nil
				}
			}
			log.Error("turnloop scorer failed", "err", err, "requested_model", req.RequestedModel)
			return res, err
		}
		fresh = dec
	}
	log.Info("turnloop scorer decision",
		"fresh_model", fresh.Model,
		"fresh_provider", fresh.Provider,
		"fresh_reason", fresh.Reason,
	)
	res.Fresh = fresh
	if res.AuthoritativePerTurn {
		// Shadow before the pin-preserving gates so it covers every authoritative
		// exit. PinTier partitions the result: authoritative_per_turn = fresh was
		// served, sticky/confidence = pin was already kept.
		activePin := sessionpin.Pin{}
		if pinFound {
			activePin = pin
		}
		res.AuthorityShadow = s.authorityCacheShadowFor(
			ctx, req, activePin, hmmHistory, fresh, s.plannerTokensFor(env, feats), prefixBroken,
		)
		s.logAuthorityCacheShadow(ctx, res)
		// Upgrade-confidence guard: authoritative selection bypasses the HMM
		// cost gate, but the escalation floor still applies. A scored fresh
		// decision that costs more than the pinned model only wins at
		// confidence >= threshold; below it the session stays on its pin.
		// Unscored decisions, downgrades, and unpinned turns pass through.
		if s.ResolveAuthoritativeUpgradeGate(ctx) && pinFound && pin.Model != "" && pin.Model != fresh.Model &&
			hmmFreshIsMoreExpensive(pin.Model, fresh.Model, req.SubsidizedModelCostFactor) {
			if confidence, ok := hmmDecisionConfidence(fresh); ok && confidence < s.hmmUpgradeConfidenceThreshold {
				decision := pinDecision(pin)
				res.Decision = decision
				res.StickyHit = true
				res.PinTier = "authoritative_" + hmmReasonUpgradeConfidenceLow
				log.Info("turnloop suppressed low-confidence authoritative upgrade; keeping session pin",
					"pin_model", pin.Model,
					"pin_provider", pin.Provider,
					"fresh_model", fresh.Model,
					"fresh_provider", fresh.Provider,
					"confidence", confidence,
					"threshold", s.hmmUpgradeConfidenceThreshold,
				)
				s.refreshPin(ctx, installationID, res.SessionKey, pin, res.PinRole, decision)
				return res, nil
			}
		}
		res.Decision = fresh
		res.PinTier = "authoritative_per_turn"
		s.writeNewPin(ctx, installationID, res.SessionKey, res.PinRole, fresh)
		return res, nil
	}
	if isHMMDecision(fresh) {
		activePin := sessionpin.Pin{}
		if pinFound {
			activePin = pin
		}
		hmmDecision, hmmPlannerDecision, hmmSticky, hmmStayModel := s.hmmCostGatedDecision(
			req,
			activePin,
			hmmHistory,
			fresh,
			s.plannerTokensFor(env, feats),
			prefixBroken,
		)
		res.Decision = hmmDecision
		res.PlannerDecision = hmmPlannerDecision
		// Runs after the planner assignment above, so the hold survives in the
		// reason a bake-off reads rather than being overwritten by it.
		if heldEffort := effortHysteresisHold(fresh, res.PriorServedModel, hmmDecision.Model, hmmDecision.Effort); heldEffort != "" {
			res.Decision.Effort = heldEffort
			res.PlannerDecision.Reason = appendEffortHysteresisReason(res.PlannerDecision.Reason)
			log.Info("HMM effort hysteresis held incumbent",
				"incumbent", res.PriorServedModel,
				"challenger_effort", hmmDecision.Effort,
				"serving_effort", heldEffort,
			)
		}
		if hmmStayModel != "" {
			res.PinModel = hmmStayModel
			if hmmPin, ok := s.hmmStayPin(req, activePin, hmmHistory); ok && hmmPin.Model == hmmStayModel {
				applyPinEvidence(&res, hmmPin)
			}
		}
		if hmmSticky {
			res.StickyHit = true
			res.PinTier = "hmm_ev_stay_" + hmmPlannerDecision.Reason
			res.StickyRole = hmmHistoryRole(res.PinRole)
		} else {
			res.PinTier = "hmm_fresh_unpinned"
			if hmmPlannerDecision.Outcome == planner.OutcomeStay && hmmPlannerDecision.Reason != "" {
				res.PinTier = "hmm_ev_same_" + hmmPlannerDecision.Reason
			} else if hmmPlannerDecision.Reason != "" && hmmPlannerDecision.Reason != planner.ReasonNoPin {
				res.PinTier = "hmm_ev_switch_" + hmmPlannerDecision.Reason
			}
		}
		return res, nil
	}

	// Expired-pin re-anchor: when the pin lapsed mid-session (!pinFound but
	// pin.Model != "", not a first-turn miss), prefer the prior model over a
	// lateral scorer switch on just the expiry turn — a single-turn switch is
	// often noise the session would otherwise stay on for its whole life.
	// Re-anchor only if: both tiers known, fresh isn't a tier upgrade, prior
	// model is routable/not excluded, prior provider still enabled, prior
	// turn didn't max out the output cap (mirrors the live-pin guard above),
	// this turn has no images if prior model is text-only (ditto), and the
	// client didn't trim history this turn (a trim kills the cache anyway,
	// so let the fresh pick win). Writes a new pin so next turn is a sticky hit.
	if !pinFound && pin.Model != "" && !prefixBroken {
		pinTier := catalog.TierFor(pin.Model)
		freshTier := catalog.TierFor(fresh.Model)
		if pinTier != catalog.TierUnknown && freshTier != catalog.TierUnknown && freshTier <= pinTier {
			if _, excluded := req.ExcludedModels[pin.Model]; !excluded && !automaticallyDisabled(req, pin.Model) {
				if _, available := s.availableModels[pin.Model]; available {
					_, providerOK := req.EnabledProviders[pin.Provider]
					if req.EnabledProviders == nil || providerOK {
						if pin.LastOutputTokens >= prevTurnMaxedOutThreshold {
							log.Info("Expired session pin maxed out on previous turn; skipping re-anchor",
								"pin_model", pin.Model,
								"pin_provider", pin.Provider,
								"last_output_tokens", pin.LastOutputTokens,
							)
						} else if req.HasImages && !catalog.AcceptsImages(pin.Model) {
							log.Info("Expired session pin is text-only for image-bearing turn; skipping re-anchor",
								"pin_model", pin.Model,
								"pin_provider", pin.Provider,
							)
						} else {
							priorDecision := pinDecision(pin)
							res.Decision = priorDecision
							res.StickyHit = true
							res.PinTier = "postgres_reanchor"
							s.writeNewPin(ctx, installationID, res.SessionKey, res.PinRole, priorDecision)
							log.Info("router re-anchored expired session pin",
								"prior_model", pin.Model,
								"prior_provider", pin.Provider,
								"fresh_model", fresh.Model,
								"fresh_provider", fresh.Provider,
								"prior_tier", pinTier.String(),
								"fresh_tier", freshTier.String(),
							)
							return res, nil
						}
					}
				}
			}
		}
	}

	if !s.ResolvePlannerEnabled(ctx) {
		res.Decision = fresh
		s.writeNewPin(ctx, installationID, res.SessionKey, res.PinRole, fresh)
		return res, nil
	}

	plannerTokens := s.plannerTokensFor(env, feats)
	prefixTokens, prefixKnown := cacheablePrefixTokens(pin, plannerTokens, prefixBroken)
	plannerIn := planner.Inputs{
		Pin:                   pin,
		Fresh:                 fresh,
		EstimatedInputTokens:  plannerTokens,
		CacheablePrefixTokens: prefixTokens,
		CachePrefixKnown:      prefixKnown,
		PriorOutputTokens:     pin.LastOutputTokens,
		AvailableModels:       s.availableModels,
		// A trimmed prefix kills the cache even inside the provider TTL.
		PinCacheCold: pinFound && pinCacheCold(pin, prefixBroken),
		// Applies the subsidy discount to pinned sessions too, not just fresh
		// decisions. nil when subscription-aware routing is off.
		SubsidizedCostFactor: req.SubsidizedModelCostFactor,
	}
	if !pinFound {
		plannerIn.Pin = sessionpin.Pin{}
	}
	decision := planner.Decide(plannerIn, s.planner)
	res.PlannerDecision = decision

	if decision.Outcome == planner.OutcomeStay && pinFound {
		anchor := pinDecision(pin)
		// Band swap picks which half of the pinned pair serves this turn; the
		// pin itself stays anchored (refreshed below) so we can swap again next turn.
		served := s.bandSwapServed(ctx, res.TurnType, pin, fresh, req.HasImages, req.EnabledProviders, req.ExcludedModels)
		res.Decision = served
		res.StickyHit = true
		res.PinTier = "postgres_stay_" + decision.Reason
		s.refreshPin(ctx, installationID, res.SessionKey, pin, res.PinRole, anchor)
		return res, nil
	}

	// Switch path: attempt bounded-cost handover off a warm cache. Any
	// summarizer failure keeps the full prior history rather than trimming —
	// an expensive switch turn beats silently dropping context.
	//
	// Privacy guard: the summarizer runs on deployment-level creds by default,
	// which would cross the tenant boundary for a BYOK/client request. Prefer
	// the caller's own forwarded creds for the summarizer's provider when
	// available; skip summarization (pass full history through) only when the
	// request is BYOK/client-keyed with no matching creds forwarded.
	if pinFound && prefixBroken {
		// Client already trimmed its own history — summarizing again is pure
		// cost, so forward unchanged.
		log.Info("Handover summarizer skipped: client history trim already bounded this switch turn",
			"pin_model", pin.Model,
			"fresh_model", fresh.Model,
		)
	}
	if pinFound && !prefixBroken {
		var (
			sumProvider       string
			sumCreds          *Credentials
			canCallSummarizer bool
		)
		if s.summarizer != nil {
			sumProvider = s.summarizer.Provider()
			sumCreds = resolveSummarizerCreds(ctx, sumProvider, reqHeaders)
			nonDepCreds := s.requestUsesNonDeploymentCreds(ctx, reqHeaders)
			canCallSummarizer = sumCreds != nil || !nonDepCreds
		}
		switch {
		case s.summarizer == nil:
			res.Handover.Invoked = true
			res.Handover.FallbackToFullHistory = true
			log.Info("Handover summarizer not wired; preserved full history instead", "pin_model", pin.Model, "fresh_model", fresh.Model)
		case !canCallSummarizer:
			res.Handover.Invoked = true
			res.Handover.FallbackToFullHistory = true
			log.Info("Handover summarizer skipped to preserve tenant boundary; preserved full history instead", "pin_model", pin.Model, "fresh_model", fresh.Model, "sum_provider", sumProvider)
		default:
			summCtx := ctx
			if sumCreds != nil {
				summCtx = context.WithValue(ctx, CredentialsContextKey{}, sumCreds)
			} else {
				// Strip any request credential (e.g. subscription OAuth token)
				// so this synthetic call doesn't inherit it and 401/cross tenants.
				summCtx = clearCredentials(ctx)
			}
			start := time.Now()
			summary, summaryUsage, sumErr := s.summarizer.Summarize(summCtx, env)
			res.Handover.Invoked = true
			res.Handover.LatencyMS = time.Since(start).Milliseconds()
			switch {
			case sumErr != nil:
				res.Handover.FallbackToFullHistory = true
				log.Warn("Handover summarizer failed; preserved full history instead", "err", sumErr, "pin_model", pin.Model, "fresh_model", fresh.Model)
			case summary == "":
				res.Handover.FallbackToFullHistory = true
				log.Warn("Handover summarizer returned empty summary; preserved full history instead", "pin_model", pin.Model, "fresh_model", fresh.Model)
			default:
				handover.RewriteEnvelope(env, summary)
				res.Handover.SummaryTokens = estimateSummaryTokens(summary)
				res.Handover.SummaryUsage = summaryUsage
			}
		}
	}

	res.Decision = fresh
	if pinFound {
		res.PinTier = "switch_" + decision.Reason
	}
	s.writeNewPin(ctx, installationID, res.SessionKey, res.PinRole, fresh)
	return res, nil
}

func (s *Service) hmmCostGatedDecision(
	req router.Request,
	activePin sessionpin.Pin,
	hmmHistory sessionpin.Pin,
	fresh router.Decision,
	estimatedInputTokens int,
	prefixBroken bool,
) (router.Decision, planner.Decision, bool, string) {
	stayPin, ok := s.hmmStayPin(req, activePin, hmmHistory)
	if !ok {
		return fresh, planner.Decision{Outcome: planner.OutcomeSwitch, Reason: planner.ReasonNoPin}, false, ""
	}
	if hmmToolExecutionPhaseChanged(stayPin.Reason, fresh.Reason) {
		return fresh, planner.Decision{Outcome: planner.OutcomeSwitch, Reason: hmmReasonPhaseChange}, false, stayPin.Model
	}

	cfg := s.planner
	// HMM owns semantic selection; the shared Go planner owns cache economics
	// because HMM clusters and catalog tiers are not the same axis.
	cfg.TierUpgradeEnabled = false
	stayPrefix, stayPrefixKnown := cacheablePrefixTokens(stayPin, estimatedInputTokens, prefixBroken)
	base := planner.Decide(planner.Inputs{
		Pin:                   stayPin,
		Fresh:                 fresh,
		EstimatedInputTokens:  estimatedInputTokens,
		CacheablePrefixTokens: stayPrefix,
		CachePrefixKnown:      stayPrefixKnown,
		PriorOutputTokens:     stayPin.LastOutputTokens,
		AvailableModels:       s.availableModels,
		PinCacheCold:          pinCacheCold(stayPin, prefixBroken),
		SubsidizedCostFactor:  req.SubsidizedModelCostFactor,
	}, cfg)

	if hmmFreshIsMoreExpensive(stayPin.Model, fresh.Model, req.SubsidizedModelCostFactor) {
		confidence, ok := hmmDecisionConfidence(fresh)
		if ok && confidence >= s.hmmUpgradeConfidenceThreshold {
			base.Outcome = planner.OutcomeSwitch
			base.Reason = hmmReasonConfidentUpgrade
		} else if base.Outcome != planner.OutcomeSwitch {
			base.Outcome = planner.OutcomeStay
			base.Reason = hmmReasonUpgradeConfidenceLow
		}
	}

	// Runs after the upgrade block, so only a plain ReasonEVPositive switch
	// reaches here; a confident upgrade has already changed Reason.
	if s.hmmSameTierPin && base.Outcome == planner.OutcomeSwitch && base.Reason == planner.ReasonEVPositive {
		pinTier, freshTier := catalog.TierFor(stayPin.Model), catalog.TierFor(fresh.Model)
		if pinTier != catalog.TierUnknown && pinTier == freshTier {
			base.Outcome = planner.OutcomeStay
			base.Reason = planner.ReasonSameTierPinned
		}
	}

	if base.Outcome == planner.OutcomeStay && stayPin.Model != fresh.Model {
		return pinDecision(stayPin), base, true, stayPin.Model
	}
	return fresh, base, false, stayPin.Model
}

// authorityCacheShadowFor computes the HMM cache gate's verdict for an
// authoritative-per-turn turn without changing the served decision. Calls
// hmmCostGatedDecision unmodified -- a shadow that approximates the rule is not
// a preview of it, and that function is pure.
func (s *Service) authorityCacheShadowFor(
	ctx context.Context,
	req router.Request,
	activePin sessionpin.Pin,
	hmmHistory sessionpin.Pin,
	fresh router.Decision,
	estimatedInputTokens int,
	prefixBroken bool,
) authorityCacheShadow {
	if !s.ResolveAuthorityCacheShadow(ctx) {
		return authorityCacheShadow{}
	}
	// The gate's rules are HMM-specific: hmmStayPin only accepts HMM-written
	// pins and the upgrade override reads a sidecar confidence, so a verdict
	// against a non-HMM decision describes a rollout that cannot happen.
	if !isHMMDecision(fresh) {
		return authorityCacheShadow{}
	}
	_, plannerDecision, sticky, stayModel := s.hmmCostGatedDecision(
		req, activePin, hmmHistory, fresh, estimatedInputTokens, prefixBroken,
	)
	// Re-resolve the provider; guard on model equality because normalizeHMMStayPin
	// consults the clock and a concurrently expiring pin must not pair a stale
	// binding with the model the gate actually priced.
	stayProvider := activePin.Provider
	stayPin, stayPinOK := s.hmmStayPin(req, activePin, hmmHistory)
	if stayPinOK && stayPin.Model == stayModel {
		stayProvider = stayPin.Provider
	}
	freshRosterArmID := ""
	if fresh.Metadata != nil {
		freshRosterArmID = fresh.Metadata.SelectedRosterArmID
	}
	shadow := authorityCacheShadow{
		Computed:   true,
		Decision:   plannerDecision,
		StayModel:  stayModel,
		Sticky:     sticky,
		StayScore:  candidateScoreForWithProvider(fresh, stayModel, stayProvider),
		FreshScore: candidateScoreForWithArm(fresh, fresh.ServedIdentity(), fresh.Provider, freshRosterArmID),
	}
	if stayPinOK && stayPin.Model == stayModel {
		shadow.StayProvider = stayPin.Provider
	}
	return shadow
}

// candidateScoreFor reads the sidecar's catalog-level score for servedIdentity.
// Returns nil when the sidecar reported no score -- nil must not be coerced to 0.
func candidateScoreFor(dec router.Decision, servedIdentity string) *float64 {
	return candidateScoreForWithProvider(dec, servedIdentity, dec.Provider)
}

// candidateScoreForWithProvider falls back to the sidecar's per-arm WMI score
// when the catalog-level score vector is absent. AA roster policies expose WMI
// scores as arm_scores (provider/model[:effort]) rather than candidate_scores.
func candidateScoreForWithProvider(dec router.Decision, servedIdentity, provider string) *float64 {
	return candidateScoreForWithArm(dec, servedIdentity, provider, "")
}

func candidateScoreForWithArm(dec router.Decision, servedIdentity, provider, rosterArmID string) *float64 {
	if servedIdentity == "" || dec.Metadata == nil {
		return nil
	}
	if score, ok := dec.Metadata.CandidateScores[baseModelOf(servedIdentity)]; ok {
		value := float64(score)
		return &value
	}

	if rosterArmID != "" {
		if score, ok := dec.Metadata.ArmScores[rosterArmID]; ok {
			value := float64(score)
			return &value
		}
	}

	effort, model := stripEffortSuffix(servedIdentity)
	if model == "" || len(dec.Metadata.ArmScores) == 0 {
		return nil
	}
	armSuffix := ""
	if effort != "" {
		armSuffix = ":" + effort
	}
	keys := make([]string, 0, 2)
	if provider != "" {
		keys = append(keys, provider+"/"+model+armSuffix)
	}
	keys = append(keys, model+armSuffix)
	for _, key := range keys {
		if score, ok := dec.Metadata.ArmScores[key]; ok {
			value := float64(score)
			return &value
		}
	}

	// Gateway providers may use a different namespace from the roster arm
	// (for example, anthropic_gateway serves an anthropic/... arm). Fall back
	// only when the model/effort match is unique; an ambiguous provider match
	// must remain NULL rather than silently assigning another arm's score.
	var fallback *float64
	for armID, score := range dec.Metadata.ArmScores {
		armEffort, armModel := stripEffortSuffix(armID)
		if armEffort != effort || (armModel != model && !strings.HasSuffix(armModel, "/"+model)) {
			continue
		}
		if armProvider, ok := dec.Metadata.CandidateArmProviders[armID]; ok && provider != "" && armProvider == provider {
			value := float64(score)
			return &value
		}
		if fallback != nil {
			return nil
		}
		value := float64(score)
		fallback = &value
	}
	return fallback
}

// logAuthorityCacheShadow emits the shadow verdict as a structured line. The
// Postgres columns are the analysis surface; this exists so a single session can
// be traced in logs without a query, matching logPlannerOutcome.
func (s *Service) logAuthorityCacheShadow(ctx context.Context, res turnLoopResult) {
	if !res.AuthorityShadow.Computed {
		return
	}
	shadow := res.AuthorityShadow
	log := observability.FromContext(ctx)
	log.Info("authoritative turn cache-gate shadow",
		"turn_type", string(res.TurnType),
		"fresh_model", res.Fresh.Model,
		"shadow_outcome", plannerOutcome(shadow.Decision.Outcome),
		"shadow_reason", shadow.Decision.Reason,
		"shadow_stay_model", shadow.StayModel,
		"shadow_would_diverge", shadow.Sticky,
		"shadow_ev_ran", shadow.EVRan(),
	)
	if !shadow.EVRan() {
		// OutcomeStay is the zero value and plannerOutcome renders it "stay", so
		// logging these on an early exit would report a verdict and a cost that
		// were never computed -- and would disagree with the NULL columns.
		return
	}
	log.Info("authoritative turn cache-gate shadow EV",
		"shadow_expected_savings_usd", shadow.Decision.ExpectedSavingsUSD,
		"shadow_eviction_cost_usd", shadow.Decision.EvictionCostUSD,
		"shadow_pin_cache_cold", shadow.Decision.PinCacheCold,
		"shadow_corrected_outcome", plannerOutcome(shadow.Decision.ShadowOutcome),
		"shadow_corrected_savings_usd", shadow.Decision.ShadowExpectedSavingsUSD,
	)
}

func (s *Service) hmmStayPin(req router.Request, activePin sessionpin.Pin, hmmHistory sessionpin.Pin) (sessionpin.Pin, bool) {
	var (
		best sessionpin.Pin
		ok   bool
	)
	// Only HMM-written pins are stay candidates; a cluster/planner pin from a
	// prior non-HMM stretch must not steer an HMM EV stay.
	if !isHMMPinReason(activePin.Reason) {
		activePin = sessionpin.Pin{}
	}
	for _, candidate := range []sessionpin.Pin{activePin, hmmHistory} {
		normalized, candidateOK := s.normalizeHMMStayPin(req, candidate)
		if !candidateOK {
			continue
		}
		if !ok || normalized.LastTurnEndedAt.After(best.LastTurnEndedAt) {
			best = normalized
			ok = true
		}
	}
	return best, ok
}

// isHMMPinReason reports whether reason is HMM-written (hmm_history or hmm_policy*);
// guards against a stale cluster/planner pin steering an HMM turn's EV stay.
func isHMMPinReason(reason string) bool {
	return reason == hmmHistoryReason ||
		strings.HasPrefix(strings.TrimSpace(reason), "hmm_policy")
}

func isHMMToolExecutionReason(reason string) bool {
	return strings.HasPrefix(strings.TrimSpace(reason), "hmm_policy:tool_execution")
}

func hmmToolExecutionPhaseChanged(stayReason, freshReason string) bool {
	return isHMMToolExecutionReason(stayReason) != isHMMToolExecutionReason(freshReason)
}

func (s *Service) normalizeHMMStayPin(req router.Request, p sessionpin.Pin) (sessionpin.Pin, bool) {
	model := p.LastServedModel
	if model == "" {
		model = p.Model
	}
	if model == "" {
		return sessionpin.Pin{}, false
	}
	if p.LastTurnEndedAt.IsZero() {
		return sessionpin.Pin{}, false
	}
	if !p.PinnedUntil.IsZero() && !p.PinnedUntil.After(time.Now()) {
		return sessionpin.Pin{}, false
	}
	if p.LastOutputTokens >= prevTurnMaxedOutThreshold {
		return sessionpin.Pin{}, false
	}
	if req.ExcludedModels != nil {
		if _, excluded := req.ExcludedModels[model]; excluded {
			return sessionpin.Pin{}, false
		}
	}
	// An HMM stay is the policy re-choosing the incumbent, so a deployment-wide
	// disable applies to it exactly as it does to a fresh selection.
	if automaticallyDisabled(req, model) {
		return sessionpin.Pin{}, false
	}
	if s.availableModels != nil {
		if _, available := s.availableModels[model]; !available {
			return sessionpin.Pin{}, false
		}
	}
	if req.HasImages && !catalog.AcceptsImages(model) {
		return sessionpin.Pin{}, false
	}
	p.Model = model
	providerSet := req.EnabledProviders
	if providerSet == nil {
		providerSet = make(map[string]struct{}, len(s.providers))
		for provider := range s.providers {
			providerSet[provider] = struct{}{}
		}
	}
	// A failed turn preserves the prior model but may leave an invalid provider
	// binding; validate before reusing, or re-resolve against available providers.
	if p.Provider != "" {
		if _, enabled := providerSet[p.Provider]; enabled {
			pinned := map[string]struct{}{p.Provider: {}}
			if _, valid := catalog.ResolveBindingWithCustom(model, pinned, req.CustomBindings); valid {
				return p, true
			}
		}
	}
	binding, ok := catalog.ResolveBindingWithCustom(model, providerSet, req.CustomBindings)
	if !ok {
		return sessionpin.Pin{}, false
	}
	p.Provider = binding.Provider
	return p, true
}

func hmmDecisionConfidence(dec router.Decision) (float64, bool) {
	if dec.Metadata == nil {
		return 0, false
	}
	confidence := float64(dec.Metadata.ChosenScore)
	if confidence <= 0 {
		return 0, false
	}
	return confidence, true
}

func hmmFreshIsMoreExpensive(stayModel, freshModel string, factors map[string]float64) bool {
	stay, okStay := hmmEffectiveInputUSDPer1M(stayModel, factors)
	fresh, okFresh := hmmEffectiveInputUSDPer1M(freshModel, factors)
	return okStay && okFresh && fresh > stay
}

func hmmEffectiveInputUSDPer1M(model string, factors map[string]float64) (float64, bool) {
	price, ok := catalog.PrimaryPriceFor(model)
	if !ok {
		return 0, false
	}
	value := price.InputUSDPer1M
	if factor, covered := factors[model]; covered {
		value *= factor
	}
	return value, true
}

func maxedOutServedModel(pin sessionpin.Pin) string {
	if pin.LastOutputTokens < prevTurnMaxedOutThreshold {
		return ""
	}
	model := pin.LastServedModel
	if model == "" {
		model = pin.Model
	}
	// ExcludedModels / SafetyExcludedModels key on bare catalog IDs; strip effort
	// so the exclusion matches. Exclusion is model-level: saturate at any effort
	// and the model is barred at every effort for this turn.
	return baseModelOf(model)
}

// baseModelOf strips a trailing ":effort" from a serving identity, returning the
// bare catalog ID. Safe on inputs that carry no effort.
func baseModelOf(servedIdentity string) string {
	if idx := strings.LastIndex(servedIdentity, ":"); idx > 0 {
		return servedIdentity[:idx]
	}
	return servedIdentity
}

// roleForTier maps a requested-model tier to its session-pin role. Each tier
// gets its own row so separate-tier turns never share a pin. TierUnknown
// falls back to DefaultRole.
func roleForTier(t catalog.Tier) string {
	switch t {
	case catalog.TierLow:
		return sessionpin.DefaultRole + "_low"
	case catalog.TierMid:
		return sessionpin.DefaultRole + "_mid"
	case catalog.TierHigh:
		return sessionpin.DefaultRole + "_high"
	default:
		return sessionpin.DefaultRole
	}
}

// loadPin returns the stored pin and whether it may actively serve this turn.
// Expired rows are misses for routing, but their history fields still protect
// Anthropic emit from stale thinking-block signatures in the client transcript.
func (s *Service) loadPin(ctx context.Context, sessionKey [sessionpin.SessionKeyLen]byte, role string) (sessionpin.Pin, bool) {
	log := observability.FromContext(ctx)
	log.Debug("loadPin called", "role", role, "session_key_hex", fmt.Sprintf("%x", sessionKey))
	pin, found, err := s.pinStore.Get(ctx, sessionKey, role)
	if err != nil {
		log.Error("session pin store unavailable; falling through to cluster scorer", "err", err)
		return sessionpin.Pin{}, false
	}
	if !found {
		return sessionpin.Pin{}, false
	}
	if !pinMatchesEffectiveStrategy(ctx, pin) {
		return sessionpin.Pin{}, false
	}
	if !pin.PinnedUntil.After(time.Now()) {
		return pin, false
	}
	return pin, true
}

func (s *Service) loadHMMHistory(ctx context.Context, sessionKey [sessionpin.SessionKeyLen]byte, role string) sessionpin.Pin {
	log := observability.FromContext(ctx)
	pin, found, err := s.pinStore.Get(ctx, sessionKey, hmmHistoryRole(role))
	if err != nil {
		log.Error("HMM switch-history store unavailable", "err", err)
		return sessionpin.Pin{}
	}
	if !found {
		return sessionpin.Pin{}
	}
	if !pinMatchesEffectiveStrategy(ctx, pin) {
		return sessionpin.Pin{}
	}
	return pin
}

func switchHistoryFromPins(pins ...sessionpin.Pin) (string, bool) {
	var latest sessionpin.Pin
	sessionEverSwitched := false
	seenModel := ""
	for _, pin := range pins {
		sessionEverSwitched = sessionEverSwitched || pin.HasEverSwitched
		if pin.LastServedModel == "" {
			continue
		}
		if seenModel != "" && seenModel != pin.LastServedModel {
			sessionEverSwitched = true
		}
		seenModel = pin.LastServedModel
		if latest.LastServedModel == "" || pin.LastTurnEndedAt.After(latest.LastTurnEndedAt) {
			latest = pin
		}
	}
	return latest.LastServedModel, sessionEverSwitched
}

func buildPolicyTurnContext(
	req router.Request,
	res turnLoopResult,
	activePin sessionpin.Pin,
	hmmHistory sessionpin.Pin,
) *router.PolicyTurnContext {
	previous := activePin
	if hmmHistory.LastServedModel != "" &&
		(previous.LastServedModel == "" ||
			hmmHistory.LastTurnEndedAt.After(previous.LastTurnEndedAt)) {
		previous = hmmHistory
	}
	cacheState := router.PolicyCacheStateUnknown
	var priorOutputTokens *int
	if !previous.LastTurnEndedAt.IsZero() {
		cacheState = router.PolicyCacheStateCold
		if cacheWarm(previous) && !res.PrefixTrimmed && !req.HistoryTruncated {
			cacheState = router.PolicyCacheStateWarm
		}
		outputTokens := previous.LastOutputTokens
		priorOutputTokens = &outputTokens
	}
	userTurns := 0
	for _, message := range req.ConversationMessages {
		if strings.EqualFold(message.Role, "user") {
			userTurns++
		}
	}
	visibleTurnIndex := max(userTurns-1, 0)
	sessionTurnCount := max(activePin.TurnCount, hmmHistory.TurnCount)
	previousProvider := ""
	if res.PriorServedModel != "" {
		previousProvider = previous.Provider
	}
	return &router.PolicyTurnContext{
		VisibleTurnIndex:    visibleTurnIndex,
		SessionTurnCount:    sessionTurnCount,
		TurnType:            string(res.TurnType),
		PreviousServedModel: res.PriorServedModel,
		PreviousProvider:    previousProvider,
		CacheState:          cacheState,
		PriorOutputTokens:   priorOutputTokens,
		SessionEverSwitched: res.SessionEverSwitched,
		HistoryTruncated:    req.HistoryTruncated || res.PrefixTrimmed,
	}
}

// refreshPin extends the TTL on an existing pin. Carries the existing pin's
// usage forward so the planner has evidence before the next UpdateUsage
// writeback lands.
func (s *Service) refreshPin(ctx context.Context, installationID uuid.UUID, sessionKey [sessionpin.SessionKeyLen]byte, existing sessionpin.Pin, role string, chosen router.Decision) {
	if installationID == uuid.Nil {
		return
	}
	effort := chosen.Effort
	if effort == "" && chosen.Model == existing.Model {
		effort = existing.Effort
	}
	p := sessionpin.Pin{
		SessionKey:     sessionKey,
		Role:           role,
		InstallationID: installationID,
		Provider:       chosen.Provider,
		Model:          chosen.Model,
		Effort:         effort,
		// No scorer runs on a plain refresh, so carry the existing pair
		// forward unchanged (ON CONFLICT preserves an empty one).
		PairedProvider: existing.PairedProvider,
		PairedModel:    existing.PairedModel,
		Reason:         chosen.Reason,
		Strategy:       router.StrategyFromContext(ctx),
		// Same rationale as the pair above: a refresh runs no policy, so the
		// reconstructed decision carries no group. Carry the stored one forward.
		PolicyGroup:           existing.PolicyGroup,
		TurnCount:             1,
		PinnedUntil:           pinExpiry(chosen.Reason),
		LastInputTokens:       existing.LastInputTokens,
		LastCachedReadTokens:  existing.LastCachedReadTokens,
		LastCachedWriteTokens: existing.LastCachedWriteTokens,
		LastOutputTokens:      existing.LastOutputTokens,
		LastTurnEndedAt:       existing.LastTurnEndedAt,
		LastServedModel:       existing.LastServedModel,
	}
	s.upsertPin(ctx, p)
}

// writeNewPin records a freshly-routed decision as the active pin. Used on
// first-turn routing and switch turns. UpdateUsage fills in usage stats later.
func (s *Service) writeNewPin(ctx context.Context, installationID uuid.UUID, sessionKey [sessionpin.SessionKeyLen]byte, role string, chosen router.Decision) {
	log := observability.FromContext(ctx)
	// pinDecision(pin) reconstructions carry no Metadata, so the nil guard
	// leaves the pair empty; ON CONFLICT then preserves the stored pair
	// instead of wiping it.
	var pairedProvider, pairedModel string
	if chosen.Metadata != nil {
		pairedProvider = chosen.Metadata.PairedProvider
		pairedModel = chosen.Metadata.PairedModel
	}
	log.Info("writeNewPin called", "installation_id", installationID.String(), "role", role, "model", chosen.Model, "paired_model", pairedModel, "paired_provider", pairedProvider, "session_key_hex", fmt.Sprintf("%x", sessionKey))
	if installationID == uuid.Nil {
		log.Info("writeNewPin: skipping because installationID is uuid.Nil")
		return
	}
	p := sessionpin.Pin{
		SessionKey:     sessionKey,
		Role:           role,
		InstallationID: installationID,
		Provider:       chosen.Provider,
		Model:          chosen.Model,
		Effort:         chosen.Effort,
		PairedProvider: pairedProvider,
		PairedModel:    pairedModel,
		Reason:         chosen.Reason,
		Strategy:       router.StrategyFromContext(ctx),
		PolicyGroup:    decisionPolicyGroup(chosen),
		TurnCount:      1,
		PinnedUntil:    pinExpiry(chosen.Reason),
	}
	s.upsertPin(ctx, p)
}

// upsertPin synchronously persists a pin write. context.Background() is used
// so the DB write survives request-ctx cancellation after the response has
// finished streaming.
func (s *Service) upsertPin(ctx context.Context, p sessionpin.Pin) {
	log := observability.FromContext(ctx)
	if p.Strategy == "" {
		p.Strategy = router.StrategyFromContext(ctx)
	}
	if err := s.pinStore.Upsert(context.Background(), p); err != nil {
		log.Error("session pin upsert failed", "err", err)
		return
	}
	log.Debug("session pin upsert ok", "installation_id", p.InstallationID.String(), "role", p.Role, "model", p.Model)
}

// estimateSummaryTokens is a rough char/4 estimate. The summarizer
// adapter doesn't expose a tokenizer and the value is only used for
// OTel/log attribution where order-of-magnitude is enough.
func estimateSummaryTokens(s string) int {
	if s == "" {
		return 0
	}
	return len(s) / 4
}

// resolveSummarizerCreds returns BYOK or client-supplied credentials for
// provider so the handover orchestrator can summarize on the caller's own
// account instead of crossing the deployment-key tenant boundary. Returns nil
// if no caller creds exist; callers then use the deployment key or skip summarization.
func resolveSummarizerCreds(ctx context.Context, provider string, headers http.Header) *Credentials {
	if provider == "" {
		return nil
	}
	if byok := BuildCredentialsMap(externalKeysFromContext(ctx)); byok != nil {
		if creds, ok := byok[provider]; ok {
			return creds
		}
	}
	creds := ExtractClientCredentials(provider, headers)
	if creds != nil && creds.OAuth {
		// A Claude subscription token can't authenticate the synthetic
		// summarizer call (no Claude Code identity block) and would 401.
		return nil
	}
	return creds
}

// sortedEnabledKeys returns a deterministic slice of the keys in m for
// log-line attribution. nil/empty map yields an empty slice.
func sortedEnabledKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
