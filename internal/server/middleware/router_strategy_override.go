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
// posterior). Absent or unrecognized values fall through to the resolved
// API key's default_strategy (see auth.APIKey.DefaultStrategy), and from
// there to the deployment default.
const RouterStrategyOverrideHeader = "x-weave-router-strategy"

// WithRouterStrategyOverride stashes the request's routing strategy on the
// request context. The header, when present and recognized, always wins —
// this is the only path clients like Claude Code or an eval harness use.
// Absent or unrecognized header values fall back to the authed API key's
// default_strategy, which exists for clients that can't send custom headers
// (e.g. Cursor's Override Base URL). Like the cluster-version override this
// gates on a resolved installation so anonymous traffic can't flip
// strategies, and it never silently picks a model — the rl and hmm
// strategies fail closed (HTTP 503) when no policy sidecar is wired.
func WithRouterStrategyOverride() gin.HandlerFunc {
	return func(c *gin.Context) {
		installation := InstallationFrom(c)
		if installation == nil {
			c.Next()
			return
		}

		raw := strings.ToLower(strings.TrimSpace(c.GetHeader(RouterStrategyOverrideHeader)))
		if raw != "" {
			if strategy, ok := parseRouterStrategy(raw); ok {
				applyRouterStrategy(c, installation.ID, strategy, "header")
				c.Next()
				return
			}
			observability.FromGin(c).Warn(
				"Router-strategy override header ignored: unrecognized value",
				"installation_id", installation.ID,
				"requested_strategy", raw,
			)
		}

		if apiKey := APIKeyFrom(c); apiKey != nil && apiKey.DefaultStrategy != "" {
			if strategy, ok := parseRouterStrategy(apiKey.DefaultStrategy); ok {
				applyRouterStrategy(c, installation.ID, strategy, "api_key_default")
			} else {
				observability.FromGin(c).Warn(
					"API key default_strategy ignored: unrecognized value",
					"installation_id", installation.ID,
					"api_key_id", apiKey.ID,
					"default_strategy", apiKey.DefaultStrategy,
				)
			}
		}
		c.Next()
	}
}

// parseRouterStrategy reports whether raw is a recognized router.Strategy value.
func parseRouterStrategy(raw string) (router.Strategy, bool) {
	strategy := router.Strategy(raw)
	switch strategy {
	case router.StrategyCluster, router.StrategyRL, router.StrategyHMM, router.StrategyBandit:
		return strategy, true
	default:
		return "", false
	}
}

// applyRouterStrategy stashes strategy on the request context and logs which
// source (header vs. api_key_default) supplied it.
func applyRouterStrategy(c *gin.Context, installationID string, strategy router.Strategy, source string) {
	ctx := router.WithStrategy(c.Request.Context(), strategy)
	c.Request = c.Request.WithContext(ctx)
	observability.FromGin(c).Info(
		"Router-strategy override applied",
		"installation_id", installationID,
		"strategy", string(strategy),
		"source", source,
	)
}
