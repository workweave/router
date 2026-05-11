package proxy

import (
	"context"

	"workweave/router/internal/observability/otel"
	"workweave/router/internal/router"
)

// observationContext bundles the per-request routing-brain values that
// both the OTel span and the telemetry row record. Captured once per
// request via buildObservationContext and applied to the span via
// applySpanAttrs; the InsertTelemetryParams populate from the same
// instance. The shape exists because the two writes used to be
// hand-duplicated across `ProxyMessages` and
// `ProxyOpenAIChatCompletion`, and W-1335 / W-1309 will add more
// fields to the bundle — adding them once here beats updating two
// physically separated blocks.
//
// All fields are zero-valued for non-cluster decisions
// (`router.Decision.Metadata == nil`); the telemetry adapter maps zero
// values to NULL columns.
type observationContext struct {
	// ClusterIDs is the top-p cluster set widened to int32 for SQLC's
	// INT[] column type. Nil when not a cluster decision.
	ClusterIDs []int32
	// CandidateModels mirrors the scorer's eligible argmax set. Nil
	// when not a cluster decision.
	CandidateModels []string
	// ChosenScore is the argmax score on the chosen model. Pointer so
	// "argmax produced literal 0.0" stays distinct from "not a cluster
	// decision" — collapsing them with a `!= 0` guard silently drops
	// legitimate zero scores.
	ChosenScore *float64
	// ClusterRouterVersion is the artifact version that produced this
	// decision. Empty for non-cluster routers.
	ClusterRouterVersion string
	// TTFTMs is the upstream-request → first-byte delta in ms. Pointer
	// because the timing context may not have stamped both ends (zero
	// is a legitimate sub-millisecond measurement, not "unmeasured").
	TTFTMs *int64
}

// buildObservationContext derives the observation bundle from the
// routing decision and request context. Cluster fields come off
// `decision.Metadata`; TTFT comes off `otel.TimingFrom(ctx)`. Both
// sources are nil-safe — non-cluster routes and OTel-disabled paths
// return a zero observationContext that the telemetry writer
// translates into NULL columns.
func buildObservationContext(ctx context.Context, decision router.Decision) observationContext {
	obs := observationContext{}
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
		// Unconditional inside `md != nil`: the *float64 distinguishes
		// "not a cluster decision" (outer nil) from "argmax was 0"
		// (&0). Outer nil-check already excludes the former; an inner
		// `!= 0` guard would silently drop legitimate zero scores.
		score := float64(md.ChosenScore)
		obs.ChosenScore = &score
		obs.ClusterRouterVersion = md.ClusterRouterVersion
	}
	if t := otel.TimingFrom(ctx); t != nil {
		if ms := t.Ms(&t.UpstreamRequestNanos, &t.UpstreamFirstByteNanos); ms > 0 {
			obs.TTFTMs = &ms
		}
	}
	return obs
}

// applySpanAttrs records the routing-observability fields on an OTel
// AttrBuilder using the same gating the telemetry row uses, so the
// span and the DB row stay symmetric. Caller invokes this before
// `AttrBuilder.Build`. No-op on a zero observationContext, so the
// non-cluster path doesn't need a guard.
func (o observationContext) applySpanAttrs(b *otel.AttrBuilder) {
	if len(o.ClusterIDs) > 0 {
		// AttrBuilder.IntSlice consumes []int; widen back from the
		// DB-shape int32 we carry on the struct. One alloc per span
		// is acceptable; the alternative is dragging both shapes
		// around just to avoid this loop.
		ids := make([]int, len(o.ClusterIDs))
		for i, k := range o.ClusterIDs {
			ids[i] = int(k)
		}
		b.IntSlice("routing.cluster_ids", ids)
	}
	if o.ChosenScore != nil {
		b.Float64("routing.chosen_score", *o.ChosenScore)
	}
	if o.ClusterRouterVersion != "" {
		b.String("routing.cluster_version", o.ClusterRouterVersion)
	}
	if o.TTFTMs != nil {
		b.Int64("latency.ttft_ms", *o.TTFTMs)
	}
}
