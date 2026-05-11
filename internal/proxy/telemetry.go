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

// InsertTelemetryParams mirrors one router.upstream span row. The
// routing-brain fields after UpstreamStatusCode are nullable — the
// pinned-route and heuristic paths leave them as their zero values
// and the adapter translates that to NULL columns. Schema is additive,
// dashboard read queries are unaffected.
type InsertTelemetryParams struct {
	InstallationID         string
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

	// Routing observability fields. Populated for cluster-routed
	// requests; left nil/empty for pinned routes and heuristic
	// fallbacks. Adapter maps zero values to NULL columns.
	ClusterIDs           []int32  // top-p clusters; nil for non-cluster decisions
	CandidateModels      []string // eligible-model set argmax ran over; nil otherwise
	ChosenScore          *float64 // argmax score; nil for non-cluster decisions
	AlphaBreakdown       []byte   // pre-marshaled JSON for W-1335; nil until then
	ClusterRouterVersion string   // artifact version; empty for non-cluster decisions
	TTFTMs               *int64   // upstream first-byte ms; nil when unmeasured
	CacheCreationTokens  *int32   // W-1309 forward-compat; nil until populated
	CacheReadTokens      *int32   // W-1309 forward-compat; nil until populated
	DeviceID             string   // from client identity; empty if absent
	SessionID            string   // from client identity; empty if absent
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

// TelemetryRow is one individual upstream span returned by the drill-down
// endpoint to show what happened inside a chart bucket.
type TelemetryRow struct {
	Timestamp          time.Time
	RequestID          string
	RequestedModel     string
	DecisionModel      string
	DecisionProvider   string
	DecisionReason     string
	StickyHit          bool
	InputTokens        int32
	OutputTokens       int32
	RequestedCostUSD   float64
	ActualCostUSD      float64
	TotalLatencyMs     int64
	UpstreamStatusCode int32
}
