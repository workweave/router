package proxy

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"workweave/router/internal/observability"
	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"
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

// cacheWarm reports whether the pin's upstream prompt cache is likely still
// warm — a prior turn completed within the pinned provider's best-effort cache
// TTL. A cold pin earns no cache-read discount in the planner's EV math.
func cacheWarm(pin sessionpin.Pin) bool {
	if pin.LastTurnEndedAt.IsZero() {
		return false
	}
	return time.Since(pin.LastTurnEndedAt) < providers.CacheTTLFor(pin.Provider)
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
	// session-pin role split (roleForTier) so a low-tier background turn and a
	// high-tier main turn never share a pin.
	RequestedTier catalog.Tier
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
	// PriorServedModel is the model that actually served the previous turn in
	// this session (the pin's LastServedModel), independent of PinModel — a
	// /force-model write changes PinModel but not this. Compared against the
	// current decision model to detect a mid-session switch so the Anthropic
	// emit path can strip thinking blocks whose signatures the new model rejects.
	PriorServedModel string
	// SessionEverSwitched is the pin's latched has_ever_switched flag: true once
	// the session has served two different models at any point. PriorServedModel
	// only flags the single switch-back turn, but the stale-signed thinking
	// blocks a cross-model excursion left in the client transcript persist on
	// every later turn, so the emit path ORs this into ModelSwitched to keep
	// stripping them for the life of the session.
	SessionEverSwitched bool
	// Handover captures the summarize-or-trim step when the planner switched.
	Handover handoverOutcome
	// SuggestionMode is true when the request arrived with the
	// x-weave-suggestion-mode header. The routing marker is suppressed so
	// the badge does not appear in suggestion-overlay responses.
	SuggestionMode bool
	// EscalateEffort is true when the previous turn in this session looked like
	// an observable failure (produced no output, or the pin carried a consecutive
	// upstream error). The escalate-on-failure effort policy reads it to bump a
	// gpt-5.x turn from low to high effort. It reflects the loaded pin's prior-turn
	// state regardless of any same-turn pin-drop guards below, and is a no-op
	// unless Service.effortEscalation is enabled.
	EscalateEffort bool
}

// modelSwitched reports whether the Anthropic emit path must strip historical
// thinking blocks for this turn. Two cases force a strip: the transition turn
// itself (the model serving this turn differs from the one that served the
// previous turn), and any turn in a session that has ever switched — because
// Claude Code re-sends its full transcript every turn, so the stale-signed
// blocks an earlier cross-model excursion left behind keep coming back and
// would 400 with `Invalid signature in thinking block` on every later turn,
// not just the switch-back.
func (r turnLoopResult) modelSwitched() bool {
	transition := r.PriorServedModel != "" && r.PriorServedModel != r.Decision.Model
	return transition || r.SessionEverSwitched
}

// handoverOutcome describes the synchronous handover step.
type handoverOutcome struct {
	Invoked       bool
	LatencyMS     int64
	SummaryTokens int
	// FallbackToFullHistory is set when handover was invoked but no summary
	// was applied (summarizer unwired, tenant-boundary skip, timeout, error,
	// or empty summary). The original body is passed through unchanged rather
	// than trimmed, so the model keeps full prior context. No summary ledger
	// row is billed on this path.
	FallbackToFullHistory bool
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
	log := observability.FromContext(ctx)
	res := turnLoopResult{
		TurnType:      turntype.DetectFromEnvelope(env, feats, subAgentHint),
		PinTier:       "miss",
		RequestedTier: catalog.TierFor(feats.Model),
	}
	res.PinRole = roleForTier(res.RequestedTier)
	log.Info("turnloop classified",
		"turn_type", string(res.TurnType),
		"requested_tier", res.RequestedTier.String(),
		"pin_role", res.PinRole,
		"sub_agent_hint", subAgentHint,
	)

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
		res.Decision = decision
		res.Fresh = decision
		return res, nil
	}

	res.SessionKey = DeriveSessionKey(env, apiKeyID)

	pin, pinFound := s.loadPin(ctx, res.SessionKey, res.PinRole)
	res.PriorServedModel = pin.LastServedModel
	res.SessionEverSwitched = pin.HasEverSwitched
	// Escalate-on-failure signal: a prior turn that completed (LastTurnEndedAt set)
	// but produced no output, or left a consecutive upstream error, is an
	// observable failure. Computed from the loaded pin before any same-turn
	// pin-drop guards below so it reflects the prior turn's outcome. The policy
	// that acts on it (Service.effortEscalation) is gated separately.
	res.EscalateEffort = pinFound && !pin.LastTurnEndedAt.IsZero() &&
		(pin.LastOutputTokens == 0 || pin.ConsecutiveUpstreamErrors > 0)
	if pinFound {
		res.PinModel = pin.Model
		res.PinAgeSec = pinAge(pin)
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

	// User-forced pins are immutable stickies — skip scorer and planner entirely.
	// The pin was written by /force-model with a never-expires PinnedUntil, so it
	// survives arbitrarily long idle gaps and stays active until /unforce-model
	// clears it (rewriting the row with a past PinnedUntil), at which point the
	// pin is expired and this branch is not taken.
	//
	// Invariants maintained here:
	//   1. Excluded-model policy is still enforced: if the forced model has been
	//      added to the installation exclusion list since the pin was written, fall
	//      through to normal routing so the exclusion takes effect immediately.
	//   2. Provider eligibility is enforced per-request. In BYOK mode the request's
	//      EnabledProviders may not contain the pinned provider (e.g. the user
	//      forced gpt-5 but the current request only carries Anthropic BYOK creds).
	//      Falling through to normal routing avoids a guaranteed 401/unauthenticated
	//      upstream call.
	//   3. The user's forced model is served as-is and the pin is refreshed with
	//      the original decision, so the directive survives across turns.
	// forcedTierFloor preserves the user's tier intent when a user-forced pin
	// is dropped below because its model can no longer serve this turn (most
	// often the session outgrew the model's context window and the pre-filter
	// evicted it). The scorer call further down constrains the fresh decision
	// to this tier so we pick the next-best model in it rather than collapsing
	// to the cheap tier-default. TierUnknown means "no constraint".
	forcedTierFloor := catalog.TierUnknown
	if pinFound && (pin.Reason == translate.ReasonUserForceModel || pin.Reason == translate.ReasonLoopEscalation) {
		_, excluded := req.ExcludedModels[pin.Model]
		_, providerEnabled := req.EnabledProviders[pin.Provider]
		providerEligible := req.EnabledProviders == nil || providerEnabled
		if !excluded && providerEligible {
			decision := pinDecision(pin)
			decision.Reason = pin.Reason
			res.PinTier = pin.Reason
			res.Decision = decision
			res.StickyHit = true
			s.refreshPin(ctx, installationID, res.SessionKey, pin, res.PinRole, pinDecision(pin))
			return res, nil
		}
		if excluded {
			// The forced model can't serve this turn — typically the session
			// outgrew its context window and the pre-filter evicted it. The
			// user still asked for a model of this tier, so remember it and
			// constrain the fresh decision to the same tier below.
			forcedTierFloor = catalog.TierFor(pin.Model)
		}
		// Forced pin is no longer servable on this request (excluded by policy
		// or pinned provider not in EnabledProviders/BYOK). Treat it as missing
		// so downstream sticky branches don't dispatch to an unauthorized
		// provider. The pin row remains in storage — a later request whose
		// EnabledProviders includes the forced provider will resume serving it.
		pinFound = false
		pin = sessionpin.Pin{}
	}

	// Previous-turn-maxed-out guard: when an OSS model's tool-call tokens fail
	// to parse server-side (kimi <|tool_call_begin|>, qwen3 <tool_call> XML)
	// the upstream emits them as content and generates to the output cap.
	// Claude Code's "Output token limit hit. Resume directly…" auto-continue
	// then re-pins the same broken model, producing a multi-minute loop. When
	// the previous turn saturated the output cap, exclude the pinned model for
	// this turn and treat the pin as missing so downstream sticky branches
	// (ToolResult, !plannerEnabled) cannot re-anchor it before the scorer runs.
	if pinFound && pin.LastOutputTokens >= prevTurnMaxedOutThreshold {
		log.Info("Session pin maxed out on previous turn; excluding for this turn",
			"pin_model", pin.Model,
			"pin_provider", pin.Provider,
			"last_output_tokens", pin.LastOutputTokens,
		)
		// Defensive copy: callers may share the ExcludedModels map across requests.
		excluded := make(map[string]struct{}, len(req.ExcludedModels)+1)
		for k := range req.ExcludedModels {
			excluded[k] = struct{}{}
		}
		excluded[pin.Model] = struct{}{}
		req.ExcludedModels = excluded
		pinFound = false
		pin = sessionpin.Pin{}
	}

	// Context-window overflow guard: if the pre-filter in ProxyMessages /
	// ProxyOpenAIChatCompletion added the pinned model to ExcludedModels
	// (because this turn's estimated context exceeds the model's window),
	// verify with a direct fit-check before evicting the pin. The pre-filter
	// uses body_bytes÷5 as a conservative token estimate; the pin guard uses
	// the same formula but checks the model's actual context window, so a
	// conservative over-estimate that only slightly overshoots a large-window
	// model does not cause an unnecessary mid-session switch.
	// A confirmed overflow (needed > model window) still evicts the pin.
	if pinFound {
		if _, overCapacity := req.ExcludedModels[pin.Model]; overCapacity {
			outputReserveForPin := contextWindowOutputReserve
			if feats.MaxTokens > outputReserveForPin {
				outputReserveForPin = feats.MaxTokens
			}
			needed := env.FullTokenEstimate() + outputReserveForPin
			modelCW := contextWindowForRequest(pin.Model)
			if needed > modelCW {
				log.Info("Session pin excluded by context-window pre-filter; falling through to scorer",
					"pin_model", pin.Model,
					"pin_provider", pin.Provider,
					"token_estimate", env.FullTokenEstimate(),
					"needed", needed,
					"model_context_window", modelCW,
				)
				pinFound = false
				pin = sessionpin.Pin{}
			} else {
				// Pre-filter was overly conservative — pin model fits.
				// Remove it from ExcludedModels only if it was added
				// exclusively by the context-overflow filter, NOT by an
				// operator/installation policy exclusion. Policy exclusions
				// are a hard operator constraint; lifting them here would
				// let sticky routing bypass an operator-excluded model on
				// the very turns where the context window happens to fit.
				policyExcluded := s.excludedModelsForRequest(ctx)
				if _, policyExcludes := policyExcluded[pin.Model]; !policyExcludes {
					if len(req.ExcludedModels) > 0 {
						pruned := make(map[string]struct{}, len(req.ExcludedModels)-1)
						for k := range req.ExcludedModels {
							if k != pin.Model {
								pruned[k] = struct{}{}
							}
						}
						req.ExcludedModels = pruned
					}
				}
				log.Info("Session pin preserved despite context-window pre-filter exclusion",
					"pin_model", pin.Model,
					"token_estimate", env.FullTokenEstimate(),
					"needed", needed,
					"model_context_window", modelCW,
				)
			}
		}
	}

	// Provider-eligibility guard: when the pinned provider is no longer in
	// this request's enabled set (installation/env provider exclusion, or a
	// BYOK request without that provider's creds), treat the pin as missing
	// so the sticky branches below (ToolResult, OutcomeStay, !plannerEnabled)
	// cannot keep serving through a provider the request must not use.
	// Mirrors the providerEligible check on the user-forced pin path above —
	// without it, a session pinned before the exclusion was saved would keep
	// hitting the excluded provider until the pin expired.
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

	// Image-input guard: when this turn carries image content but the pinned
	// model is text-only, drop the pin and fall through to the scorer so an
	// image-capable model is chosen. The scorer's own image-input filter then
	// removes text-only models from the eligible pool, with a soft empty-pool
	// fallback (an OSS-only self-host with no image-capable candidate keeps the
	// text-only pool and lets the upstream surface the 4xx). We deliberately do
	// NOT add the pin to ExcludedModels: exclusion is a hard filter that errors
	// on an empty pool, which would turn that soft fallback into a routing
	// failure on exactly those deploys. Without this guard, a session pinned to
	// a text-only model on earlier text turns 4xxs the moment the user pastes a
	// screenshot — the pin would otherwise bypass the image-aware fresh decision
	// via the sticky branches below. Runs before those branches for the same
	// reason as the context-window guard.
	if pinFound && req.HasImages && !catalog.AcceptsImages(pin.Model) {
		log.Info("Session pin is text-only for image-bearing turn; falling through to scorer",
			"pin_model", pin.Model,
			"pin_provider", pin.Provider,
		)
		pinFound = false
		pin = sessionpin.Pin{}
	}

	// Tool-result turns are mid-turn continuations. Re-routing them on
	// trailing tool_result embedding flips decisions to noisy candidates;
	// reuse the pin verbatim when present and refresh the TTL.
	if res.TurnType == turntype.ToolResult && pinFound {
		decision := pinDecision(pin)
		res.Decision = decision
		res.StickyHit = true
		res.PinTier = "postgres_tool_result_sc"
		s.refreshPin(ctx, installationID, res.SessionKey, pin, res.PinRole, decision)
		return res, nil
	}

	// Planner-disabled + pin found: preserve first-decision-wins behavior.
	if !s.plannerEnabled && pinFound {
		decision := pinDecision(pin)
		res.Decision = decision
		res.StickyHit = true
		res.PinTier = "postgres"
		s.refreshPin(ctx, installationID, res.SessionKey, pin, res.PinRole, decision)
		return res, nil
	}

	// Always run the scorer when no pin, or on MainLoop with a pin. When a
	// user-forced pin was just evicted (typically the session outgrew the
	// model's context window), the user still asked for a model of that tier:
	// route the scorer constrained to the forced model's tier and pick the
	// next-best in it. Collapsing straight to the cheap tier-default would
	// silently downgrade the user's directive. Fall back to the unconstrained
	// scorer when no in-tier model survives the request's other filters, so the
	// constraint never turns a routable turn into a failure.
	var fresh router.Decision
	routed := false
	if forcedTierFloor != catalog.TierUnknown {
		if constrained, ok := s.restrictToTier(req.ExcludedModels, forcedTierFloor); ok {
			tierReq := req
			tierReq.ExcludedModels = constrained
			if dec, derr := s.router.Route(ctx, tierReq); derr == nil {
				fresh, routed = dec, true
				log.Info("user-forced model evicted; rerouted to next-best in same tier",
					"forced_tier", forcedTierFloor.String(),
					"fresh_model", dec.Model,
					"fresh_provider", dec.Provider,
				)
			} else {
				log.Info("tier-constrained reroute found no candidate; using unconstrained scorer",
					"forced_tier", forcedTierFloor.String(), "err", derr)
			}
		}
	}
	if !routed {
		dec, err := s.router.Route(ctx, req)
		if err != nil {
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

	// Expired-pin re-anchor: when the session pin has lapsed mid-session
	// (!pinFound but pin.Model != "" — an expired row, not a first-turn
	// miss), prefer staying on the prior model instead of taking whatever
	// lateral switch the scorer happened to pick on the one expiry turn.
	// A model-switch on a single expiry turn is frequently noise: the
	// scorer may land in a different cluster on that turn and return a
	// different model that the session then stays on for the rest of its
	// life, even if subsequent turns would have scored the prior model
	// equally well. Re-anchor when all of the following hold:
	//   (a) both model tiers are known (TierUnknown falls through to scorer)
	//   (b) the fresh recommendation is NOT a tier upgrade
	//   (c) the prior model is still routable (in availableModels)
	//   (d) the prior model is not excluded (e.g. context-window overflow)
	//   (e) the prior provider is still in the request's enabled set
	//   (f) the prior turn did NOT saturate the output cap (maxed-out guard
	//       mirrors the live-pin check above — re-anchoring a broken model
	//       that already generated to the cap would restart the degenerate
	//       auto-continue loop)
	//   (g) this turn does NOT carry images if the prior model is text-only
	//       (image guard mirrors the live-pin check above — re-anchoring a
	//       text-only model on an image-bearing turn would immediately 4xx)
	// When re-anchoring, write a new pin so the next turn is a sticky hit.
	if !pinFound && pin.Model != "" {
		pinTier := catalog.TierFor(pin.Model)
		freshTier := catalog.TierFor(fresh.Model)
		if pinTier != catalog.TierUnknown && freshTier != catalog.TierUnknown && freshTier <= pinTier {
			if _, excluded := req.ExcludedModels[pin.Model]; !excluded {
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

	if !s.plannerEnabled {
		res.Decision = fresh
		s.writeNewPin(ctx, installationID, res.SessionKey, res.PinRole, fresh)
		return res, nil
	}

	plannerIn := planner.Inputs{
		Pin:                  pin,
		Fresh:                fresh,
		EstimatedInputTokens: feats.Tokens,
		AvailableModels:      s.availableModels,
		PinCacheCold:         pinFound && !cacheWarm(pin),
	}
	if !pinFound {
		plannerIn.Pin = sessionpin.Pin{}
	}
	decision := planner.Decide(plannerIn, s.planner)
	res.PlannerDecision = decision

	if decision.Outcome == planner.OutcomeStay && pinFound {
		stay := pinDecision(pin)
		res.Decision = stay
		res.StickyHit = true
		res.PinTier = "postgres_stay_" + decision.Reason
		s.refreshPin(ctx, installationID, res.SessionKey, pin, res.PinRole, stay)
		return res, nil
	}

	// Switch path: when switching off a warm cache, attempt bounded-cost
	// handover. On any summarizer failure we keep the full prior history
	// rather than trimming it — a more expensive switch turn is always
	// preferable to silently dropping the conversation the new model needs.
	//
	// Privacy guard: the summarizer is wired with deployment-level creds.
	// Routing a BYOK/client request's prior conversation through that
	// deployment account would cross the tenant boundary. We avoid that by
	// preferring per-request creds for the summarizer's provider when the
	// caller forwarded them (BYOK or inbound Authorization/x-api-key for
	// that provider) — that's the caller's own account, not the platform's.
	// Only when the request is BYOK/client-keyed AND no matching creds for
	// the summarizer's provider were forwarded do we skip summarization and
	// pass the full history through.
	if pinFound {
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
	if !pin.PinnedUntil.After(time.Now()) {
		return pin, false
	}
	return pin, true
}

// refreshPin extends the TTL on an existing pin. Carries the existing pin's
// usage forward so the planner has evidence before the next UpdateUsage
// writeback lands.
func (s *Service) refreshPin(ctx context.Context, installationID uuid.UUID, sessionKey [sessionpin.SessionKeyLen]byte, existing sessionpin.Pin, role string, chosen router.Decision) {
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
	log.Info("writeNewPin called", "installation_id", installationID.String(), "role", role, "model", chosen.Model, "session_key_hex", fmt.Sprintf("%x", sessionKey))
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
		Reason:         chosen.Reason,
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
// provider when available on the request. Used by the handover orchestrator
// to run summarization on the caller's own account, avoiding tenant data
// crossing the deployment key boundary when the request is BYOK/client-keyed.
// Returns nil when no caller-supplied creds for the provider exist; callers
// then either use the deployment key (if request is fully deployment-keyed)
// or skip summarization (if request is BYOK/client-keyed for a different
// provider).
func resolveSummarizerCreds(ctx context.Context, provider string, headers http.Header) *Credentials {
	if provider == "" {
		return nil
	}
	if byok := BuildCredentialsMap(externalKeysFromContext(ctx)); byok != nil {
		if creds, ok := byok[provider]; ok {
			return creds
		}
	}
	return ExtractClientCredentials(provider, headers)
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
