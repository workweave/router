// Package admin holds operational handlers (health, validate) that aren't part
// of any provider-compat surface.
package admin

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// HealthHandler is the liveness/startup probe target.
func HealthHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
