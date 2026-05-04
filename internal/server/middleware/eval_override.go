package middleware

import (
	"context"
	"strings"

	"workweave/router/internal/observability"
	"workweave/router/internal/router/evalswitch"

	"github.com/gin-gonic/gin"
)

// EvalOverrideHeader forces the request through the heuristic fallback.
// Only honored for allow-listed installations.
const EvalOverrideHeader = "x-weave-disable-cluster"

// WithEvalRoutingOverride attaches an evalswitch.Decision to the request
// context when the installation is allow-listed and the header is "true".
func WithEvalRoutingOverride() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := strings.TrimSpace(c.GetHeader(EvalOverrideHeader))
		if !strings.EqualFold(raw, "true") {
			c.Next()
			return
		}
		installation := InstallationFrom(c)
		if installation == nil {
			c.Next()
			return
		}
		if !installation.IsEvalAllowlisted {
			observability.FromGin(c).Debug(
				"Ignored eval routing override from non-allow-listed installation",
				"installation_id", installation.ID,
			)
			c.Next()
			return
		}
		ctx := context.WithValue(c.Request.Context(), evalswitch.ContextKey{}, evalswitch.Decision{UseFallback: true})
		c.Request = c.Request.WithContext(ctx)
		observability.FromGin(c).Info(
			"Eval routing override applied; routing via fallback (heuristic)",
			"installation_id", installation.ID,
		)
		c.Next()
	}
}
