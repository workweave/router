package policy

import (
	"context"

	"workweave/router/internal/router"
)

// SchemaVersionV1 is the first stable policy-sidecar wire contract.
const SchemaVersionV1 = "policy_router_v1"

const (
	ExecutionModeServing = "serving"
	ExecutionModeShadow  = "shadow"
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
}

// Decider asks a policy sidecar to choose one supplied candidate.
type Decider interface {
	Decide(ctx context.Context, query Query) (Result, error)
}

// OutcomeReporter sends final dispatch outcomes to a policy sidecar.
type OutcomeReporter interface {
	ReportOutcome(ctx context.Context, payload map[string]interface{}) error
}

// FeedbackReporter sends explicit user/session feedback to a policy sidecar.
type FeedbackReporter interface {
	ReportFeedback(ctx context.Context, payload map[string]interface{}) error
}
