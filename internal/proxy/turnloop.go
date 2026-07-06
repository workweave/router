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

// turnLoopResult bundles the routing decision and pin/planner state.
type turnLoopResult struct {
	Decision   router.Decision
	SessionKey [sessionpin.SessionKeyLen]byte
	TurnType   turntype.TurnType
	StickyHit  bool
	HardPinned bool
	// UsageBypass is true when the caller's own subscription has headroom:
	// ProxyMessages must serve the requested model straight through with no
	// billing debit, bypassing Decision's normal dispatch.
	UsageBypass bool
	PinTier     string
	PinAgeSec   int64
	// RequestedTier drives the session-pin role split (roleForTier) so a
	// low-tier background turn and a high-tier main turn never share a pin.
	RequestedTier catalog.Tier
	// PinRole is the session-pin role used for this turn, preventing a
	// low-tier background turn and a high-tier main turn from sharing a pin.
	PinRole string
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
}

// modelSwitched reports whether the Anthropic emit path must strip historical
// thinking blocks: true on the transition turn itself, or any turn after a
// session has ever switched. Claude Code re-sends the full transcript every
// turn, so stale-signed blocks from an earlier cross-model excursion would
// otherwise 400 with "Invalid signature in thinking block" on every later turn.
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
	// was applied (unwired, tenant-boundary skip, timeout, error, or empty
	// summary), so the full body passes through unchanged. No ledger row billed.
	FallbackToFullHistory bool
	// SummaryUsage is the summarizer call's upstream usage, so fireBilling can
	// debit it as a separate "_summary" ledger row. Zero on fallback/error paths.
	SummaryUsage handover.Usage
}

// isHardPinnedTurn reports whether a turn type bypasses pin lookup/write,
// planner, and scorer entirely via the boot-time hard pin. These turns are
// also skipped by proactive compaction: they are either tiny (probe/title-gen/
// classifier) or carry their own dedicated flow (Claude Code's compaction turn,
// whose request the router must not rewrite).
func (s *Service) isHardPinnedTurn(tt turntype.TurnType) bool {
	switch tt {
	case turntype.Compaction, turntype.Probe, turntype.TitleGen, turntype.Classifier:
		return true
	case turntype.SubAgentDispatch:
		return s.hardPinExplore
	default:
		return false
	}
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

	// Discounts covered models' cost term by the caller's observed subscription
	// headroom. nil (feature off / no headroom yet) leaves scoring unchanged.
	req.SubsidizedModelCostFactor = s.subsidyFactors(ctx, reqHeaders)

	// Hard pins bypass pin lookup/write, planner, and scorer entirely. Probes
	// and title-gen must never create a session pin: the Anthropic SDK fires
	// probes before the first real turn, and Claude Code fires title-gen
	// ~25ms before the real-conv call — an anchored pin would leak the
	// cheap-model decision into the conversation that follows.
	if s.isHardPinnedTurn(res.TurnType) {
		provider, model := s.hardPinProvider, s.hardPinModel
		// The boot-time hard-pin was computed over every registered provider,
		// but a BYOK request may only authenticate to a subset. Resolve
		// per-request against enabled-providers, and apply ExcludedModels
		// here too — this path bypasses the scorer, the only other place
		// exclusions are honored.
		if s.hardPinResolver != nil {
			p, m, ok := s.hardPinResolver(req.EnabledProviders, req.ExcludedModels)
			if !ok {
				log.Warn(
					"Hard-pin: no eligible provider for request; returning ErrClusterUnavailable",
					"turn_type", string(res.TurnType),
					"enabled_providers", sortedEnabledKeys(req.EnabledProviders),
				)
				return res, fmt.Errorf("hard-pin: no eligible provider for %s: %w", res.TurnType, cluster.ErrClusterUnavailable)
			}
			provider, model = p, m
		} else if _, excluded := req.ExcludedModels[model]; excluded {
			// No resolver wired (bundle load failed at boot), so we can't
			// pick an alternative. Serve the pin anyway but log the misroute.
			log.Warn(
				"Hard-pin: boot-time pin is in excluded_models but no resolver is wired to pick an alternative; serving pin anyway",
				"turn_type", string(res.TurnType),
				"model", model,
			)
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
	sessionKey := DeriveSessionKey(env, apiKeyID)

	// Runs before routing so the planner can price the pin's cache as dead on
	// the turn the client rewrote the prompt prefix; env isn't rewritten yet
	// so counts match what the client sent.
	res.PrefixTrimmed = s.compaction.checkAndRecord(
		sessionKey, installationID, res.PinRole,
		feats.MessageCount, len(env.AssistantToolCallSignatures()),
	)
	// prefixTrimFreeSwitch gates actions only; detection stays unconditional
	// so the compaction handover keeps working when the lever is off.
	prefixBroken := s.prefixTrimFreeSwitch && res.PrefixTrimmed
	if res.PrefixTrimmed {
		log.Info("turnloop detected client history trim",
			"message_count", feats.MessageCount,
			"free_switch_armed", prefixBroken,
		)
	}

	// Without a pin store, run the scorer and return its decision. The usage
	// bypass intercepts the fresh scorer decision here too (no pins to honor).
	if s.pinStore == nil {
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
	res.PriorServedModel = pin.LastServedModel
	res.SessionEverSwitched = pin.HasEverSwitched
	// Computed before any same-turn pin-drop guards below so it reflects the
	// prior turn's outcome; Service.effortEscalation gates whether it's acted on.
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

	// User-forced pins (/force-model) are immutable stickies with a never-expires
	// PinnedUntil, so they skip scorer/planner until /unforce-model expires them.
	// Still enforced per-request: (1) exclusion policy — a newly-excluded forced
	// model falls through to normal routing; (2) provider eligibility — a BYOK
	// request missing the pinned provider's creds falls through rather than
	// guaranteeing a 401.
	//
	// forcedTierFloor preserves the user's tier intent when the forced pin gets
	// dropped below (usually the session outgrew the model's context window):
	// the scorer call further down constrains the fresh decision to this tier
	// instead of collapsing to the cheap tier-default. TierUnknown = no constraint.
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
			// User still asked for this tier; constrain the fresh decision
			// to it below rather than losing the intent entirely.
			forcedTierFloor = catalog.TierFor(pin.Model)
		}
		// Treat as missing so downstream sticky branches don't dispatch to an
		// unauthorized provider. The row stays in storage — a later request
		// with the forced provider enabled resumes serving it.
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
		maxedModel := pin.LastServedModel
		if maxedModel == "" {
			maxedModel = pin.Model
		}
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
		pinFound = false
		pin = sessionpin.Pin{}
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
			modelCW := contextWindowForRequest(pin.Model)
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
					"token_estimate", pinTokenEstimate,
					"needed", needed,
					"model_context_window", modelCW,
				)
			}
		}
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

	// Positioned after hard-pin/forced-pin (higher precedence) but before the
	// tool-result/planner-disabled stickies below, so a stale pin from a prior
	// routed stretch can't make a tool_result continuation diverge from the
	// bypassed tool_use turn. The pin itself is untouched and resumes once
	// utilization crosses the threshold.
	if dec, ok := s.usageBypassDecision(ctx, reqHeaders, req); ok {
		res.Decision = dec
		res.UsageBypass = true
		return res, nil
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
			} else {
				log.Info("tier-constrained reroute found no candidate; using unconstrained scorer",
					"forced_tier", forcedTierFloor.String(), "err", derr)
			}
		}
	}
	if !routed {
		dec, err := s.routeFor(ctx, req)
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
		// A trimmed prefix kills the cache even inside the provider TTL.
		PinCacheCold: pinFound && (!cacheWarm(pin) || prefixBroken),
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
		SessionKey:     sessionKey,
		Role:           role,
		InstallationID: installationID,
		Provider:       chosen.Provider,
		Model:          chosen.Model,
		// No scorer runs on a plain refresh, so carry the existing pair
		// forward unchanged (ON CONFLICT preserves an empty one).
		PairedProvider:        existing.PairedProvider,
		PairedModel:           existing.PairedModel,
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
		PairedProvider: pairedProvider,
		PairedModel:    pairedModel,
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
