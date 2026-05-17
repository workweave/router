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

// turnLoopResult bundles the routing decision and pin/planner state.
type turnLoopResult struct {
	Decision   router.Decision
	SessionKey [sessionpin.SessionKeyLen]byte
	TurnType   turntype.TurnType
	StickyHit  bool
	HardPinned bool
	PinTier    string
	PinAgeSec  int64
	// RequestedTier is the tier of the inbound requested model. Drives the
	// tier-ceiling clamp. TierUnknown disables clamping — the right behavior
	// for custom model names that have no known tier.
	RequestedTier capability.Tier
	// TierClamped is true when the original decision violated the
	// requested-model ceiling and was rewritten.
	TierClamped   bool
	PreClampModel string
	// PinRole is the session-pin role used for this turn, preventing a
	// low-tier background turn and a high-tier main turn from sharing a pin.
	PinRole string
	// Fresh is the scorer's recommendation for this turn when the scorer ran.
	Fresh router.Decision
	// PlannerDecision holds the planner's verdict and EV math when the planner ran.
	PlannerDecision planner.Decision
	// PinModel is the model on the loaded pin (stamped independently of
	// PlannerDecision so log lines can name the from-model even on stay outcomes).
	PinModel string
	// Handover captures the summarize-or-trim step when the planner switched.
	Handover handoverOutcome
}

// handoverOutcome describes the synchronous handover step.
type handoverOutcome struct {
	Invoked        bool
	LatencyMS      int64
	SummaryTokens  int
	FallbackToTrim bool
	// SummaryUsage captures upstream token usage for the summarizer call
	// so proxy.fireBilling can debit it as a separate ledger row with the
	// "_summary" request_id suffix. Zero on fallback/error paths.
	SummaryUsage handover.Usage
}

// runTurnLoop is the format-agnostic routing orchestrator: detect turn type,
// short-circuit hard pins, load pin, run scorer, hand to planner, and on
// switch attempt bounded-cost handover.
//
// installationID == uuid.Nil skips async pin upsert (pin rows need an
// installation_id); the rest of the path runs normally.
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

	// Hard pins bypass pin lookup, pin write, planner, and scorer entirely.
	// Probes and title-gen MUST NOT create a session pin — the Anthropic SDK
	// fires probes on init before the first real user turn, and Claude Code
	// fires title-gen ~25ms before the real-conv call. An anchored pin would
	// inherit the cheap-model decision into the immediately-following real
	// conversation that should have routed on its own.
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

	// Without a pin store, run the scorer and return its decision.
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

	// Planner-disabled + pin found: preserve first-decision-wins behavior.
	if !s.plannerEnabled && pinFound {
		decision := s.clampToCeiling(pinDecision(pin), res.RequestedTier, req.EnabledProviders, req.ExcludedModels, &res)
		res.Decision = decision
		res.StickyHit = true
		res.PinTier = "postgres"
		s.refreshPin(installationID, res.SessionKey, pin, pinCacheKey, res.PinRole, decision)
		return res, nil
	}

	// Always run the scorer when no pin, or on MainLoop with a pin.
	fresh, err := s.router.Route(ctx, req)
	if err != nil {
		return res, err
	}
	fresh = s.clampToCeiling(fresh, res.RequestedTier, req.EnabledProviders, req.ExcludedModels, &res)
	res.Fresh = fresh

	if !s.plannerEnabled {
		res.Decision = fresh
		s.writeNewPin(installationID, res.SessionKey, pinCacheKey, res.PinRole, fresh)
		return res, nil
	}

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

	// Switch path: when switching off a warm cache, attempt bounded-cost
	// handover. On summarizer error fall back to TrimLastN. When the
	// summarizer is not wired or the request carries BYOK/client credentials,
	// skip summarization and trim instead — the switch turn must NOT forward
	// the full prior conversation to the new model.
	//
	// Privacy guard: the summarizer is wired with deployment-level creds.
	// Calling it on a BYOK/client-creds request would route prior conversation
	// through the platform account, violating tenant data boundaries.
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
			summary, summaryUsage, sumErr := s.summarizer.Summarize(ctx, env)
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
				res.Handover.SummaryUsage = summaryUsage
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

// roleForTier maps a requested-model tier to its session-pin role. Each tier
// gets its own row so separate-tier turns never share a pin. TierUnknown
// falls back to DefaultRole.
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
// decision's tier exceeds the ceiling, the resolver picks the cheapest
// in-ceiling alternative. Decisions at/below ceiling pass through.
// TierUnknown disables clamping. Resolver failure preserves the original
// decision as a soft fallback.
func (s *Service) clampToCeiling(decision router.Decision, ceiling capability.Tier, enabled, excluded map[string]struct{}, res *turnLoopResult) router.Decision {
	// Reset state every call: the orchestrator clamps multiple decision
	// sources per turn, and without this reset a clamp on `fresh` would leak
	// TierClamped=true + PreClampModel into a subsequent unclamped pin decision.
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
// LRU first then Postgres. Expired rows are treated as misses.
func (s *Service) loadPin(ctx context.Context, pinCacheKey string, sessionKey [sessionpin.SessionKeyLen]byte, role string) (sessionpin.Pin, bool) {
	log := observability.Get()
	log.Debug("loadPin called", "role", role, "session_key_hex", fmt.Sprintf("%x", sessionKey))
	if s.pinCache != nil {
		if pin, ok := s.pinCache.Get(pinCacheKey); ok {
			// Check expiry: recordTurnUsage refreshes LRU entries on every
			// turn (resetting the 30s eviction clock), so without this guard
			// an expired entry whose refreshPin write was dropped could keep
			// being served past its PinnedUntil.
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

// refreshPin extends the TTL on an existing pin. Carries the existing pin's
// usage forward so the planner has evidence before the next UpdateUsage
// writeback lands. Async, bounded; drops on saturation.
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

// writeNewPin records a freshly-routed decision as the active pin. Used on
// first-turn routing and switch turns. UpdateUsage fills in usage stats later.
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
// Drops on saturation. Primes the in-proc LRU so the next turn avoids Postgres.
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
