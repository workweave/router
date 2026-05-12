package proxy

import (
	"context"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"
	"workweave/router/internal/router"
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

// sessionRouteResult bundles the routing decision and pin-state fields for
// OTel spans and log lines.
type sessionRouteResult struct {
	Decision   router.Decision
	SessionKey [sessionpin.SessionKeyLen]byte
	TurnType   turntype.TurnType
	StickyHit  bool
	HardPinned bool
	PinTier    string
	PinAgeSec  int64
}

// routeWithSession is the format-agnostic routing orchestrator: turn-type
// detection, hard-pin short-circuit, tiered session-pin lookup, cluster
// scorer fallback, and async pin upsert.
//
// installationID == uuid.Nil skips the async upsert (pin rows require an
// installation_id). bypassLegacySticky disables only the Tier-3 apiKeyID LRU;
// Tier 1/2 stay active because their key derivation is per-prompt.
func (s *Service) routeWithSession(
	ctx context.Context,
	env *translate.RequestEnvelope,
	feats translate.RoutingFeatures,
	apiKeyID string,
	installationID uuid.UUID,
	subAgentHint string,
	bypassLegacySticky bool,
	req router.Request,
) (sessionRouteResult, error) {
	log := observability.Get()
	res := sessionRouteResult{
		TurnType: turntype.DetectFromEnvelope(env, feats, subAgentHint),
		PinTier:  "miss",
	}

	// §3.4: hard pins for turn types whose optimal model is known a priori.
	// These bypass all pin tiers and the cluster scorer, and must NOT write
	// a pin upsert (would overwrite the session's main-loop model).
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

	// Tier 1 (in-proc LRU) → Tier 2 (Postgres) → Tier 3 (legacy apiKeyID
	// LRU). See docs/plans/SESSION_PIN.md §5. Tier 3 stays bypassed under
	// eval-override headers because the eval harness shares a single
	// apiKeyID across unrelated prompts.
	pinEligible := s.pinStore != nil
	var pinCacheKey string
	if pinEligible {
		res.SessionKey = DeriveSessionKey(env, apiKeyID, auth.UserIDFrom(ctx))
		pinCacheKey = sessionPinCacheKey(res.SessionKey, sessionpin.DefaultRole)

		if s.pinCache != nil {
			if pin, ok := s.pinCache.Get(pinCacheKey); ok {
				res.Decision = pinDecision(pin)
				res.StickyHit = true
				res.PinTier = "in_proc"
				res.PinAgeSec = pinAge(pin)
			}
		}
		if !res.StickyHit {
			pin, found, err := s.pinStore.Get(ctx, res.SessionKey, sessionpin.DefaultRole)
			if err != nil {
				log.Error("session pin store unavailable; falling through to cluster scorer", "err", err)
			} else if found && pin.PinnedUntil.After(time.Now()) {
				res.Decision = pinDecision(pin)
				res.StickyHit = true
				res.PinTier = "postgres"
				res.PinAgeSec = pinAge(pin)
				if s.pinCache != nil {
					s.pinCache.Add(pinCacheKey, pin)
				}
			}
		}
	}

	// Tier 3: legacy apiKeyID LRU. Consulted only on Tier-2 miss (or when
	// pinStore is nil) and never under eval-override headers.
	if !res.StickyHit && s.stickyDecisions != nil && apiKeyID != "" && !bypassLegacySticky {
		if d, ok := s.stickyDecisions.Get(apiKeyID); ok {
			res.Decision = d
			res.StickyHit = true
			res.PinTier = "legacy_apikey"
		}
	}

	if !res.StickyHit {
		decision, err := s.router.Route(ctx, req)
		if err != nil {
			return res, err
		}
		res.Decision = decision
		if s.stickyDecisions != nil && apiKeyID != "" && !bypassLegacySticky {
			s.stickyDecisions.Add(apiKeyID, decision)
		}
	}

	// §3.3: annotate pin tier when a tool-result turn short-circuits to an
	// existing session pin.
	if res.StickyHit && res.TurnType == turntype.ToolResult {
		res.PinTier += "_tool_result_sc"
	}

	// Async upsert refreshes the sliding TTL on sticky hits and records
	// new pins on fresh routes. context.Background() is mandatory: a
	// request cancel would otherwise drop the write and force re-routing
	// next turn (breaking provider/cache continuity).
	if pinEligible && installationID != uuid.Nil {
		pin := sessionpin.Pin{
			SessionKey:     res.SessionKey,
			Role:           sessionpin.DefaultRole,
			InstallationID: installationID,
			Provider:       res.Decision.Provider,
			Model:          res.Decision.Model,
			Reason:         res.Decision.Reason,
			TurnCount:      1, // ON CONFLICT increments; only meaningful on first insert
			PinnedUntil:    time.Now().Add(pinSessionTTL),
		}
		select {
		case s.pinWriteSem <- struct{}{}:
			go func(p sessionpin.Pin) {
				defer func() { <-s.pinWriteSem }()
				if err := s.pinStore.Upsert(context.Background(), p); err != nil {
					observability.Get().Error("session pin upsert failed", "err", err)
				}
			}(pin)
		default:
			observability.Get().Debug("session pin upsert dropped: semaphore full")
		}
		if s.pinCache != nil {
			s.pinCache.Add(pinCacheKey, pin)
		}
	}

	return res, nil
}
