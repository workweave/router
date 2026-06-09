// Package router defines the Router interface and its Decision/Request types.
package router

import "context"

type Overrides struct {
	Alpha                *float64
	SpeedWeight          *float64
	OutputCostRatio      *float64
	ExpectedOutputTokens *int
	PerModelVerbosity    *bool
}

type Request struct {
	RequestedModel       string
	EstimatedInputTokens int
	HasTools             bool
	// HasImages is true when the request carries image content. The scorer
	// drops text-only models from the eligible pool; the turn loop evicts a
	// text-only session pin.
	HasImages  bool
	PromptText string
	// Per-request provider gating — nil means unrestricted.
	EnabledProviders map[string]struct{}
	// Per-request model exclusion — nil or empty means no exclusion.
	// If filtering empties eligible set, scorer returns ErrNoEligibleProvider.
	ExcludedModels map[string]struct{}
	RoutingKnobs   *Overrides // NEW: parsed dynamic knobs
}

type Decision struct {
	Provider string
	Model    string
	Reason   string
	// Nil for non-content-aware routers; nil-check before dereferencing.
	Metadata *RoutingMetadata
}

// RoutingMetadata lets downstream components reuse the embedding and
// cluster context without recomputing.
type RoutingMetadata struct {
	Embedding            []float32
	ClusterIDs           []int // Sorted ascending; [0] is NOT necessarily closest.
	CandidateModels      []string
	ChosenScore          float32
	ClusterRouterVersion string
	EffectiveKnobsHash   uint64 // NEW: canonical knobs hash for response-cache isolation
	// CandidateScores is the full pre-argmax blended score per eligible model.
	// Surfaced for off-policy analysis (the substrate a contextual bandit needs);
	// nil for routers that don't compute a score vector. Does not affect routing.
	CandidateScores map[string]float32
	// CandidateProviders is the per-request resolved provider for each eligible
	// model, so an exploration policy can switch to an in-band peer using this
	// request's binding (correct under BYOK) instead of a boot-time default.
	// nil for routers that don't resolve providers. Does not affect routing.
	CandidateProviders map[string]string
	// Propensity is the probability the chosen model was selected under the
	// acting policy. 1.0 for a deterministic argmax; <1.0 only when an
	// exploration policy randomizes. Logged so logged decisions carry the
	// importance weight an off-policy estimator requires.
	Propensity float32
}

type Router interface {
	Route(ctx context.Context, req Request) (Decision, error)
}

type routingKnobsContextKey struct{}

// WithRoutingKnobs stashes Overrides on ctx.
func WithRoutingKnobs(ctx context.Context, o *Overrides) context.Context {
	if o == nil {
		return ctx
	}
	return context.WithValue(ctx, routingKnobsContextKey{}, o)
}

// RoutingKnobsFromContext returns Overrides from ctx or nil.
func RoutingKnobsFromContext(ctx context.Context) *Overrides {
	o, _ := ctx.Value(routingKnobsContextKey{}).(*Overrides)
	return o
}
