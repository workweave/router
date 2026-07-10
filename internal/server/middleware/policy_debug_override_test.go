package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"workweave/router/internal/auth"
	"workweave/router/internal/proxy"
	"workweave/router/internal/server/middleware"
)

func runPolicyDebugOverride(t *testing.T, installation *auth.Installation, header string) bool {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		if installation != nil {
			c.Set("router_installation", installation)
		}
		c.Next()
	})
	engine.Use(middleware.WithPolicyDebugOverride())
	var observed bool
	engine.GET("/probe", func(c *gin.Context) {
		observed, _ = c.Request.Context().Value(proxy.PolicyDebugEnabledContextKey{}).(bool)
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	if header != "" {
		req.Header.Set(middleware.RouterPolicyDebugHeader, header)
	}
	engine.ServeHTTP(httptest.NewRecorder(), req)
	return observed
}

func TestPolicyDebugOverride_UsesPersistedValue(t *testing.T) {
	assert.True(t, runPolicyDebugOverride(t, &auth.Installation{PolicyDebugEnabled: true}, ""))
}

func TestPolicyDebugOverride_AuthorizedHeaderOverridesPersistedValue(t *testing.T) {
	installation := &auth.Installation{PolicyDebugEnabled: true, PolicyHeaderOverridesEnabled: true}
	assert.False(t, runPolicyDebugOverride(t, installation, "false"))
}

func TestPolicyDebugOverride_UnauthorizedHeaderPreservesPersistedValue(t *testing.T) {
	installation := &auth.Installation{PolicyDebugEnabled: false, PolicyHeaderOverridesEnabled: false}
	assert.False(t, runPolicyDebugOverride(t, installation, "true"))
}
