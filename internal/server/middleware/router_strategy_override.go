package middleware

import (
	"strings"

	"workweave/router/internal/observability"
	"workweave/router/internal/router"

	"github.com/gin-gonic/gin"
)

// RouterStrategyOverrideHeader selects the routing strategy for a request.
// Accepted values are "cluster" (the default scorer), "rl" (the trained
// RL/DPO policy router), and "bandit" (Thompson sampling over a frozen
// posterior). Absent or unrecognized values fall through to the deployment
// default.
const RouterStrategyOverrideHeader = "x-weave-router-strategy"

// WithRouterStrategyOverride stashes the requested routing strategy on the
// request context when the header is set to a recognized value. Like the
// cluster-version override it gates on a resolved installation so anonymous
// traffic can't flip strategies, and it never silently picks a model — the rl
// and hmm strategies fail closed (HTTP 503) when no policy sidecar is wired.
func WithRouterStrategyOverride() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := strings.ToLower(strings.TrimSpace(c.GetHeader(RouterStrategyOverrideHeader)))
		if raw == "" {
			c.Next()
			return
		}
		installation := InstallationFrom(c)
		if installation == nil {
			c.Next()
			return
		}
		strategy := router.Strategy(raw)
		if strategy != router.StrategyCluster && strategy != router.StrategyRL && strategy != router.StrategyHMM && strategy != router.StrategyBandit {
			observability.FromGin(c).Warn(
				"Router-strategy override ignored: unrecognized value",
				"installation_id", installation.ID,
				"requested_strategy", raw,
			)
			c.Next()
			return
		}
		ctx := router.WithStrategy(c.Request.Context(), strategy)
		c.Request = c.Request.WithContext(ctx)
		observability.FromGin(c).Info(
			"Router-strategy override applied",
			"installation_id", installation.ID,
			"requested_strategy", raw,
		)
		c.Next()
	}
}
