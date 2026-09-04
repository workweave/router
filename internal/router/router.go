// Package router defines the Router interface and its Decision/Request types.
package router

import "context"

// WireFormat identifies the client-facing request representation. It is kept
// independent from provider names so the router package remains an inner-ring
// value-types package.
type WireFormat string

const (
	WireFormatAnthropic WireFormat = "anthropic_messages"
	WireFormatOpenAI    WireFormat = "openai"
	WireFormatGemini    WireFormat = "gemini"
)

// TranslationEndpoint identifies the source endpoint contract within a wire
// format. Different OpenAI endpoints carry different semantic unions.
type TranslationEndpoint string

const (
	EndpointAnthropicMessages TranslationEndpoint = "messages"
	EndpointOpenAIChat        TranslationEndpoint = "chat_completions"
	EndpointOpenAIResponses   TranslationEndpoint = "responses"
	EndpointGeminiGenerate    TranslationEndpoint = "generate_content"
)

// TranslationRequirements records semantics which must survive a route. It is
// deliberately additive to the existing scoring hints: HasTools and HasImages
// remain quality signals while these fields are compatibility constraints.
type TranslationRequirements struct {
	SourceFormat WireFormat
	Endpoint     TranslationEndpoint

	FunctionTools      bool
	CustomTools        bool
	ReasoningReplay    bool
	ReasoningSignature bool
	Images             bool
	Audio              bool
	Files              bool
	CitationsOrSearch  bool
	StructuredOutput   bool
	PromptCacheControl bool
	UsageDetail        bool

	// NativeOnly requires the source wire family and endpoint to reach the
	// upstream unchanged. It is used for currently unrepresentable unions,
	// never as a quality preference.
	NativeOnly bool
}

// IsZero reports whether the request has no compatibility contract.
func (r TranslationRequirements) IsZero() bool {
	return r.SourceFormat == "" && r.Endpoint == "" && !r.FunctionTools && !r.CustomTools &&
		!r.ReasoningReplay && !r.ReasoningSignature && !r.Images && !r.Audio && !r.Files &&
		!r.CitationsOrSearch && !r.StructuredOutput && !r.PromptCacheControl && !r.UsageDetail && !r.NativeOnly
}

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
	// ForceEffort, when non-empty, is the user-requested effort level
	// (x-weave-effort header / :level suffix). Wins over effortEscalation;
	// per-model caps (xhigh→max on non-CapXhighEffort) still apply.
	ForceEffort string
}

type Request struct {
	RequestedModel string
	// ForceModel is the canonical model named by a valid explicit force-model
	// request. Router decorators must preserve the underlying selection rather
	// than applying alternative-policy behavior such as exploration.
	ForceModel string
	// ForceCluster is the sidecar classifier-group label forced by
	// x-weave-force-cluster on this turn. Empty means no cluster constraint.
	// Unlike ForceModel (which pins one canonical model), it constrains the pool
	// the policy's own selection may draw from; the sidecar is never told a
	// cluster is forced, so enforcement is a router-side re-check of the live
	// Result.RankedFallback after the sidecar responds (see
	// policy.ApplyClusterArmOverridesRequireMatch). Meaningless outside the
	// hmm/hmm_embedding strategies, which the proxy construction sites reject
	// with a typed error rather than silently ignoring.
	ForceCluster         string
	EstimatedInputTokens int
	// OrganizationID and InstallationID are opaque external identifiers used
	// to correlate policy decisions with rollout and privacy state.
	OrganizationID string
	InstallationID string
	// ClientApp is the normalized request harness (for example claude-code,
	// codex, cursor, or api).
	ClientApp string
	// RolloutID correlates eval/shadow traffic across route and outcome events.
	RolloutID string
	HasTools  bool
	// HasImages: scorer drops text-only models from the eligible pool; turn
	// loop evicts a text-only session pin.
	HasImages bool
	// TranslationRequirements are hard semantic requirements inferred at
	// ingress. Routing implementations must not turn their empty result into
	// an incompatible fallback pool.
	TranslationRequirements TranslationRequirements
	// ReasoningConfigurationSHA256 and ToolConfigurationSHA256 are privacy-safe hashes; raw request content is never stored here.
	ReasoningConfigurationSHA256 string
	ToolConfigurationSHA256      string
	PromptText                   string
	// ConversationMessages is provider-neutral visible history for sidecar
	// routers that need multi-turn context.
	ConversationMessages []ConversationMessage
	// AvailableTools is a bounded list of tool names declared on this request.
	// Deprecated: use Tools for structural capability decisions.
	AvailableTools []string
	// Tools preserves the provider-facing declaration shape needed to
	// distinguish provider-executed tools from client-executed functions.
	Tools []ToolDescriptor
	// PolicyTurnContext carries persisted, content-free state from the turn
	// orchestrator. Nil keeps older policy clients wire-compatible.
	PolicyTurnContext *PolicyTurnContext
	// HistoryTruncated records deterministic ingress rewrites that happened
	// before the turn orchestrator could build PolicyTurnContext.
	HistoryTruncated bool
	// FeedbackKey/FeedbackRole are optional opaque correlation fields for
	// sidecar routers that accept per-session feedback.
	FeedbackKey  string
	FeedbackRole string
	// ClientSessionID is the calling client's own session id (e.g. Claude
	// Code metadata.user_id), when present on the inbound envelope.
	ClientSessionID string
	// Per-request provider gating — nil means unrestricted.
	EnabledProviders map[string]struct{}
	// CustomBindings maps catalog model ID to configuration-declared providers
	// (from a key's model_aliases). They rank after catalog bindings, so a
	// wired direct vendor still wins — except under GatewayProviders, where the
	// aliases are the only thing that can be routed.
	CustomBindings map[string][]string
	// GatewayProviders is the installation's BYOK gateway providers. Non-empty
	// means gateway-exclusive routing: only aliased models are routable.
	GatewayProviders map[string]struct{}
	// Per-request model exclusion — nil or empty means no exclusion.
	// If filtering empties eligible set, scorer returns ErrNoEligibleProvider.
	// Full union: installation excluded_models plus request-time safety filters.
	ExcludedModels map[string]struct{}
	// AllowedModels is the org's positive model allowlist as a set; nil means no
	// restriction. Enforcement is via ExcludedModels (proxy desugars the allowlist
	// into it); this field lets scorer/resolver NAME the constraint in errors
	// rather than reporting a large exclusion list.
	AllowedModels map[string]struct{}
	// SafetyExcludedModels holds only the hard request-time constraints a model
	// physically cannot satisfy: context-window overflow and gemini-unsigned
	// history — not the installation's excluded_models policy. The bypass gate
	// consults this so policy exclusions don't block pass-through, but physical
	// constraints still do.
	SafetyExcludedModels map[string]struct{}
	// AutomaticExcludedModels is the deployment-wide set Weave has withdrawn
	// from AUTOMATIC selection. Deliberately not folded into ExcludedModels:
	// that set is hard (it also rejects an explicit /force-model pin), whereas
	// this one must leave a user's explicit pin serving. Routers apply it as a
	// soft filter — if honoring it would empty the pool, it is ignored for that
	// turn rather than failing the request.
	AutomaticExcludedModels map[string]struct{}
	// PreferredModels is the per-installation priority ranking (index 0 =
	// first). The scorer adds a small rank-decaying bonus to each preferred
	// model's score — enough to win close calls, not to override a clearly
	// better model. Entries not in the eligible pool are ignored.
	PreferredModels []string
	// RoutingIntent is a strategy-neutral preset for future low/medium/high
	// routing modes. Empty means use the installation's normal policy.
	RoutingIntent string
	RoutingKnobs  *Overrides // NEW: parsed dynamic knobs
	// TrainingAllowed is false unless the organization is explicitly eligible
	// for policy learning. Serving must continue when it is false.
	TrainingAllowed bool
	// CaptureMode describes the operational content-retention mode without
	// granting permission for policy learning.
	CaptureMode string
	// DebugEnabled gates detailed policy diagnostics in requests and responses.
	DebugEnabled bool
	// ShadowMode marks a decision-only comparison that must never be treated as
	// served traffic or admitted into online-learning inputs.
	ShadowMode bool
	// SubsidizedModelCostFactor is the per-model rate-limit headroom factor in
	// [epsilon, 1] for models the caller's subscription covers (see
	// internal/proxy/usage): ~epsilon when the window has slack, rising to 1 as
	// it binds. Absent = no subsidy. The cluster scorer treats it as a
	// preference signal (score bonus proportional to 1−factor, cost axis left
	// at full catalog so Haiku↔Opus spread is preserved); the planner instead
	// treats it as a literal cost multiplier for dollar-EV cache-switch math.
	SubsidizedModelCostFactor map[string]float64
	// ClusterArmOverrides is the per-API-key HMM cluster allowlist: cluster label
	// → ordered catalog model IDs (index 0 = highest priority). Absent clusters
	// keep the artifact default. Nil means no override.
	ClusterArmOverrides map[string][]string
}

// ToolDescriptor records the source declaration facts relevant to routing.
type ToolDescriptor struct {
	Name           string
	Type           string
	ServerExecuted bool
}

type ConversationMessage struct {
	Role        string                   `json:"role"`
	Text        string                   `json:"text,omitempty"`
	ToolCalls   []ConversationToolCall   `json:"tool_calls,omitempty"`
	ToolResults []ConversationToolResult `json:"tool_results,omitempty"`
}

type ConversationToolCall struct {
	Name      string   `json:"name,omitempty"`
	InputKeys []string `json:"input_keys,omitempty"`
	InputJSON string   `json:"input_json,omitempty"`
}

type ConversationToolResult struct {
	ToolUseID     string `json:"tool_use_id,omitempty"`
	IsError       bool   `json:"is_error,omitempty"`
	Text          string `json:"text,omitempty"`
	ResultPresent bool   `json:"result_present,omitempty"`
	CharCount     int    `json:"char_count,omitempty"`
	ByteCount     int    `json:"byte_count,omitempty"`
	ExitCategory  string `json:"exit_category,omitempty"`
}

// PolicyTurnContext describes the current per-turn decision state without
// carrying raw tool-result content.
type PolicyTurnContext struct {
	VisibleTurnIndex    int
	SessionTurnCount    int
	TurnType            string
	PreviousServedModel string
	PreviousProvider    string
	CacheState          string
	PriorOutputTokens   *int
	SessionEverSwitched bool
	HistoryTruncated    bool
}

const (
	PolicyCacheStateUnknown = "unknown"
	PolicyCacheStateCold    = "cold"
	PolicyCacheStateWarm    = "warm"
)

type Decision struct {
	Provider string
	Model    string
	// Effort is the canonical reasoning-effort level selected for this turn
	// ("low".."xhigh"), empty when the policy expressed no preference. Model
	// stays a bare catalog ID so catalog.ByID lookups keep working; effort is
	// applied by the emit path via EmitOptions.ForceEffort. An effort-only
	// change registers as a model switch via ServedIdentity() because it
	// invalidates thinking-block signatures and the prompt-cache prefix.
	Effort string
	Reason string
	// Nil for non-content-aware routers; nil-check before dereferencing.
	Metadata *RoutingMetadata
}

// ServedIdentity returns the model identity to persist and compare across
// turns: the bare model when no effort was selected, else "model:effort".
// Pins written before effort existed hold a bare ID, so the first post-rollout
// turn compares "m" against "m:high" and reports a switch — conservative, not unsafe.
func (d Decision) ServedIdentity() string {
	if d.Effort == "" {
		return d.Model
	}
	return d.Model + ":" + d.Effort
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
	// PolicyRouteKey is the generic policy-internal bucket/arm key used for
	// online learning. HMM routing_bucket values map here.
	PolicyRouteKey string
	// PolicyGroup is the policy-internal cluster/group the decision was drawn
	// from (HMM complexity cluster). Session pins persist it so a later turn can
	// tell a within-cluster reroute from a genuine cluster change.
	PolicyGroup string
	// Policy artifact metadata identifies the immutable production package.
	PolicyArtifactID     string
	PolicyArtifactSHA256 string
	RosterVersion        string
	// SidecarTimings is set only on fresh sidecar decisions; never persist
	// into pins or a replayed pin re-emits a stale measurement.
	SidecarTimings *SidecarTimings
	// SidecarStats is set only on fresh sidecar decisions; never persist
	// into pins or a replayed pin re-emits a stale measurement.
	SidecarStats  *SidecarServingStats
	SelectedArmID string
	// SelectedRosterArmID is the served arm as the sidecar names it, effort
	// suffix intact — SelectedArmID has been split for binding resolution, so
	// only this form keys into ArmScores.
	SelectedRosterArmID  string
	SelectedUpstreamID   string
	BindingIndex         int
	CandidateArmIDs      []string
	SidecarSchemaVersion string
	DebugRef             string
	// AuthoritativePerTurnSelection means downstream orchestration may retry
	// providers but must not replace this decision's model on this turn.
	AuthoritativePerTurnSelection bool
	// DisplayMarker is an optional, already-humanized route badge. Sidecars
	// use this to show strategy-specific labels without moving their display
	// logic into router-internal.
	DisplayMarker      string
	EffectiveKnobsHash uint64 // NEW: canonical knobs hash for response-cache isolation
	// CandidateScores: full pre-argmax blended score per eligible model, for
	// off-policy analysis (contextual bandit substrate). Doesn't affect routing.
	CandidateScores map[string]float32
	// CandidateArmScores preserves scores for configuration-level actions.
	CandidateArmScores map[string]float32
	// CandidateProviders: per-request resolved provider per eligible model, so
	// an exploration policy can switch to an in-band peer using this request's
	// binding (correct under BYOK) rather than a boot-time default.
	CandidateProviders map[string]string
	// CandidateArmProviders preserves providers for configuration-level actions.
	CandidateArmProviders map[string]string
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
	// ArmScores is the router-selected preference-adjusted score map for the
	// cluster this decision was drawn from. Absent on fixed/legacy rosters.
	ArmScores map[string]float32
}

// SidecarTimings holds the sidecar's per-stage decision cost in milliseconds.
// A nil field was not measured; present 0 is a real sub-millisecond measurement.
type SidecarTimings struct {
	EmbedMs  *float64 // embedding round trip
	SelectMs *float64 // arm selection
	OtherMs  *float64 // remainder of the sidecar's route handler
}

// SidecarServingStats holds per-decision serving stats sampled by the
// sidecar: per-request embed-cache deltas (present only when the embed
// stage ran) and instance gauges. A nil field was not reported by the
// sidecar; present 0 is a measured zero and must be preserved as such.
type SidecarServingStats struct {
	EmbedCacheHits      *int64
	EmbedCacheMisses    *int64
	EmbedCacheEvictions *int64
	RoutesInflight      *int64
	OverrunsLive        *int64
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
