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
	// EnvProviderKeys lists provider names whose upstream API key is set
	// via env var on the deployment (e.g. OPENAI_API_KEY). The dashboard
	// renders these as read-only entries — they aren't stored in Postgres
	// and can only be unset by editing the deployment env + restarting.
	EnvProviderKeys []string `json:"env_provider_keys"`
}

// envProviderKey maps a provider name to the env var that configures its
// upstream API key. Keep in sync with cmd/router/main.go's provider wiring.
var envProviderKey = map[string]string{
	"anthropic":  "ANTHROPIC_API_KEY",
	"openai":     "OPENAI_API_KEY",
	"openrouter": "OPENROUTER_API_KEY",
	"fireworks":  "FIREWORKS_API_KEY",
	"google":     "GOOGLE_API_KEY",
}

// ConfigHandler returns the current non-secret router configuration. Accepts
// either an admin session cookie or a valid rk_ bearer.
func ConfigHandler(c *gin.Context) {
	if middleware.AdminPrincipalFrom(c) == nil && middleware.InstallationFrom(c) == nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid_key"})
		return
	}
	envKeyed := make([]string, 0, len(envProviderKey))
	// Iterate over a stable order so the response is deterministic.
	for _, p := range []string{"anthropic", "openai", "openrouter", "fireworks", "google"} {
		if config.GetOr(envProviderKey[p], "") != "" {
			envKeyed = append(envKeyed, p)
		}
	}
	c.JSON(http.StatusOK, configResponse{
		ClusterVersion:       config.GetOr("ROUTER_CLUSTER_VERSION", "artifacts/latest"),
		EmbedLastUserMsg:     config.GetOr("ROUTER_EMBED_LAST_USER_MESSAGE", "false") == "true",
		StickyDecisionTTL:    config.GetOr("ROUTER_STICKY_DECISION_TTL_MS", "0"),
		OtelEnabled:          config.GetOr("OTEL_EXPORTER_OTLP_ENDPOINT", "") != "",
		SemanticCacheEnabled: config.GetOr("ROUTER_SEMANTIC_CACHE_ENABLED", "true") == "true",
		EnvProviderKeys:      envKeyed,
	})
}
