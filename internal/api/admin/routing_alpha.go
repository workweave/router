package admin

import (
	"errors"
	"net/http"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"

	"github.com/gin-gonic/gin"
)

type routingAlphaResponse struct {
	Alpha int `json:"alpha"`
	Min   int `json:"min"`
	Max   int `json:"max"`
}

type updateRoutingAlphaRequest struct {
	Alpha int `json:"alpha"`
}

// GetRoutingAlphaHandler returns the caller installation's current routing
// alpha plus the valid range so the UI can render a slider without hardcoding
// the bounds.
func GetRoutingAlphaHandler(authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}
		c.JSON(http.StatusOK, routingAlphaResponse{
			Alpha: installation.RoutingAlpha,
			Min:   auth.MinRoutingAlpha,
			Max:   auth.MaxRoutingAlpha,
		})
	}
}

// UpdateRoutingAlphaHandler replaces the installation's routing alpha. Out of
// range values return 400; the new value is visible to the request path
// within the APIKey cache TTL (same write-through-by-TTL pattern as excluded
// models).
func UpdateRoutingAlphaHandler(authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := observability.FromGin(c)
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}

		var req updateRoutingAlphaRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		if err := authSvc.SetInstallationRoutingAlpha(c.Request.Context(), installation.ExternalID, installation.ID, req.Alpha); err != nil {
			if errors.Is(err, auth.ErrAlphaOutOfRange) {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			log.Error("Failed to update routing alpha", "err", err, "installation_id", installation.ID, "alpha", req.Alpha)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to update routing alpha"})
			return
		}

		log.Info("Routing alpha updated", "installation_id", installation.ID, "alpha", req.Alpha)
		c.JSON(http.StatusOK, routingAlphaResponse{
			Alpha: req.Alpha,
			Min:   auth.MinRoutingAlpha,
			Max:   auth.MaxRoutingAlpha,
		})
	}
}
