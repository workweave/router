package admin

import (
	"net/http"
	"time"

	"workweave/router/internal/proxy"

	"github.com/gin-gonic/gin"
)

type latencyPercentilesBucket struct {
	Bucket        string `json:"bucket"`
	RequestCount  int64  `json:"request_count"`
	TotalP50Ms    int64  `json:"total_p50_ms"`
	TotalP90Ms    int64  `json:"total_p90_ms"`
	TotalP99Ms    int64  `json:"total_p99_ms"`
	RouteP50Ms    int64  `json:"route_p50_ms"`
	RouteP90Ms    int64  `json:"route_p90_ms"`
	UpstreamP50Ms int64  `json:"upstream_p50_ms"`
	UpstreamP90Ms int64  `json:"upstream_p90_ms"`
	TTFTP50Ms     int64 `json:"ttft_p50_ms"`
	TTFTP90Ms     int64 `json:"ttft_p90_ms"`
}

type latencyPercentilesResponse struct {
	Buckets []latencyPercentilesBucket `json:"buckets"`
}

// InternalLatencyPercentilesHandler returns time-bucketed latency percentiles across all installations.
func InternalLatencyPercentilesHandler(proxySvc *proxy.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		granularity := c.DefaultQuery("granularity", "hour")
		if granularity != "hour" && granularity != "day" && granularity != "week" {
			granularity = "hour"
		}
		from, to := parseTimeWindow(c)

		var model, provider *string
		if v := c.Query("model"); v != "" {
			model = &v
		}
		if v := c.Query("provider"); v != "" {
			provider = &v
		}

		buckets, err := proxySvc.MetricsLatencyPercentilesAll(c.Request.Context(), from, to, granularity, model, provider)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch latency percentiles"})
			return
		}

		out := make([]latencyPercentilesBucket, 0, len(buckets))
		for _, b := range buckets {
			out = append(out, latencyPercentilesBucket{
				Bucket:        b.Bucket.UTC().Format(time.RFC3339),
				RequestCount:  b.RequestCount,
				TotalP50Ms:    b.TotalP50Ms,
				TotalP90Ms:    b.TotalP90Ms,
				TotalP99Ms:    b.TotalP99Ms,
				RouteP50Ms:    b.RouteP50Ms,
				RouteP90Ms:    b.RouteP90Ms,
				UpstreamP50Ms: b.UpstreamP50Ms,
				UpstreamP90Ms: b.UpstreamP90Ms,
				TTFTP50Ms:     b.TTFTP50Ms,
				TTFTP90Ms:     b.TTFTP90Ms,
			})
		}
		c.JSON(http.StatusOK, latencyPercentilesResponse{Buckets: out})
	}
}

type modelPerformanceRow struct {
	DecisionModel    string  `json:"decision_model"`
	DecisionProvider string  `json:"decision_provider"`
	RequestCount     int64   `json:"request_count"`
	TotalP50Ms       int64   `json:"total_p50_ms"`
	TotalP90Ms       int64   `json:"total_p90_ms"`
	ErrorRate        float64 `json:"error_rate"`
	CostPerRequest   float64 `json:"cost_per_request_usd"`
	TrafficSharePct  float64 `json:"traffic_share_pct"`
}

type modelPerformanceResponse struct {
	Models []modelPerformanceRow `json:"models"`
}

// InternalModelPerformanceHandler returns per-model performance metrics across all installations.
func InternalModelPerformanceHandler(proxySvc *proxy.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		from, to := parseTimeWindow(c)

		rows, err := proxySvc.MetricsModelPerformanceAll(c.Request.Context(), from, to)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch model performance"})
			return
		}

		var totalRequests int64
		for _, r := range rows {
			totalRequests += r.RequestCount
		}

		out := make([]modelPerformanceRow, 0, len(rows))
		for _, r := range rows {
			var errorRate float64
			if r.RequestCount > 0 {
				errorRate = float64(r.ErrorCount) / float64(r.RequestCount)
			}
			var costPerRequest float64
			if r.RequestCount > 0 {
				costPerRequest = r.TotalActualCostUSD / float64(r.RequestCount)
			}
			var trafficShare float64
			if totalRequests > 0 {
				trafficShare = float64(r.RequestCount) / float64(totalRequests) * 100.0
			}
			out = append(out, modelPerformanceRow{
				DecisionModel:    r.DecisionModel,
				DecisionProvider: r.DecisionProvider,
				RequestCount:     r.RequestCount,
				TotalP50Ms:       r.TotalP50Ms,
				TotalP90Ms:       r.TotalP90Ms,
				ErrorRate:        errorRate,
				CostPerRequest:   costPerRequest,
				TrafficSharePct:  trafficShare,
			})
		}
		c.JSON(http.StatusOK, modelPerformanceResponse{Models: out})
	}
}
