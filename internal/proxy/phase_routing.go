package proxy

import (
	"net/http"

	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/router/turntype"
)

// Default phase routing knobs. Research is speed-biased (fast, decently-smart
// models for read/search/summarize); planning is quality-biased (the smartest
// tool-calling models) and pairs with a High tier floor. All satisfy the
// scorer's Alpha+SpeedWeight<=1.0 constraint and stay under its warning
// thresholds (alpha>0.9 / alpha+speed>0.95).
const (
	DefaultPhaseResearchAlpha       = 0.40
	DefaultPhaseResearchSpeedWeight = 0.45
	DefaultPhasePlanningAlpha       = 0.80
	DefaultPhasePlanningSpeedWeight = 0.0
)

// PhaseRoutingConfig parameterizes phase-aware routing: a detected coding-agent
// phase (research / planning) supplies default routing knobs and, for planning,
// a tier floor. Populated at boot from ROUTER_PHASE_* env vars. Enabled=false
// disables phase routing entirely.
type PhaseRoutingConfig struct {
	Enabled       bool
	ResearchKnobs router.Overrides
	PlanningKnobs router.Overrides
	// PlanningFloor is the minimum tier the scorer may pick during planning.
	// TierUnknown disables the floor.
	PlanningFloor catalog.Tier
}

// composePhaseKnobs merges phase defaults UNDER per-request overrides: every
// router.Overrides field is a pointer, so a non-nil per-request field wins and
// the phase default fills the gaps. This keeps an explicit x-weave-routing-*
// header authoritative over the phase nudge.
func composePhaseKnobs(base router.Overrides, reqKnobs *router.Overrides) *router.Overrides {
	merged := base
	if reqKnobs != nil {
		if reqKnobs.Alpha != nil {
			merged.Alpha = reqKnobs.Alpha
		}
		if reqKnobs.SpeedWeight != nil {
			merged.SpeedWeight = reqKnobs.SpeedWeight
		}
		if reqKnobs.OutputCostRatio != nil {
			merged.OutputCostRatio = reqKnobs.OutputCostRatio
		}
		if reqKnobs.ExpectedOutputTokens != nil {
			merged.ExpectedOutputTokens = reqKnobs.ExpectedOutputTokens
		}
		if reqKnobs.PerModelVerbosity != nil {
			merged.PerModelVerbosity = reqKnobs.PerModelVerbosity
		}
	}
	return &merged
}

// applyPlanningFloor unions every at-or-below-(floor-1) model into a defensive
// copy of req.ExcludedModels so the scorer's argmax can only land on an
// at-or-above-floor model. Soft: when no at-or-above-floor model is available to
// this deploy/request, the floor is skipped — a phase preference must never 503
// a request (the scorer's ExcludedModels filter is a hard filter).
func (s *Service) applyPlanningFloor(req router.Request) router.Request {
	floor := s.phaseRouting.PlanningFloor
	if floor == catalog.TierUnknown {
		return req
	}
	if !anyAvailableAtOrAboveFloor(s.availableModels, req.ExcludedModels, req.EnabledProviders, floor) {
		return req
	}
	below := catalog.AllowedAtOrBelow(floor - 1)
	if len(below) == 0 {
		return req
	}
	excluded := make(map[string]struct{}, len(req.ExcludedModels)+len(below))
	for k := range req.ExcludedModels {
		excluded[k] = struct{}{}
	}
	for k := range below {
		excluded[k] = struct{}{}
	}
	req.ExcludedModels = excluded
	return req
}

// anyAvailableAtOrAboveFloor reports whether at least one available, non-excluded
// model has a known tier at or above the floor AND is reachable by this request.
// Unknown-tier (custom) models never count toward the floor.
//
// enabledProviders is the per-request provider gate: nil means unrestricted
// (the scorer falls back to the deploy-wide set, which s.availableModels already
// reflects). When non-nil (BYOK / passthrough), a deploy-wide high-tier model
// the request can't authenticate to must NOT satisfy the floor — otherwise the
// floor would exclude every model the request CAN route to and the scorer would
// 503, breaking applyPlanningFloor's soft-fallback guarantee.
func anyAvailableAtOrAboveFloor(available, excluded, enabledProviders map[string]struct{}, floor catalog.Tier) bool {
	for m := range available {
		if _, ex := excluded[m]; ex {
			continue
		}
		t := catalog.TierFor(m)
		if t == catalog.TierUnknown || t < floor {
			continue
		}
		if enabledProviders != nil {
			if _, ok := catalog.ResolveBinding(m, enabledProviders); !ok {
				continue
			}
		}
		return true
	}
	return false
}

// phaseNote surfaces the detected workflow phase in the routing badge; empty
// when no phase was detected.
func phaseNote(res turnLoopResult) string {
	if res.Phase == turntype.PhaseNone {
		return ""
	}
	return string(res.Phase) + " phase"
}

// setPhaseHeader emits the x-router-phase debug header when a phase was detected.
// Absent when phase routing is disabled or no phase matched.
func setPhaseHeader(w http.ResponseWriter, res turnLoopResult) {
	if res.Phase != turntype.PhaseNone {
		w.Header().Set("x-router-phase", string(res.Phase))
	}
}
