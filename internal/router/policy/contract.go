package policy

import (
	"context"

	"workweave/router/internal/router"
)

// SchemaVersionV1 is the first stable policy-sidecar wire contract.
const SchemaVersionV1 = "policy_router_v1"

// SchemaVersionV2 identifies sidecar requests that carry configuration-level
// arm identities and require arm-aware selection when a roster is ambiguous.
const SchemaVersionV2 = "policy_router_v2"

const (
	ExecutionModeServing = "serving"
	ExecutionModeShadow  = "shadow"
	ExecutionModePreview = "preview"
)

// Capabilities declares which optional harness behaviors a policy supports.
type Capabilities struct {
	SchemaVersion            string `json:"schema_version"`
	ReportsOutcomes          bool   `json:"reports_outcomes"`
	ReportsFeedback          bool   `json:"reports_feedback"`
	HonorsPreferredModels    bool   `json:"honors_preferred_models"`
	HonorsQualityPriceBias   bool   `json:"honors_quality_price_bias"`
	SupportsDebugRouteDetail bool   `json:"supports_debug_route_detail"`
	SupportsPreview          bool   `json:"supports_preview"`
	SupportsShadow           bool   `json:"supports_shadow"`
	// ReportsRankedFallback declares that the sidecar returns ranked_fallback on
	// the serving /route response. When false, cluster arm overrides fail open.
	ReportsRankedFallback bool `json:"reports_ranked_fallback"`
	// AuthoritativePerTurnSelection makes eligible main/tool-result decisions
	// model-authoritative through dispatch.
	AuthoritativePerTurnSelection bool `json:"authoritative_per_turn_selection"`
}

// CapabilitySource returns the live capability set of a sidecar router.
// Implementations must be safe for concurrent reads and background writes.
type CapabilitySource interface {
	CurrentCapabilities() Capabilities
}

// StrategySpec is the complete proxy registration for one policy strategy.
// Reporter capabilities are discovered from Router when the spec is installed.
type StrategySpec struct {
	Strategy     router.Strategy
	Router       router.Router
	Unavailable  error
	Capabilities Capabilities
}

// Query contains the strategy-neutral request context supplied to a policy.
type Query struct {
	SchemaVersion        string
	Strategy             router.Strategy
	ExecutionMode        string
	RouteID              string
	OrganizationID       string
	InstallationID       string
	ClientApp            string
	RolloutID            string
	RequestedModel       string
	PromptText           string
	ConversationMessages []router.ConversationMessage
	AvailableTools       []string
	FeedbackKey          string
	FeedbackRole         string
	ClientSessionID      string
	TurnContext          *router.PolicyTurnContext
	EstimatedInputTokens int
	HasTools             bool
	HasImages            bool
	RoutingIntent        string
	PreferredModels      []string
	RoutingKnobs         *router.Overrides
	TrainingAllowed      bool
	CaptureMode          string
	DebugEnabled         bool
	Candidates           []Candidate
}

// Result is a policy sidecar's selected candidate and decision metadata.
type Result struct {
	SchemaVersion        string
	RouteID              string
	ArmID                string
	Model                string
	Provider             string
	Score                float64
	CandidateScores      map[string]float32
	ScoreKind            string
	Reason               string
	PolicyState          string
	PolicyGroup          string
	PolicyLabel          string
	PolicyRouteKey       string
	Confidence           *float64
	Margin               *float64
	Propensity           float64
	DisplayMarker        string
	PolicyArtifactID     string
	PolicyArtifactSHA256 string
	RosterVersion        string
	DebugRef             string
	Debug                map[string]interface{}
	// RankedFallback is every classifier group in serving fallback order, each
	// with its full and eligible roster arms. Populated when ReportsRankedFallback;
	// empty on older sidecars (arm override fails open).
	RankedFallback []PreviewGroup
}

// PreviewGroup records one classifier group in serving fallback order.
type PreviewGroup struct {
	Group        string   `json:"group"`
	Probability  float64  `json:"probability"`
	RosterArms   []string `json:"roster_arms"`
	EligibleArms []string `json:"eligible_arms"`
}

// PreviewResult is a side-effect-free policy evaluation with every arm in the
// first nonempty ranked group, plus authoritative router resolution details.
type PreviewResult struct {
	SchemaVersion         string             `json:"schema_version"`
	RouteID               string             `json:"route_id"`
	Strategy              router.Strategy    `json:"strategy"`
	PolicyArtifactID      string             `json:"policy_artifact_id"`
	PolicyArtifactSHA256  string             `json:"policy_artifact_sha256"`
	RosterSHA256          string             `json:"roster_sha256"`
	HMMStateID            int                `json:"hmm_state_id"`
	HMMStatePath          []int              `json:"hmm_state_path"`
	HMMStateProbabilities []float64          `json:"hmm_state_probabilities"`
	ClassOrder            []string           `json:"class_order"`
	ClassProbabilities    map[string]float64 `json:"class_probabilities"`
	RankedFallback        []PreviewGroup     `json:"ranked_fallback"`
	SelectedGroup         string             `json:"selected_group,omitempty"`
	EligibleRosterIDs     []string           `json:"eligible_roster_ids"`
	ResolverCandidates    []Candidate        `json:"resolver_candidates"`
	ResolverExclusions    []Diagnostic       `json:"resolver_exclusions"`
}

// RosterSnapshot is the frozen per-cluster arm roster from the sidecar artifact.
// Used to seed the control plane's default arm-order UI.
type RosterSnapshot struct {
	Clusters     map[string][]string `json:"clusters"`
	RosterSHA256 string              `json:"roster_sha256"`
}

// RosterSource returns the sidecar's frozen per-cluster arm roster.
type RosterSource interface {
	ClusterRoster(ctx context.Context) (RosterSnapshot, error)
}

// Decider asks a policy sidecar to choose one supplied candidate.
type Decider interface {
	Decide(ctx context.Context, query Query) (Result, error)
}

// PreviewDecider asks a policy sidecar for a decision trace without serving.
type PreviewDecider interface {
	Preview(ctx context.Context, query Query) (PreviewResult, error)
}

// RoutePreviewer evaluates a fully formed router request without dispatch or
// serving lifecycle side effects.
type RoutePreviewer interface {
	PreviewRoute(ctx context.Context, req router.Request) (PreviewResult, error)
}

// OutcomeReporter sends final dispatch outcomes to a policy sidecar.
type OutcomeReporter interface {
	ReportOutcome(ctx context.Context, payload map[string]interface{}) error
}

// FeedbackReporter sends explicit user/session feedback to a policy sidecar.
type FeedbackReporter interface {
	ReportFeedback(ctx context.Context, payload map[string]interface{}) error
}
