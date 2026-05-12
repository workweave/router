package admin

import (
	"net/http"

	"workweave/router/internal/config"
	"workweave/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
)

type configResponse struct {
	ClusterVersion       string `json:"cluster_version"`
	EmbedLastUserMsg     bool   `json:"embed_last_user_message"`
	StickyDecisionTTL    string `json:"sticky_decision_ttl_ms"`
	OtelEnabled          bool   `json:"otel_enabled"`
	SemanticCacheEnabled bool   `json:"semantic_cache_enabled"`
}

// ConfigHandler returns the current non-secret router configuration. Accepts
// either an admin session cookie or a valid rk_ bearer.
func ConfigHandler(c *gin.Context) {
	if middleware.AdminPrincipalFrom(c) == nil && middleware.InstallationFrom(c) == nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid_key"})
		return
	}
	c.JSON(http.StatusOK, configResponse{
		ClusterVersion:       config.GetOr("ROUTER_CLUSTER_VERSION", "artifacts/latest"),
		EmbedLastUserMsg:     config.GetOr("ROUTER_EMBED_LAST_USER_MESSAGE", "false") == "true",
		StickyDecisionTTL:    config.GetOr("ROUTER_STICKY_DECISION_TTL_MS", "0"),
		OtelEnabled:          config.GetOr("OTEL_EXPORTER_OTLP_ENDPOINT", "") != "",
		SemanticCacheEnabled: config.GetOr("ROUTER_SEMANTIC_CACHE_ENABLED", "true") == "true",
	})
}
