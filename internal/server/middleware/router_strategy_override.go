package middleware

import (
	"fmt"
	"net/http"
	"strings"

	"workweave/router/internal/observability"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"

	"github.com/gin-gonic/gin"
)

// RouterStrategyOverrideHeader selects the routing strategy for a request.
// Accepted values are "cluster" (the default scorer), "rl" (the trained
// RL/DPO policy router), and "bandit" (Thompson sampling over a frozen
// posterior). Absent or unrecognized values fall through to the deployment
// default.
const RouterStrategyOverrideHeader = "x-weave-router-strategy"

// WithRouterStrategyOverride applies the persisted installation strategy and permits
// an eval header override when authorized; available is injected by the proxy registry.
func WithRouterStrategyOverride(available ...router.Strategy) gin.HandlerFunc {
	return WithRouterStrategyDefault(router.StrategyCluster, available...)
}

// WithRouterStrategyDefault applies a deployment-level default for installations
// with no explicit override, enabling allowlist-first then one-flag global rollout.
func WithRouterStrategyDefault(defaultStrategy router.Strategy, available ...router.Strategy) gin.HandlerFunc {
	allowed := make(map[router.Strategy]struct{}, len(available)+1)
	allowed[router.StrategyCluster] = struct{}{}
	if len(available) == 0 {
		available = []router.Strategy{router.StrategyRL, router.StrategyHMM, router.StrategyBandit}
	}
	for _, strategy := range available {
		allowed[strategy] = struct{}{}
	}
	defaultStrategy = normalizeRouterStrategyDefault(defaultStrategy, allowed)
	return func(c *gin.Context) {
		installation := InstallationFrom(c)
		if installation == nil {
			c.Next()
			return
		}

		strategy := router.Strategy(strings.ToLower(strings.TrimSpace(string(installation.RoutingStrategy))))
		if strategy == "" {
			strategy = defaultStrategy
		}

		raw := strings.ToLower(strings.TrimSpace(c.GetHeader(RouterStrategyOverrideHeader)))
		if raw != "" {
			requested := router.Strategy(raw)
			switch {
			case !installation.PolicyHeaderOverridesEnabled:
				observability.FromGin(c).Warn("Router-strategy override ignored: installation is not authorized for policy headers", "installation_id", installation.ID)
			case !strategyAllowed(requested, allowed):
				observability.FromGin(c).Warn("Router-strategy override ignored: strategy is not registered", "installation_id", installation.ID, "requested_strategy", raw)
			default:
				strategy = requested
				observability.FromGin(c).Info("Router-strategy override applied", "installation_id", installation.ID, "requested_strategy", raw)
			}
		}

		if _, ok := allowed[strategy]; !ok {
			log := observability.FromGin(c).With(
				"installation_id", installation.ID,
				"persisted_strategy", strategy,
			)
			err := fmt.Errorf("strategy %q requested but no router configured: %w", strategy, router.ErrStrategyUnavailable)
			cls, ok := proxy.ClassifyDispatchError(err)
			if !ok {
				c.AbortWithStatus(http.StatusServiceUnavailable)
				return
			}
			proxy.LogDispatchErrorClass(log, cls, err)
			if cls.RetryAfter {
				c.Header("Retry-After", "1")
			}
			abortStrategyUnavailable(c, cls.Status, cls.Message)
			return
		}

		ctx := router.WithStrategy(c.Request.Context(), strategy)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// NormalizeRouterStrategyDefault clamps an unregistered deployment default to cluster.
func NormalizeRouterStrategyDefault(defaultStrategy router.Strategy, available ...router.Strategy) router.Strategy {
	allowed := make(map[router.Strategy]struct{}, len(available)+1)
	allowed[router.StrategyCluster] = struct{}{}
	for _, strategy := range available {
		allowed[strategy] = struct{}{}
	}
	return normalizeRouterStrategyDefault(defaultStrategy, allowed)
}

func normalizeRouterStrategyDefault(defaultStrategy router.Strategy, allowed map[router.Strategy]struct{}) router.Strategy {
	if !strategyAllowed(defaultStrategy, allowed) {
		return router.StrategyCluster
	}
	return defaultStrategy
}

func strategyAllowed(strategy router.Strategy, allowed map[router.Strategy]struct{}) bool {
	if strategy == "" {
		return false
	}
	_, ok := allowed[strategy]
	return ok
}

// abortStrategyUnavailable writes a 503 with the error envelope matching the
// route's API format — same detectAPIFormat branching as abortInvalidKnob.
func abortStrategyUnavailable(c *gin.Context, status int, message string) {
	switch detectAPIFormat(c.Request.URL.Path) {
	case apiFormatAnthropic:
		c.AbortWithStatusJSON(status, gin.H{
			"type": "error",
			"error": gin.H{
				"type":    "api_error",
				"message": message,
			},
		})
	case apiFormatGemini:
		c.AbortWithStatusJSON(status, gin.H{
			"error": gin.H{
				"code":    status,
				"message": message,
				"status":  "UNAVAILABLE",
			},
		})
	default:
		c.AbortWithStatusJSON(status, gin.H{
			"error": gin.H{
				"type":    "api_error",
				"message": message,
				"param":   nil,
				"code":    nil,
			},
		})
	}
}
