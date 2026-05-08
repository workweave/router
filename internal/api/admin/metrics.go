package admin

import (
	"net/http"
	"strconv"
	"time"

	"workweave/router/internal/proxy"
	"workweave/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
)

type metricsSummaryResponse struct {
	RequestCount          int64   `json:"request_count"`
	TotalTokens           int64   `json:"total_tokens"`
	TotalRequestedCostUSD float64 `json:"total_requested_cost_usd"`
	TotalActualCostUSD    float64 `json:"total_actual_cost_usd"`
	TotalSavingsUSD       float64 `json:"total_savings_usd"`
}

type timeseriesBucket struct {
	Bucket           string  `json:"bucket"`
	RequestedCostUSD float64 `json:"requested_cost_usd"`
	ActualCostUSD    float64 `json:"actual_cost_usd"`
}

type metricsTimeseriesResponse struct {
	Buckets []timeseriesBucket `json:"buckets"`
}

// MetricsSummaryHandler returns aggregated cost/token totals. Admin-cookie
// sessions see totals across every installation; rk_-keyed callers see
// totals for their own installation only.
func MetricsSummaryHandler(proxySvc *proxy.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		from, to := parseTimeWindow(c)

		var (
			summary proxy.TelemetrySummary
			err     error
		)
		if admin := middleware.AdminPrincipalFrom(c); admin != nil {
			summary, err = proxySvc.MetricsSummaryAll(c.Request.Context(), from, to)
		} else {
			installation := middleware.InstallationFrom(c)
			if installation == nil {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid_key"})
				return
			}
			summary, err = proxySvc.MetricsSummary(c.Request.Context(), installation.ID, from, to)
		}
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch metrics"})
			return
		}

		c.JSON(http.StatusOK, metricsSummaryResponse{
			RequestCount:          summary.RequestCount,
			TotalTokens:           summary.TotalTokens,
			TotalRequestedCostUSD: summary.TotalRequestedCostUSD,
			TotalActualCostUSD:    summary.TotalActualCostUSD,
			TotalSavingsUSD:       summary.TotalSavingsUSD,
		})
	}
}

// MetricsTimeseriesHandler returns bucketed cost data for the cost savings
// chart. Admin-cookie sessions aggregate across every installation; rk_-keyed
// callers see only their own installation.
func MetricsTimeseriesHandler(proxySvc *proxy.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		granularity := c.DefaultQuery("granularity", "hour")
		if granularity != "hour" && granularity != "day" && granularity != "week" {
			granularity = "hour"
		}
		from, to := parseTimeWindow(c)

		var (
			buckets []proxy.TelemetryBucket
			err     error
		)
		if admin := middleware.AdminPrincipalFrom(c); admin != nil {
			buckets, err = proxySvc.MetricsTimeseriesAll(c.Request.Context(), from, to, granularity)
		} else {
			installation := middleware.InstallationFrom(c)
			if installation == nil {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid_key"})
				return
			}
			buckets, err = proxySvc.MetricsTimeseries(c.Request.Context(), installation.ID, from, to, granularity)
		}
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch timeseries"})
			return
		}

		out := make([]timeseriesBucket, 0, len(buckets))
		for _, b := range buckets {
			out = append(out, timeseriesBucket{
				Bucket:           b.Bucket.UTC().Format(time.RFC3339),
				RequestedCostUSD: b.RequestedCostUSD,
				ActualCostUSD:    b.ActualCostUSD,
			})
		}
		c.JSON(http.StatusOK, metricsTimeseriesResponse{Buckets: out})
	}
}

type metricsDetailRow struct {
	Timestamp          string  `json:"timestamp"`
	RequestID          string  `json:"request_id"`
	RequestedModel     string  `json:"requested_model"`
	DecisionModel      string  `json:"decision_model"`
	DecisionProvider   string  `json:"decision_provider"`
	DecisionReason     string  `json:"decision_reason"`
	StickyHit          bool    `json:"sticky_hit"`
	InputTokens        int32   `json:"input_tokens"`
	OutputTokens       int32   `json:"output_tokens"`
	RequestedCostUSD   float64 `json:"requested_cost_usd"`
	ActualCostUSD      float64 `json:"actual_cost_usd"`
	TotalLatencyMs     int64   `json:"total_latency_ms"`
	UpstreamStatusCode int32   `json:"upstream_status_code"`
}

type metricsDetailsResponse struct {
	Rows []metricsDetailRow `json:"rows"`
}

// MetricsDetailsHandler returns individual telemetry rows for a time window.
// Used by the dashboard drill-down modal to show the underlying requests
// behind a chart bucket. Admin sessions span every installation; rk_-keyed
// callers see only their own.
func MetricsDetailsHandler(proxySvc *proxy.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		from, to := parseTimeWindow(c)
		const defaultLimit = 100
		const maxLimit = 1000
		limit := int32(defaultLimit)
		if raw := c.Query("limit"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 {
				if n > maxLimit {
					n = maxLimit
				}
				limit = int32(n)
			}
		}

		var (
			rows []proxy.TelemetryRow
			err  error
		)
		if admin := middleware.AdminPrincipalFrom(c); admin != nil {
			rows, err = proxySvc.MetricsRowsAll(c.Request.Context(), from, to, limit)
		} else {
			installation := middleware.InstallationFrom(c)
			if installation == nil {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid_key"})
				return
			}
			rows, err = proxySvc.MetricsRows(c.Request.Context(), installation.ID, from, to, limit)
		}
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch details"})
			return
		}

		out := make([]metricsDetailRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, metricsDetailRow{
				Timestamp:          r.Timestamp.UTC().Format(time.RFC3339Nano),
				RequestID:          r.RequestID,
				RequestedModel:     r.RequestedModel,
				DecisionModel:      r.DecisionModel,
				DecisionProvider:   r.DecisionProvider,
				DecisionReason:     r.DecisionReason,
				StickyHit:          r.StickyHit,
				InputTokens:        r.InputTokens,
				OutputTokens:       r.OutputTokens,
				RequestedCostUSD:   r.RequestedCostUSD,
				ActualCostUSD:      r.ActualCostUSD,
				TotalLatencyMs:     r.TotalLatencyMs,
				UpstreamStatusCode: r.UpstreamStatusCode,
			})
		}
		c.JSON(http.StatusOK, metricsDetailsResponse{Rows: out})
	}
}

// parseTimeWindow reads optional ?from= and ?to= RFC3339 query params.
// Defaults to the last 7 days when absent or unparseable.
func parseTimeWindow(c *gin.Context) (from, to time.Time) {
	to = time.Now().UTC()
	from = to.AddDate(0, 0, -7)

	if raw := c.Query("from"); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			from = t.UTC()
		}
	}
	if raw := c.Query("to"); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			to = t.UTC()
		}
	}
	return from, to
}
