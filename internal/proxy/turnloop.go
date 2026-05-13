package proxy

import (
	"context"
	"net/http"
	"time"

	"workweave/router/internal/observability"
	"workweave/router/internal/router"
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
		TurnType: turntype.DetectFromEnvelope(env, feats, subAgentHint),
		PinTier:  "miss",
	}

	// Hard pins for turn types whose optimal model is known a priori
	// (compaction, Explore sub-agent dispatch). Bypass pin lookup, pin
	// write, planner and scorer entirely so the pin row keeps tracking
	// the main-loop model.
	if res.TurnType == turntype.Compaction ||
		(res.TurnType == turntype.SubAgentDispatch && s.hardPinExplore) {
		res.Decision = router.Decision{
			Provider: s.hardPinProvider,
			Model:    s.hardPinModel,
			Reason:   string(res.TurnType) + "_hard_pin",
		}
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
		res.Decision = decision
		res.Fresh = decision
		return res, nil
	}

	res.SessionKey = DeriveSessionKey(env, apiKeyID)
	pinCacheKey := sessionPinCacheKey(res.SessionKey, sessionpin.DefaultRole)

	pin, pinFound := s.loadPin(ctx, pinCacheKey, res.SessionKey)
	if pinFound {
		res.PinModel = pin.Model
		res.PinAgeSec = pinAge(pin)
	}

	// Tool-result turns are mid-turn continuations. Re-routing them on
	// trailing tool_result embedding flips decisions to noisy candidates;
	// reuse the pin verbatim when present and refresh the TTL.
	if res.TurnType == turntype.ToolResult && pinFound {
		res.Decision = pinDecision(pin)
		res.StickyHit = true
		res.PinTier = "postgres_tool_result_sc"
		s.refreshPin(installationID, res.SessionKey, pin, pinCacheKey, res.Decision)
		return res, nil
	}

	// Planner-disabled + pin found: preserve "first decision wins"
	// behavior for callers that opt out via env flag. Skip the scorer
	// entirely; the pin's model is authoritative.
	if !s.plannerEnabled && pinFound {
		res.Decision = pinDecision(pin)
		res.StickyHit = true
		res.PinTier = "postgres"
		s.refreshPin(installationID, res.SessionKey, pin, pinCacheKey, res.Decision)
		return res, nil
	}

	// MainLoop (or ToolResult without an existing pin, or planner-
	// disabled without a pin): always run the scorer. The scorer's
	// error path surfaces to HTTP 503 in the presentation layer.
	fresh, err := s.router.Route(ctx, req)
	if err != nil {
		return res, err
	}
	res.Fresh = fresh

	// Planner-disabled with no pin: take the fresh decision and write
	// a new pin row so subsequent turns see it.
	if !s.plannerEnabled {
		res.Decision = fresh
		s.writeNewPin(installationID, res.SessionKey, pinCacheKey, fresh)
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
		res.Decision = pinDecision(pin)
		res.StickyHit = true
		res.PinTier = "postgres_stay_" + decision.Reason
		s.refreshPin(installationID, res.SessionKey, pin, pinCacheKey, res.Decision)
		return res, nil
	}

	// Switch path: either explicit outcome=switch, or (defensively)
	// outcome=stay with no pin to actually stay on. When switching off an
	// existing warm cache, attempt bounded-cost handover: synchronously
	// summarize prior context with the cheap-model summarizer; on error
	// fall back to TrimLastN so the switch turn still succeeds.
	//
	// Privacy guard: the summarizer is wired at boot with deployment-level
	// provider credentials. Calling it on a request whose upstream call
	// would otherwise use BYOK or client-supplied credentials would route
	// prior conversation context (carried verbatim into the summarizer
	// prompt) through the platform account, violating tenant data
	// boundaries. Detect that case and skip straight to TrimLastN.
	if pinFound && s.summarizer != nil {
		if s.requestUsesNonDeploymentCreds(ctx, reqHeaders) {
			elided := handover.TrimLastN(env, 3)
			res.Handover.Invoked = true
			res.Handover.FallbackToTrim = true
			log.Info("Handover summarizer skipped to preserve BYOK tenant boundary; trimmed envelope instead", "elided_messages", elided, "pin_model", pin.Model, "fresh_model", fresh.Model)
		} else {
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
	s.writeNewPin(installationID, res.SessionKey, pinCacheKey, fresh)
	return res, nil
}

// loadPin returns the active pin for this session, consulting the in-proc
// LRU first then Postgres. Expired rows are treated as misses. A store
// error is logged and surfaces as "not found" so the caller falls
// through to the planner with a zero pin.
func (s *Service) loadPin(ctx context.Context, pinCacheKey string, sessionKey [sessionpin.SessionKeyLen]byte) (sessionpin.Pin, bool) {
	log := observability.Get()
	if s.pinCache != nil {
		if pin, ok := s.pinCache.Get(pinCacheKey); ok {
			return pin, true
		}
	}
	pin, found, err := s.pinStore.Get(ctx, sessionKey, sessionpin.DefaultRole)
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
func (s *Service) refreshPin(installationID uuid.UUID, sessionKey [sessionpin.SessionKeyLen]byte, existing sessionpin.Pin, pinCacheKey string, chosen router.Decision) {
	if installationID == uuid.Nil {
		return
	}
	p := sessionpin.Pin{
		SessionKey:            sessionKey,
		Role:                  sessionpin.DefaultRole,
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
func (s *Service) writeNewPin(installationID uuid.UUID, sessionKey [sessionpin.SessionKeyLen]byte, pinCacheKey string, chosen router.Decision) {
	if installationID == uuid.Nil {
		return
	}
	p := sessionpin.Pin{
		SessionKey:     sessionKey,
		Role:           sessionpin.DefaultRole,
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
			}
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
