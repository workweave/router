package admin

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
)

// HealthChecker is an optional readiness gate. When non-nil and its
// Check call returns an error, /health reports 503 so the platform
// (Cloud Run) defers traffic until the dependency is ready.
type HealthChecker interface {
	CheckHealth(ctx context.Context) error
}

// HealthHandler returns a gin handler that reports service health.
// When checker is nil the handler always returns 200.
func HealthHandler(checker HealthChecker) gin.HandlerFunc {
	return func(c *gin.Context) {
		if checker != nil {
			if err := checker.CheckHealth(c.Request.Context()); err != nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"status": "unhealthy",
					"error":  err.Error(),
				})
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}
