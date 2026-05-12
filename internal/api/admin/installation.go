package admin

import (
	"net/http"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"
	"workweave/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
)

// resolveInstallation returns the installation admin operations should act on. rk_-keyed callers see
// their own; admin-cookie sessions resolve to the singleton dashboard-owned installation, creating it
// on first call so the UI is usable on a fresh self-hosted deploy without out-of-band setup.
// Writes a 401 when no identity is present or 500 on lookup failure; callers return when ok == false.
func resolveInstallation(c *gin.Context, authSvc *auth.Service) (*auth.Installation, bool) {
	if installation := middleware.InstallationFrom(c); installation != nil {
		return installation, true
	}
	if admin := middleware.AdminPrincipalFrom(c); admin != nil {
		installation, err := authSvc.EnsureAdminInstallation(c.Request.Context())
		if err != nil {
			observability.FromGin(c).Error("Failed to ensure admin installation", "err", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve admin installation"})
			return nil, false
		}
		return installation, true
	}
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid_key"})
	return nil, false
}
