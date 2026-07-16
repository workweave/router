// force_effort_override.go — parse x-weave-effort request header into a
// router.Overrides.ForceEffort value the proxy forwards into EmitOptions.
//
// Header shape mirrors the sibling knobs in routing_knobs_override.go:
// value is one of "low", "medium", "high", "max", "xhigh" (canonical) or the
// alias forms "fast", "minimal", "ultra" (canonicalizeEffort maps them). An
// unparseable value = 400 with the format-aware envelope (abortInvalidKnob).
//
// Mirrors the experience the user gets from the verbose `/force-effort <lvl>`
// slash command, but rides on every request the eval harness sends so a
// pin-and-effort bake-off doesn't need to share session state — same shape
// x-weave-force-model got for the model pin (force_model.go:189).

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
		overrides := router.Overrides{ForceEffort: canonical}
		ctx := router.WithRoutingKnobs(c.Request.Context(), &overrides)
		c.Request = c.Request.WithContext(ctx)
		log := observability.FromGin(c)
		log.Debug(
			"Force-effort override applied",
			"override_force_effort", canonical,
		)
		c.Next()
	}
}
