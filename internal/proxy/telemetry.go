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
	GetLatencyPercentilesAll(ctx context.Context, from, to time.Time, granularity string, model, provider *string) ([]LatencyPercentiles, error)
	GetModelPerformanceAll(ctx context.Context, from, to time.Time) ([]ModelPerformance, error)
}

// InsertTelemetryParams mirrors one router.upstream span row.
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

	ClusterIDs           []int32
	CandidateModels      []string
	ChosenScore          *float64
	AlphaBreakdown       []byte // pre-marshaled JSON for W-1335; nil until then
	ClusterRouterVersion string
	TTFTMs               *int64
	CacheCreationTokens  *int32
	CacheReadTokens      *int32
	DeviceID             string
	SessionID            string
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

// LatencyPercentiles is one time-bucket of cross-org latency aggregation.
// TTFT fields are 0 when no rows in the bucket had a ttft_ms value.
type LatencyPercentiles struct {
	Bucket        time.Time
	RequestCount  int64
	TotalP50Ms    int64
	TotalP90Ms    int64
	TotalP99Ms    int64
	RouteP50Ms    int64
	RouteP90Ms    int64
	UpstreamP50Ms int64
	UpstreamP90Ms int64
	TTFTP50Ms     int64
	TTFTP90Ms     int64
}

// ModelPerformance is one row in the per-model performance comparison.
type ModelPerformance struct {
	DecisionModel      string
	DecisionProvider   string
	RequestCount       int64
	TotalP50Ms         int64
	TotalP90Ms         int64
	ErrorCount         int64
	TotalActualCostUSD float64
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
}
