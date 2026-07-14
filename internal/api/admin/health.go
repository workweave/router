package admin

import (
	"context"
	"net/http"

	"workweave/router/internal/observability"

	"github.com/gin-gonic/gin"
)

// HealthChecker reports whether a dependency is ready to serve traffic.
type HealthChecker interface {
	CheckHealth(ctx context.Context) error
}

// HealthHandler reports process liveness without checking optional dependencies.
func HealthHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// ReadinessHandler reports whether optional dependencies are ready.
func ReadinessHandler(checker HealthChecker) gin.HandlerFunc {
	return func(c *gin.Context) {
		if checker != nil {
			if err := checker.CheckHealth(c.Request.Context()); err != nil {
				observability.FromGin(c).Debug("Readiness check failed", "err", err)
				c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unhealthy"})
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}
