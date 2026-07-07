package proxy

import (
	"context"
	"time"
)

// InstallationIDContextKey is the request-context key for the authenticated installation UUID.
type InstallationIDContextKey struct{}

// TelemetryRepository persists per-request telemetry rows used by the UI dashboard.
type TelemetryRepository interface {
	InsertRequestTelemetry(ctx context.Context, p InsertTelemetryParams) error
	GetTelemetrySummary(ctx context.Context, installationID string, from, to time.Time) (TelemetrySummary, error)
	GetTelemetryTimeseries(ctx context.Context, installationID string, from, to time.Time, granularity string) ([]TelemetryBucket, error)
	GetTelemetrySummaryAll(ctx context.Context, from, to time.Time) (TelemetrySummary, error)
	GetTelemetryTimeseriesAll(ctx context.Context, from, to time.Time, granularity string) ([]TelemetryBucket, error)
	GetTelemetryRows(ctx context.Context, installationID string, from, to time.Time, limit int32) ([]TelemetryRow, error)
	GetTelemetryRowsAll(ctx context.Context, from, to time.Time, limit int32) ([]TelemetryRow, error)
}

// InsertTelemetryParams mirrors one router.upstream span row.
type InsertTelemetryParams struct {
	InstallationID string
	// APIKeyID attributes the row to the authenticating api key (per-key spend
	// audit). Empty leaves the column NULL.
	APIKeyID               string
	RequestID              string
	SpanType               string
	TraceID                string
	Timestamp              time.Time
	RequestedModel         string
	DecisionModel          string
	DecisionProvider       string
	DecisionReason         string
	EstimatedInputTokens   int32
	StickyHit              bool
	EmbedInput             string
	InputTokens            int32
	OutputTokens           int32
	RequestedInputCostUSD  float64
	RequestedOutputCostUSD float64
	ActualInputCostUSD     float64
	ActualOutputCostUSD    float64
	RouteLatencyMs         int64
	UpstreamLatencyMs      int64
	TotalLatencyMs         int64
	CrossFormat            bool
	UpstreamStatusCode     int32

	ClusterIDs           []int32
	CandidateModels      []string
	ChosenScore          *float64
	AlphaBreakdown       []byte // pre-marshaled JSON for W-1335; nil until then
	CandidateScores      []byte // pre-marshaled JSON model->score; nil for non-score routers
	Propensity           *float64
	ClusterRouterVersion string
	// Strategy names the routing model that produced this decision ("cluster",
	// "hmm", "rl", "bandit"). Always populated. Empty leaves the column NULL.
	Strategy string
	// RouteID is the opaque sidecar correlation id (HMM/RL) joining a decision
	// to its outcome report. Empty for the default cluster scorer → NULL column.
	RouteID             string
	TTFTMs              *int64
	CacheCreationTokens *int32
	CacheReadTokens     *int32
	DeviceID            string
	SessionID           string
	RouterUserID        string
	ClientApp           string
	TurnType            string
	// RolloutID joins eval/training-harness rollout rewards onto decisions
	// (x-weave-rollout-id header). Empty for normal traffic → NULL column.
	RolloutID string

	UpstreamFinishReason  *string
	StopReason            *string
	ToolUseBlocks         *int32
	InvalidToolArgsBlocks *int32
	FailoverUsed          *bool
	DegenerateShadow      *bool

	// SessionKey + Role are the offline join key to spiral_shadow_events and
	// session_pins (16-byte digest + roleForTier of the requested model). Nil /
	// empty leaves the columns NULL.
	SessionKey []byte
	Role       string

	// FreshDecisionModel + FreshCandidateScores capture the scorer's fresh
	// recommendation even on STAY turns (shadow-mode instrumentation for the
	// hysteresis downgrade lever). PinAgeSec supports min-dwell analysis. Empty
	// / nil leaves the columns NULL.
	FreshDecisionModel   string
	FreshCandidateScores []byte
	PinAgeSec            *int64

	// ToolResultBytes is the incoming tool-output size on a tool_result turn
	// (shadow-mode instrumentation for the tier-cap lever). nil when the turn
	// carries no trailing tool_result.
	ToolResultBytes *int32

	// CredentialKeyPrefix/CredentialKeySuffix are the safe display parts of the
	// upstream credential that served the turn; CredentialSource names the
	// precedence branch it came from (subscription / codex_subscription / byok /
	// client). Empty on deployment-key turns, leaving the columns NULL. Equal
	// prefix/suffix values across distinct RouterUserIDs reveal one subscription
	// paying for many seats.
	CredentialKeyPrefix string
	CredentialKeySuffix string
	CredentialSource    string
}

// TelemetrySummary holds aggregated totals for the dashboard cards.
type TelemetrySummary struct {
	RequestCount          int64
	TotalTokens           int64
	TotalRequestedCostUSD float64
	TotalActualCostUSD    float64
	TotalSavingsUSD       float64
}

// TelemetryBucket is one time-bucket entry for the cost savings chart.
type TelemetryBucket struct {
	Bucket           time.Time
	RequestedCostUSD float64
	ActualCostUSD    float64
}

// TelemetryRow is one upstream span returned by the drill-down endpoint.
type TelemetryRow struct {
	Timestamp           time.Time
	RequestID           string
	RequestedModel      string
	DecisionModel       string
	DecisionProvider    string
	DecisionReason      string
	StickyHit           bool
	InputTokens         int32
	OutputTokens        int32
	CacheCreationTokens *int32
	CacheReadTokens     *int32
	RequestedCostUSD    float64
	ActualCostUSD       float64
	TotalLatencyMs      int64
	UpstreamStatusCode  int32
	RouterUserID        string
	ClientApp           string
	TurnType            string
	UserEmail           string
}
