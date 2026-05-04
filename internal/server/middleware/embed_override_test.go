package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workweave/router/internal/auth"
	"workweave/router/internal/proxy"
	"workweave/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// runEmbedOverride exercises WithEmbedLastUserMessageOverride against a
// single request and returns (set, value) where set reports whether the
// override was attached to the request context and value is the bool
// the handler observed.
func runEmbedOverride(t *testing.T, installation *auth.Installation, header string) (bool, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		if installation != nil {
			c.Set("router_installation", installation)
		}
		c.Next()
	})
	engine.Use(middleware.WithEmbedLastUserMessageOverride())

	var observedSet bool
	var observedValue bool
	engine.GET("/probe", func(c *gin.Context) {
		v, ok := c.Request.Context().Value(proxy.EmbedLastUserMessageContextKey{}).(bool)
		observedSet = ok
		observedValue = v
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	if header != "" {
		req.Header.Set(middleware.EmbedLastUserMessageOverrideHeader, header)
	}
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)
	return observedSet, observedValue
}

func TestEmbedOverride_AppliesTrueForAllowListedInstallation(t *testing.T) {
	allow := &auth.Installation{ID: "inst-eval", IsEvalAllowlisted: true}
	set, val := runEmbedOverride(t, allow, "true")
	assert.True(t, set, "header=true on allow-listed installation must set context override")
	assert.True(t, val)
}

func TestEmbedOverride_AppliesFalseForAllowListedInstallation(t *testing.T) {
	allow := &auth.Installation{ID: "inst-eval", IsEvalAllowlisted: true}
	set, val := runEmbedOverride(t, allow, "false")
	assert.True(t, set, "header=false on allow-listed installation must set context override")
	assert.False(t, val)
}

func TestEmbedOverride_NoHeaderLeavesDefault(t *testing.T) {
	allow := &auth.Installation{ID: "inst-eval", IsEvalAllowlisted: true}
	set, _ := runEmbedOverride(t, allow, "")
	assert.False(t, set, "no header must leave the context untouched so the server config wins")
}

func TestEmbedOverride_IgnoresUnknownHeaderValue(t *testing.T) {
	allow := &auth.Installation{ID: "inst-eval", IsEvalAllowlisted: true}
	for _, header := range []string{"yes", "0", "1", "TRUE-ish"} {
		set, _ := runEmbedOverride(t, allow, header)
		assert.Falsef(t, set, "header=%q must not set the context override (only true/false honored)", header)
	}
}

func TestEmbedOverride_SilentlyIgnoresNonAllowListedInstallation(t *testing.T) {
	customer := &auth.Installation{ID: "inst-customer", IsEvalAllowlisted: false}
	set, _ := runEmbedOverride(t, customer, "true")
	assert.False(t, set, "non-allow-listed installation must not get override even with header=true")
}

func TestEmbedOverride_NoOpsWhenInstallationMissing(t *testing.T) {
	set, _ := runEmbedOverride(t, nil, "true")
	assert.False(t, set, "missing installation (WithAuth bypassed) must not produce override")
}
