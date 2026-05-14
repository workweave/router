package proxy

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"workweave/router/internal/observability"
	"workweave/router/internal/router"
	"workweave/router/internal/router/capability"
	"workweave/router/internal/router/cluster"
	"workweave/router/internal/router/handover"
	"workweave/router/internal/router/planner"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/router/turntype"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
)

// installationIDFromContext reads and parses the installation ID stashed by
// auth middleware. Returns uuid.Nil for unauthenticated or invalid values;
// both skip the async pin upsert downstream.
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

// turnLoopResult bundles the routing decision and pin/planner state for
// OTel spans and structured log lines.
type turnLoopResult struct {
	Decision   router.Decision
	SessionKey [sessionpin.SessionKeyLen]byte
	TurnType   turntype.TurnType
	StickyHit  bool
	HardPinned bool
	PinTier    string
	PinAgeSec  int64
	// RequestedTier is the tier of the inbound requested model, looked up
	// directly via capability.TierFor (no baseline substitution). Drives
	// the tier-ceiling clamp; logged as `requested_tier`. TierUnknown
	// disables clamping, which is the right behavior for custom/proxy
	// model names (e.g. "weave-router") — substituting through
	// baselineFor would force them into the default model's tier
	// (TierMid) and clamp high-tier scorer picks despite the documented
	// "unknown ⇒ no ceiling" rule.
	RequestedTier capability.Tier
	// TierClamped is true when the original decision violated the
	// requested-model ceiling and was rewritten to an in-ceiling pick.
	TierClamped bool
	// PreClampModel records the violating model so logs can attribute
	// which model the scorer / hard-pin / pin actually wanted to use.
	PreClampModel string
	// PinRole is the session-pin role used for this turn. Threaded through
	// loadPin/refreshPin/writeNewPin and the post-response UpdateUsage call
	// so a low-tier background turn and a high-tier main turn in the same
	// session never share a pin row.
	PinRole string
	// Fresh is the scorer's recommendation for this turn when the scorer
	// ran. Zero-valued on hard-pin / tool-result-with-pin paths where we
	// never consulted the scorer.
	Fresh router.Decision
	// PlannerDecision holds the planner's verdict and EV math when the
	// planner ran. Zero-valued on hard-pin / tool-result / planner-
	// disabled / no-pin-store paths.
	PlannerDecision planner.Decision
	// PinModel is the model on the loaded pin, when one existed. Stamped
	// independently of PlannerDecision so log lines can describe the
	// from-model even on stay outcomes.
	PinModel string
	// Handover captures what happened in the cache-eviction path. Invoked
	// is true only when the planner switched off an existing pin and the
	// orchestrator attempted a summarize-or-trim step.
	Handover handoverOutcome
}

// handoverOutcome describes the synchronous handover step.
type handoverOutcome struct {
	Invoked        bool
	LatencyMS      int64
	SummaryTokens  int
	FallbackToTrim bool
}

// runTurnLoop is the format-agnostic Prism-style routing orchestrator.
// See the package-level docs (and the routing plan) for the flow; in
// brief: detect turn type, short-circuit hard pins, load any existing
// pin, run the scorer for MainLoop turns, hand the result to the
// planner, and on switch attempt bounded-cost handover.
//
// installationID == uuid.Nil skips the async pin upsert (pin rows
// require an installation_id); the rest of the path runs normally.
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
	log := observability.Get()
	res := turnLoopResult{
		TurnType:      turntype.DetectFromEnvelope(env, feats, subAgentHint),
		PinTier:       "miss",
		RequestedTier: capability.TierFor(feats.Model),
	}
	res.PinRole = roleForTier(res.RequestedTier)

	// Hard pins for turn types whose optimal model is known a priori
	// (compaction, Explore sub-agent dispatch, SDK quota probes, Claude
	// Code title generation). Bypass pin lookup, pin write, planner and
	// scorer entirely so the pin row keeps tracking the main-loop model.
	// Probes and title-gen specifically MUST NOT create a session pin —
	// the Anthropic SDK fires probes on init before the first real user
	// turn, and Claude Code fires a title-gen call ~25ms before the
	// real-conv call on every user turn. In both cases an anchored pin
	// would inherit the cheap-model decision into the immediately-
	// following real conversation that should have routed on its own.
	if res.TurnType == turntype.Compaction ||
		res.TurnType == turntype.Probe ||
		res.TurnType == turntype.TitleGen ||
		res.TurnType == turntype.Classifier ||
		(res.TurnType == turntype.SubAgentDispatch && s.hardPinExplore) {
		provider, model := s.hardPinProvider, s.hardPinModel
		// In byokOnly mode the boot-time hard-pin is unsafe: it was
		// computed over every registered provider, but the request may
		// only have BYOK credentials for a subset. Resolve per-request
		// against the request's enabled-providers set so compaction
		// stays on a provider the request can authenticate to.
		if s.hardPinResolver != nil {
			p, m, ok := s.hardPinResolver(req.EnabledProviders)
			if !ok {
				log.Warn(
					"Hard-pin: no eligible provider for request; returning ErrClusterUnavailable",
					"turn_type", string(res.TurnType),
					"enabled_providers", sortedEnabledKeys(req.EnabledProviders),
				)
				return res, fmt.Errorf("hard-pin: no eligible provider for %s: %w", res.TurnType, cluster.ErrClusterUnavailable)
			}
			provider, model = p, m
		}
		// Operator hard-pins bypass the tier ceiling by design — the
		// ROUTER_HARD_PIN_MODEL env var is an explicit operator opt-in
		// that wins over the requested-model ceiling. Clamping here
		// would silently rewrite an unknown-tier hard-pin to the
		// cheapest in-ceiling alternative, defeating the operator's
		// stated intent.
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

	// Without a pin store there is no pin lifecycle to manage; degrade to
	// "run the scorer, return its decision, persist nothing".
	if s.pinStore == nil {
		decision, err := s.router.Route(ctx, req)
		if err != nil {
			return res, err
		}
		decision = s.clampToCeiling(decision, res.RequestedTier, req.EnabledProviders, req.ExcludedModels, &res)
		res.Decision = decision
		res.Fresh = decision
		return res, nil
	}

	res.SessionKey = DeriveSessionKey(env, apiKeyID)
	pinCacheKey := sessionPinCacheKey(res.SessionKey, res.PinRole)

	pin, pinFound := s.loadPin(ctx, pinCacheKey, res.SessionKey, res.PinRole)
	if pinFound {
		res.PinModel = pin.Model
		res.PinAgeSec = pinAge(pin)
	}

	// Tool-result turns are mid-turn continuations. Re-routing them on
	// trailing tool_result embedding flips decisions to noisy candidates;
	// reuse the pin verbatim when present and refresh the TTL.
	if res.TurnType == turntype.ToolResult && pinFound {
		decision := s.clampToCeiling(pinDecision(pin), res.RequestedTier, req.EnabledProviders, req.ExcludedModels, &res)
		res.Decision = decision
		res.StickyHit = true
		res.PinTier = "postgres_tool_result_sc"
		s.refreshPin(installationID, res.SessionKey, pin, pinCacheKey, res.PinRole, decision)
		return res, nil
	}

	// Planner-disabled + pin found: preserve "first decision wins"
	// behavior for callers that opt out via env flag. Skip the scorer
	// entirely; the pin's model is authoritative.
	if !s.plannerEnabled && pinFound {
		decision := s.clampToCeiling(pinDecision(pin), res.RequestedTier, req.EnabledProviders, req.ExcludedModels, &res)
		res.Decision = decision
		res.StickyHit = true
		res.PinTier = "postgres"
		s.refreshPin(installationID, res.SessionKey, pin, pinCacheKey, res.PinRole, decision)
		return res, nil
	}

	// MainLoop (or ToolResult without an existing pin, or planner-
	// disabled without a pin): always run the scorer. The scorer's
	// error path surfaces to HTTP 503 in the presentation layer.
	fresh, err := s.router.Route(ctx, req)
	if err != nil {
		return res, err
	}
	fresh = s.clampToCeiling(fresh, res.RequestedTier, req.EnabledProviders, req.ExcludedModels, &res)
	res.Fresh = fresh

	// Planner-disabled with no pin: take the fresh decision and write
	// a new pin row so subsequent turns see it.
	if !s.plannerEnabled {
		res.Decision = fresh
		s.writeNewPin(installationID, res.SessionKey, pinCacheKey, res.PinRole, fresh)
		return res, nil
	}

	// Planner path: weigh pin vs. fresh under the EV policy.
	plannerIn := planner.Inputs{
		Pin:                  pin,
		Fresh:                fresh,
		EstimatedInputTokens: feats.Tokens,
		AvailableModels:      s.availableModels,
	}
	if !pinFound {
		plannerIn.Pin = sessionpin.Pin{}
	}
	decision := planner.Decide(plannerIn, s.planner)
	res.PlannerDecision = decision

	if decision.Outcome == planner.OutcomeStay && pinFound {
		stay := s.clampToCeiling(pinDecision(pin), res.RequestedTier, req.EnabledProviders, req.ExcludedModels, &res)
		res.Decision = stay
		res.StickyHit = true
		res.PinTier = "postgres_stay_" + decision.Reason
		s.refreshPin(installationID, res.SessionKey, pin, pinCacheKey, res.PinRole, stay)
		return res, nil
	}

	// Switch path: either explicit outcome=switch, or (defensively)
	// outcome=stay with no pin to actually stay on. When switching off an
	// existing warm cache, attempt bounded-cost handover: synchronously
	// summarize prior context with the cheap-model summarizer; on error
	// fall back to TrimLastN so the switch turn still succeeds. When the
	// summarizer is not wired (self-hoster with no Anthropic key) or the
	// request carries BYOK/client credentials, skip summarization and
	// trim instead — either way the switch turn must NOT forward the
	// full prior conversation to the new model (defeats the cost-bounding
	// goal of handover).
	//
	// Privacy guard: the summarizer is wired at boot with deployment-level
	// provider credentials. Calling it on a request whose upstream call
	// would otherwise use BYOK or client-supplied credentials would route
	// prior conversation context (carried verbatim into the summarizer
	// prompt) through the platform account, violating tenant data
	// boundaries.
	if pinFound {
		switch {
		case s.summarizer == nil:
			elided := handover.TrimLastN(env, 3)
			res.Handover.Invoked = true
			res.Handover.FallbackToTrim = true
			log.Info("Handover summarizer not wired; trimmed envelope instead", "elided_messages", elided, "pin_model", pin.Model, "fresh_model", fresh.Model)
		case s.requestUsesNonDeploymentCreds(ctx, reqHeaders):
			elided := handover.TrimLastN(env, 3)
			res.Handover.Invoked = true
			res.Handover.FallbackToTrim = true
			log.Info("Handover summarizer skipped to preserve BYOK tenant boundary; trimmed envelope instead", "elided_messages", elided, "pin_model", pin.Model, "fresh_model", fresh.Model)
		default:
			start := time.Now()
			summary, sumErr := s.summarizer.Summarize(ctx, env)
			res.Handover.Invoked = true
			res.Handover.LatencyMS = time.Since(start).Milliseconds()
			switch {
			case sumErr != nil:
				elided := handover.TrimLastN(env, 3)
				res.Handover.FallbackToTrim = true
				log.Warn("Handover summarizer failed; trimmed envelope instead", "err", sumErr, "elided_messages", elided, "pin_model", pin.Model, "fresh_model", fresh.Model)
			case summary == "":
				elided := handover.TrimLastN(env, 3)
				res.Handover.FallbackToTrim = true
				log.Warn("Handover summarizer returned empty summary; trimmed envelope instead", "elided_messages", elided, "pin_model", pin.Model, "fresh_model", fresh.Model)
			default:
				handover.RewriteEnvelope(env, summary)
				res.Handover.SummaryTokens = estimateSummaryTokens(summary)
			}
		}
	}

	res.Decision = fresh
	if pinFound {
		res.PinTier = "switch_" + decision.Reason
	}
	s.writeNewPin(installationID, res.SessionKey, pinCacheKey, res.PinRole, fresh)
	return res, nil
}

// roleForTier maps a requested-model tier to its session-pin role. Each
// tier gets its own row so a low-tier background turn and a high-tier
// main turn in the same session never share a pin (preventing the
// haiku-inherits-opus-pin leak that originally motivated the tier
// ceiling). TierUnknown falls back to DefaultRole so callers with custom
// model names (no entry in the capability table) keep their pre-ceiling
// behavior.
func roleForTier(t capability.Tier) string {
	switch t {
	case capability.TierLow:
		return sessionpin.DefaultRole + "_low"
	case capability.TierMid:
		return sessionpin.DefaultRole + "_mid"
	case capability.TierHigh:
		return sessionpin.DefaultRole + "_high"
	default:
		return sessionpin.DefaultRole
	}
}

// clampToCeiling enforces the requested-model tier ceiling. When the
// decision's model has a known tier strictly higher than the ceiling,
// the resolver picks the cheapest in-ceiling alternative for the
// request's enabled providers and replaces the decision. Decisions
// already at or below the ceiling pass through. The resolver also
// honors req.ExcludedModels so the clamp path cannot route to models
// the installation/request has explicitly denylisted — otherwise tier
// clamping would silently bypass the model access policy enforced on
// normal scorer routing. TierUnknown ceilings (custom model names with
// no capability entry) disable clamping. Resolver lookup failure (no
// in-ceiling model available) preserves the original decision rather
// than failing the turn — a soft fallback chosen over hard erroring on
// background tasks.
func (s *Service) clampToCeiling(decision router.Decision, ceiling capability.Tier, enabled, excluded map[string]struct{}, res *turnLoopResult) router.Decision {
	// Reset state every call: the orchestrator clamps multiple decision
	// sources per turn (fresh scorer output, then planner-stay pin), and
	// without this reset a clamp on `fresh` would leak `TierClamped=true`
	// + `PreClampModel` into a subsequent unclamped pin decision, falsely
	// labeling the final decision as clamped in the structured log.
	res.TierClamped = false
	res.PreClampModel = ""
	if s.tierClampResolver == nil || ceiling == capability.TierUnknown {
		return decision
	}
	if capability.IsAtOrBelow(decision.Model, ceiling) {
		return decision
	}
	p, m, ok := s.tierClampResolver(enabled, excluded, ceiling)
	if !ok {
		return decision
	}
	res.TierClamped = true
	res.PreClampModel = decision.Model
	return router.Decision{
		Provider: p,
		Model:    m,
		Reason:   decision.Reason + "+tier_clamp",
	}
}

// loadPin returns the active pin for this session, consulting the in-proc
// LRU first then Postgres. Expired rows are treated as misses on both
// tiers. A store error is logged and surfaces as "not found" so the
// caller falls through to the planner with a zero pin.
func (s *Service) loadPin(ctx context.Context, pinCacheKey string, sessionKey [sessionpin.SessionKeyLen]byte, role string) (sessionpin.Pin, bool) {
	log := observability.Get()
	log.Debug("loadPin called", "role", role, "session_key_hex", fmt.Sprintf("%x", sessionKey))
	if s.pinCache != nil {
		if pin, ok := s.pinCache.Get(pinCacheKey); ok {
			// Mirror the Postgres-tier expiry check below: an LRU entry
			// whose PinnedUntil has lapsed must not be served. The 30s
			// LRU TTL is short relative to the 1h pin TTL, so this is
			// mostly defense-in-depth — but recordTurnUsage re-adds
			// entries on every turn (resetting the LRU clock), so under
			// the worst-case ordering (refreshPin's enqueue dropped on
			// semaphore saturation while recordTurnUsage keeps landing)
			// an expired entry could otherwise outlive its PinnedUntil.
			if pin.PinnedUntil.After(time.Now()) {
				return pin, true
			}
			s.pinCache.Remove(pinCacheKey)
		}
	}
	pin, found, err := s.pinStore.Get(ctx, sessionKey, role)
	if err != nil {
		log.Error("session pin store unavailable; falling through to cluster scorer", "err", err)
		return sessionpin.Pin{}, false
	}
	if !found {
		return sessionpin.Pin{}, false
	}
	if !pin.PinnedUntil.After(time.Now()) {
		return sessionpin.Pin{}, false
	}
	if s.pinCache != nil {
		s.pinCache.Add(pinCacheKey, pin)
	}
	return pin, true
}

// refreshPin extends the TTL on an existing pin and refreshes the in-proc
// cache entry. Carries the existing pin's cached last-turn usage forward
// so the planner has evidence on subsequent turns even before the next
// UpdateUsage writeback lands. Async, bounded; drops on saturation.
func (s *Service) refreshPin(installationID uuid.UUID, sessionKey [sessionpin.SessionKeyLen]byte, existing sessionpin.Pin, pinCacheKey string, role string, chosen router.Decision) {
	if installationID == uuid.Nil {
		return
	}
	p := sessionpin.Pin{
		SessionKey:            sessionKey,
		Role:                  role,
		InstallationID:        installationID,
		Provider:              chosen.Provider,
		Model:                 chosen.Model,
		Reason:                chosen.Reason,
		TurnCount:             1,
		PinnedUntil:           time.Now().Add(pinSessionTTL),
		LastInputTokens:       existing.LastInputTokens,
		LastCachedReadTokens:  existing.LastCachedReadTokens,
		LastCachedWriteTokens: existing.LastCachedWriteTokens,
		LastOutputTokens:      existing.LastOutputTokens,
		LastTurnEndedAt:       existing.LastTurnEndedAt,
	}
	s.enqueuePinUpsert(p, pinCacheKey)
}

// writeNewPin records a freshly-routed decision as the active pin. Used
// on first-turn routing and on switch turns; the new row has no prior
// usage stats yet (UpdateUsage fills them in after the response lands).
func (s *Service) writeNewPin(installationID uuid.UUID, sessionKey [sessionpin.SessionKeyLen]byte, pinCacheKey string, role string, chosen router.Decision) {
	observability.Get().Debug("writeNewPin called", "installation_id", installationID.String(), "role", role, "model", chosen.Model, "session_key_hex", fmt.Sprintf("%x", sessionKey))
	if installationID == uuid.Nil {
		observability.Get().Debug("writeNewPin: skipping because installationID is uuid.Nil")
		return
	}
	p := sessionpin.Pin{
		SessionKey:     sessionKey,
		Role:           role,
		InstallationID: installationID,
		Provider:       chosen.Provider,
		Model:          chosen.Model,
		Reason:         chosen.Reason,
		TurnCount:      1,
		PinnedUntil:    time.Now().Add(pinSessionTTL),
	}
	s.enqueuePinUpsert(p, pinCacheKey)
}

// enqueuePinUpsert pushes a pin write onto the bounded async worker pool.
// Drops on saturation (a slow Postgres must not accumulate goroutines on
// the request path). Also primes the in-proc LRU so the next turn on
// this session avoids Postgres.
func (s *Service) enqueuePinUpsert(p sessionpin.Pin, pinCacheKey string) {
	log := observability.Get()
	select {
	case s.pinWriteSem <- struct{}{}:
		go func(pin sessionpin.Pin) {
			defer func() { <-s.pinWriteSem }()
			// context.Background(): the request ctx is canceled by the
			// time the response has finished streaming, which would drop
			// the write and break provider continuity next turn.
			if err := s.pinStore.Upsert(context.Background(), pin); err != nil {
				observability.Get().Error("session pin upsert failed", "err", err)
				return
			}
			observability.Get().Debug("session pin upsert ok", "installation_id", pin.InstallationID.String(), "role", pin.Role, "model", pin.Model)
		}(p)
	default:
		log.Debug("session pin upsert dropped: semaphore full")
	}
	if s.pinCache != nil {
		s.pinCache.Add(pinCacheKey, p)
	}
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
