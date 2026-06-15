package proxy

import (
	"context"
	"encoding/json"

	"workweave/router/internal/observability"
	"workweave/router/internal/observability/otel"
	"workweave/router/internal/router"
)

// observationContext bundles per-request routing values shared by the OTel
// span and telemetry row.
type observationContext struct {
	// ClusterIDs is the top-p cluster set, widened to int32 for SQLC's INT[].
	ClusterIDs []int32
	// CandidateModels mirrors the scorer's eligible argmax set.
	CandidateModels []string
	// ChosenScore is the argmax score. Pointer so 0.0 stays distinct from
	// "not a cluster decision".
	ChosenScore *float64
	// ClusterRouterVersion is the artifact version that produced this decision.
	ClusterRouterVersion string
	// TTFTMs is the upstream-request-to-first-byte delta in ms. Pointer because
	// zero is a legitimate sub-millisecond measurement.
	TTFTMs *int64
	// CandidateScores is the pre-argmax score vector marshaled to JSON for the
	// jsonb column. nil when the router exposes no score vector. Off-policy
	// substrate only — never read back on the request path.
	CandidateScores []byte
	// Propensity is the probability the chosen model was selected under the
	// acting policy. Pointer so 0.0 stays distinct from "not a cluster
	// decision". 1.0 for deterministic argmax.
	Propensity *float64
	// FreshDecisionModel is the scorer's fresh pick this turn, captured even
	// when the planner returned STAY (decision_model then names the pinned model
	// served). Empty when the scorer did not run. Shadow-mode instrumentation
	// for the hysteresis downgrade lever.
	FreshDecisionModel string
	// FreshCandidateScores is the fresh scorer's pre-argmax score vector
	// marshaled to JSON, captured even on STAY. Distinct from CandidateScores,
	// which mirrors the FINAL decision and is NULL on STAY. nil when the scorer
	// did not run / exposed no vector.
	FreshCandidateScores []byte
}

// buildObservationContext derives the observation bundle from the routing
// decision and request context. Nil-safe: returns a zero value when sources
// are absent. fresh is the scorer's recommendation this turn (which equals
// decision on a SWITCH/fresh-pin write, but differs on a STAY where decision
// rehydrates from the pin); its score vector is captured separately so the
// hysteresis downgrade opportunity is measurable on STAY turns.
func buildObservationContext(ctx context.Context, decision, fresh router.Decision) observationContext {
	obs := observationContext{}
	// Fresh scorer recommendation, captured independently of the final decision
	// so STAY turns (decision == pin, no metadata) still record what a re-score
	// would have picked.
	if fresh.Model != "" {
		obs.FreshDecisionModel = fresh.Model
	}
	if md := fresh.Metadata; md != nil && len(md.CandidateScores) > 0 {
		if b, err := json.Marshal(md.CandidateScores); err == nil {
			obs.FreshCandidateScores = b
		} else {
			observability.Get().Debug("Failed to marshal fresh_candidate_scores for telemetry", "err", err)
		}
	}
	if md := decision.Metadata; md != nil {
		if len(md.ClusterIDs) > 0 {
			obs.ClusterIDs = make([]int32, len(md.ClusterIDs))
			for i, k := range md.ClusterIDs {
				obs.ClusterIDs[i] = int32(k)
			}
		}
		if len(md.CandidateModels) > 0 {
			obs.CandidateModels = append([]string(nil), md.CandidateModels...)
		}
		// ChosenScore is unconditional inside md != nil: a != 0 guard
		// would silently drop legitimate zero scores.
		score := float64(md.ChosenScore)
		obs.ChosenScore = &score
		obs.ClusterRouterVersion = md.ClusterRouterVersion
		// Propensity is meaningful only alongside a score vector (the scorer
		// sets both together); gate on CandidateScores so a non-scoring router
		// leaves the column NULL instead of logging the Go zero 0.0.
		if len(md.CandidateScores) > 0 {
			prop := float64(md.Propensity)
			obs.Propensity = &prop
			if b, err := json.Marshal(md.CandidateScores); err == nil {
				obs.CandidateScores = b
			} else {
				// Telemetry loss is acceptable; a marshal failure must never
				// fail the request, so log and leave the column NULL.
				observability.Get().Debug("Failed to marshal candidate_scores for telemetry", "err", err)
			}
		}
	}
	if t := otel.TimingFrom(ctx); t != nil {
		if ms := t.Ms(&t.UpstreamRequestNanos, &t.UpstreamFirstByteNanos); ms > 0 {
			obs.TTFTMs = &ms
		}
	}
	return obs
}

// applySpanAttrs records routing fields on an OTel AttrBuilder.
func (o observationContext) applySpanAttrs(b *otel.AttrBuilder) {
	if len(o.ClusterIDs) > 0 {
		// Widen int32 → int for AttrBuilder.IntSlice.
		ids := make([]int, len(o.ClusterIDs))
		for i, k := range o.ClusterIDs {
			ids[i] = int(k)
		}
		b.IntSlice("routing.cluster_ids", ids)
	}
	if o.ChosenScore != nil {
		b.Float64("routing.chosen_score", *o.ChosenScore)
	}
	if o.Propensity != nil {
		b.Float64("routing.propensity", *o.Propensity)
	}
	if len(o.CandidateScores) > 0 {
		b.String("routing.candidate_scores", string(o.CandidateScores))
	}
	if o.ClusterRouterVersion != "" {
		b.String("routing.cluster_version", o.ClusterRouterVersion)
	}
	if o.TTFTMs != nil {
		b.Int64("latency.ttft_ms", *o.TTFTMs)
	}
}
