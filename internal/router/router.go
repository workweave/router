// Package router defines the Router interface and its Decision/Request types.
package router

import "context"

type Overrides struct {
	// Alpha is the raw per-cluster quality weight applied UNIFORMLY across every
	// cluster (the eval/debug "sledgehammer" set via x-weave-routing-alpha), so
	// a bake-off can probe a single global alpha regardless of per-cluster
	// quality dispersion.
	Alpha *float64
	// QualityBias is the user-facing "quality vs price" dial in [0, 1]. Unlike
	// Alpha, it's mapped through a per-bundle calibration (dialToAlpha /
	// computeDialCalibration) so the slider's midrange moves the routed model
	// mix instead of hitting a dead zone; endpoints still pin to all-cheapest
	// (0) and best-per-cluster quality (1). routingKnobsForRequest returns
	// either header or installation knobs, never a merge, so if both Alpha and
	// QualityBias are set, QualityBias wins.
	QualityBias          *float64
	SpeedWeight          *float64
	OutputCostRatio      *float64
	ExpectedOutputTokens *int
	PerModelVerbosity    *bool
}

type Request struct {
	RequestedModel       string
	EstimatedInputTokens int
	HasTools             bool
	// HasImages: scorer drops text-only models from the eligible pool; turn
	// loop evicts a text-only session pin.
	HasImages  bool
	PromptText string
	// ConversationMessages is provider-neutral visible history for sidecar
	// routers that need multi-turn context.
	ConversationMessages []ConversationMessage
	// AvailableTools is a bounded list of tool names declared on this request.
	AvailableTools []string
	// Per-request provider gating — nil means unrestricted.
	EnabledProviders map[string]struct{}
	// Per-request model exclusion — nil or empty means no exclusion.
	// If filtering empties eligible set, scorer returns ErrNoEligibleProvider.
	ExcludedModels map[string]struct{}
	// PreferredModels is the per-installation priority ranking (index 0 =
	// first). The scorer adds a small rank-decaying bonus to each preferred
	// model's score — enough to win close calls, not to override a clearly
	// better model. Entries not in the eligible pool are ignored.
	PreferredModels []string
	RoutingKnobs    *Overrides // NEW: parsed dynamic knobs
	// SubsidizedModelCostFactor is the per-model rate-limit headroom factor in
	// [epsilon, 1] for models the caller's subscription covers (see
	// internal/proxy/usage): ~epsilon when the window has slack, rising to 1 as
	// it binds. Absent = no subsidy. The cluster scorer treats it as a
	// preference signal (score bonus proportional to 1−factor, cost axis left
	// at full catalog so Haiku↔Opus spread is preserved); the planner instead
	// treats it as a literal cost multiplier for dollar-EV cache-switch math.
	SubsidizedModelCostFactor map[string]float64
}

type ConversationMessage struct {
	Role        string
	Text        string
	ToolCalls   []ConversationToolCall
	ToolResults []ConversationToolResult
}

type ConversationToolCall struct {
	Name      string
	InputKeys []string
}

type ConversationToolResult struct {
	ToolUseID string
	IsError   bool
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
	// Strategy identifies opt-in sidecar routers ("rl", "hmm") when metadata
	// is produced outside the default cluster scorer.
	Strategy string
	// RouteID is an opaque sidecar correlation id. Outcome reporters and logs
	// use it to join route decisions to final dispatch usage.
	RouteID string
	// DisplayMarker is an optional, already-humanized route badge. Sidecars
	// use this to show strategy-specific labels without moving their display
	// logic into router-internal.
	DisplayMarker      string
	EffectiveKnobsHash uint64 // NEW: canonical knobs hash for response-cache isolation
	// CandidateScores: full pre-argmax blended score per eligible model, for
	// off-policy analysis (contextual bandit substrate). Doesn't affect routing.
	CandidateScores map[string]float32
	// CandidateProviders: per-request resolved provider per eligible model, so
	// an exploration policy can switch to an in-band peer using this request's
	// binding (correct under BYOK) rather than a boot-time default.
	CandidateProviders map[string]string
	// Propensity is the probability the chosen model was selected under the
	// acting policy: 1.0 for deterministic argmax, <1.0 under exploration.
	// Logged as the importance weight an off-policy estimator needs.
	Propensity float32
	// PairedModel is the runner-up model — the other half of the {Model,
	// PairedModel} band pair Stage 1 freezes into the session pin so a later
	// per-turn policy can swap without re-running the scorer. Empty when only
	// one model is eligible. PairedProvider/PairedScore are informational and
	// don't affect this request's routing.
	PairedModel    string
	PairedProvider string
	PairedScore    float32
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
