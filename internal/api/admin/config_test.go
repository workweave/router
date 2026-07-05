package admin_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workweave/router/internal/api/admin"
	"workweave/router/internal/auth"
	"workweave/router/internal/providers"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestConfigHandler_EnvProviderKeys_IncludesEveryDeployedProvider guards
// [114]: ConfigHandler used to derive its display list from a hand-maintained
// literal that silently omitted deepinfra and bedrock even though both are
// wired into providerMap, so an operator who set the env var still saw the
// key reported as absent. ConfigHandler must now report every provider
// whose env var is actually set, regardless of when the provider was added
// to internal/providers.
func TestConfigHandler_EnvProviderKeys_IncludesEveryDeployedProvider(t *testing.T) {
	t.Setenv(providers.APIKeyEnvVar(providers.ProviderDeepInfra), "dummy-key")
	t.Setenv(providers.APIKeyEnvVar(providers.ProviderBedrock), "dummy-key")

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/admin/v1/config", func(c *gin.Context) {
		c.Set("router_installation", &auth.Installation{ID: "inst-1"})
	}, admin.ConfigHandler)

	req := httptest.NewRequest(http.MethodGet, "/admin/v1/config", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		EnvProviderKeys []string `json:"env_provider_keys"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Contains(t, body.EnvProviderKeys, providers.ProviderDeepInfra)
	require.Contains(t, body.EnvProviderKeys, providers.ProviderBedrock)
}
