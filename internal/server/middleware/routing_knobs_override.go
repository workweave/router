package middleware

import (
	"net/http"
	"strconv"
	"strings"

	"workweave/router/internal/observability"
	"workweave/router/internal/router"

	"github.com/gin-gonic/gin"
)

const (
	HeaderAlpha                = "x-weave-routing-alpha"
	HeaderSpeedWeight          = "x-weave-routing-speed-weight"
	HeaderOutputCostRatio      = "x-weave-routing-output-cost-ratio"
	HeaderExpectedOutputTokens = "x-weave-routing-expected-output-tokens"
	HeaderPerModelVerbosity    = "x-weave-routing-per-model-verbosity"
)

// WithRoutingKnobsOverride parses the x-weave-routing-* headers and stashes them on the request context.
func WithRoutingKnobsOverride() gin.HandlerFunc {
	return func(c *gin.Context) {
		log := observability.FromGin(c)
		var overrides router.Overrides
		hasOverrides := false

		if raw := strings.TrimSpace(c.GetHeader(HeaderAlpha)); raw != "" {
			val, err := strconv.ParseFloat(raw, 64)
			if err != nil || val < 0 || val > 1 {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
					"error": gin.H{
						"type":    "invalid_request_error",
						"message": HeaderAlpha + " must be a valid float between 0 and 1",
					},
				})
				return
			}
			overrides.Alpha = &val
			hasOverrides = true
		}

		if raw := strings.TrimSpace(c.GetHeader(HeaderSpeedWeight)); raw != "" {
			val, err := strconv.ParseFloat(raw, 64)
			if err != nil || val < 0 || val > 1 {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
					"error": gin.H{
						"type":    "invalid_request_error",
						"message": HeaderSpeedWeight + " must be a valid float between 0 and 1",
					},
				})
				return
			}
			overrides.SpeedWeight = &val
			hasOverrides = true
		}

		if raw := strings.TrimSpace(c.GetHeader(HeaderOutputCostRatio)); raw != "" {
			val, err := strconv.ParseFloat(raw, 64)
			if err != nil || val < 0 || val > 10 {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
					"error": gin.H{
						"type":    "invalid_request_error",
						"message": HeaderOutputCostRatio + " must be a valid float between 0 and 10",
					},
				})
				return
			}
			overrides.OutputCostRatio = &val
			hasOverrides = true
		}

		if raw := strings.TrimSpace(c.GetHeader(HeaderExpectedOutputTokens)); raw != "" {
			val, err := strconv.Atoi(raw)
			if err != nil || val < 0 || val > 100000 {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
					"error": gin.H{
						"type":    "invalid_request_error",
						"message": HeaderExpectedOutputTokens + " must be a valid integer between 0 and 100000",
					},
				})
				return
			}
			overrides.ExpectedOutputTokens = &val
			hasOverrides = true
		}

		if raw := strings.TrimSpace(c.GetHeader(HeaderPerModelVerbosity)); raw != "" {
			var val bool
			if raw == "true" {
				val = true
			} else if raw == "false" {
				val = false
			} else {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
					"error": gin.H{
						"type":    "invalid_request_error",
						"message": HeaderPerModelVerbosity + " must be either 'true' or 'false'",
					},
				})
				return
			}
			overrides.PerModelVerbosity = &val
			hasOverrides = true
		}

		if hasOverrides {
			ctx := router.WithRoutingKnobs(c.Request.Context(), &overrides)
			c.Request = c.Request.WithContext(ctx)
			log.Debug(
				"Routing knobs override applied",
				"override_alpha", overrides.Alpha,
				"override_speed_weight", overrides.SpeedWeight,
				"override_output_cost_ratio", overrides.OutputCostRatio,
				"override_expected_output_tokens", overrides.ExpectedOutputTokens,
				"override_per_model_verbosity", overrides.PerModelVerbosity,
			)
		}
		c.Next()
	}
}
