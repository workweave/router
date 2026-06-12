package middleware

import (
	"math"
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

// abortInvalidKnob writes a 400 with the error envelope matching the route's
// API format. Without this, every route returned the OpenAI shape, breaking
// Anthropic and Gemini clients that parse their native envelopes.
func abortInvalidKnob(c *gin.Context, message string) {
	switch detectAPIFormat(c.Request.URL.Path) {
	case apiFormatAnthropic:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"type": "error",
			"error": gin.H{
				"type":    "invalid_request_error",
				"message": message,
			},
		})
	case apiFormatGemini:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"code":    http.StatusBadRequest,
				"message": message,
				"status":  "INVALID_ARGUMENT",
			},
		})
	default:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"type":    "invalid_request_error",
				"message": message,
				"param":   nil,
				"code":    nil,
			},
		})
	}
}

type apiFormat int

const (
	apiFormatOpenAI apiFormat = iota
	apiFormatAnthropic
	apiFormatGemini
)

// detectAPIFormat picks the envelope shape based on the request path. Mirrors
// the routes mounted in internal/server/server.go.
func detectAPIFormat(path string) apiFormat {
	switch {
	case strings.HasPrefix(path, "/v1beta/"):
		return apiFormatGemini
	case strings.HasPrefix(path, "/v1/messages"), strings.HasPrefix(path, "/v1/route"):
		return apiFormatAnthropic
	default:
		return apiFormatOpenAI
	}
}

// WithRoutingKnobsOverride parses the x-weave-routing-* headers and stashes them on the request context.
func WithRoutingKnobsOverride() gin.HandlerFunc {
	return func(c *gin.Context) {
		log := observability.FromGin(c)
		var overrides router.Overrides
		hasOverrides := false

		if raw := strings.TrimSpace(c.GetHeader(HeaderAlpha)); raw != "" {
			val, err := strconv.ParseFloat(raw, 64)
			if err != nil || math.IsNaN(val) || math.IsInf(val, 0) || val < 0 || val > 1 {
				abortInvalidKnob(c, HeaderAlpha+" must be a finite float between 0 and 1.")
				return
			}
			overrides.Alpha = &val
			hasOverrides = true
		}

		if raw := strings.TrimSpace(c.GetHeader(HeaderSpeedWeight)); raw != "" {
			val, err := strconv.ParseFloat(raw, 64)
			if err != nil || math.IsNaN(val) || math.IsInf(val, 0) || val < 0 || val > 1 {
				abortInvalidKnob(c, HeaderSpeedWeight+" must be a finite float between 0 and 1.")
				return
			}
			overrides.SpeedWeight = &val
			hasOverrides = true
		}

		if raw := strings.TrimSpace(c.GetHeader(HeaderOutputCostRatio)); raw != "" {
			val, err := strconv.ParseFloat(raw, 64)
			if err != nil || math.IsNaN(val) || math.IsInf(val, 0) || val < 0 || val > 10 {
				abortInvalidKnob(c, HeaderOutputCostRatio+" must be a finite float between 0 and 10.")
				return
			}
			overrides.OutputCostRatio = &val
			hasOverrides = true
		}

		if raw := strings.TrimSpace(c.GetHeader(HeaderExpectedOutputTokens)); raw != "" {
			val, err := strconv.Atoi(raw)
			if err != nil || val < 0 || val > 100000 {
				abortInvalidKnob(c, HeaderExpectedOutputTokens+" must be a valid integer between 0 and 100000.")
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
				abortInvalidKnob(c, HeaderPerModelVerbosity+" must be either 'true' or 'false'.")
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
