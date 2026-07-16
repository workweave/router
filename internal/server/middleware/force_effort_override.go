// force_effort_override.go — WithForceEffortOverride middleware for x-weave-effort.
// Accepts canonical levels (low/medium/high/max/xhigh) and aliases (fast/minimal/ultra);
// invalid values → 400 via abortInvalidKnob.

package middleware

import (
	"strings"

	"workweave/router/internal/observability"
	"workweave/router/internal/router"
	"workweave/router/internal/translate"

	"github.com/gin-gonic/gin"
)

// ForceEffortOverrideHeader is the request-header key. Lower-case is
// conventional per the x-weave-* family; Envoy / most proxies case-fold
// headers, so casing doesn't matter at the wire.
const ForceEffortOverrideHeader = "x-weave-effort"

// WithForceEffortOverride parses the x-weave-effort request header and
// stashes it on the request context as router.Overrides.ForceEffort. Probe
// validators (translate.IsValidEffort) gate the value before storing so a
// typo returns 400 with a format-aware envelope instead of routing garbage
// to a provider that 400s on an unknown effort level.
func WithForceEffortOverride() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := strings.TrimSpace(c.GetHeader(ForceEffortOverrideHeader))
		if raw == "" {
			c.Next()
			return
		}
		if !translate.IsValidEffort(raw) {
			abortInvalidKnob(c, ForceEffortOverrideHeader+" must be one of: low, medium, high, max, xhigh (or aliases fast/minimal/ultra).")
			return
		}
		canonical := translate.CanonicalizeEffort(raw)
		// Merge with any existing routing knobs (e.g. from WithRoutingKnobsOverride)
		// so ForceEffort doesn't silently drop a separately-configured Alpha/QualityBias.
		merged := router.Overrides{ForceEffort: canonical}
		if existing := router.RoutingKnobsFromContext(c.Request.Context()); existing != nil {
			merged.Alpha = existing.Alpha
			merged.QualityBias = existing.QualityBias
			merged.SpeedWeight = existing.SpeedWeight
			merged.OutputCostRatio = existing.OutputCostRatio
			merged.ExpectedOutputTokens = existing.ExpectedOutputTokens
			merged.PerModelVerbosity = existing.PerModelVerbosity
		}
		ctx := router.WithRoutingKnobs(c.Request.Context(), &merged)
		c.Request = c.Request.WithContext(ctx)
		log := observability.FromGin(c)
		log.Debug(
			"Force-effort override applied",
			"override_force_effort", canonical,
		)
		c.Next()
	}
}
