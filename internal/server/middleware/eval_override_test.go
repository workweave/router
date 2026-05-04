package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workweave/router/internal/auth"
	"workweave/router/internal/router/evalswitch"
	"workweave/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// runOverride exercises WithEvalRoutingOverride against a single
// request and reports whether the request handler observed an
// override decision attached to the request context.
func runOverride(t *testing.T, installation *auth.Installation, header string) bool {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	// Stand-in for WithAuth: attach the installation to the gin context
	// the same way the real WithAuth middleware does.
	engine.Use(func(c *gin.Context) {
		if installation != nil {
			c.Set("router_installation", installation)
		}
		c.Next()
	})
	engine.Use(middleware.WithEvalRoutingOverride())

	var observed bool
	engine.GET("/probe", func(c *gin.Context) {
		d, ok := c.Request.Context().Value(evalswitch.ContextKey{}).(evalswitch.Decision)
		observed = ok && d.UseFallback
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	if header != "" {
		req.Header.Set(middleware.EvalOverrideHeader, header)
	}
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)
	return observed
}

func TestEvalOverride_AppliesForAllowListedInstallation(t *testing.T) {
	got := runOverride(t, &auth.Installation{ID: "inst-eval", IsEvalAllowlisted: true}, "true")
	assert.True(t, got, "allow-listed installation with header=true must produce override")
}

func TestEvalOverride_IgnoresHeaderUnsetOrFalse(t *testing.T) {
	for _, header := range []string{"", "false", "0", "yes", "TRUE-ish"} {
		got := runOverride(t, &auth.Installation{ID: "inst-eval", IsEvalAllowlisted: true}, header)
		assert.Falsef(t, got, "header=%q must not produce override", header)
	}
}

func TestEvalOverride_HonorsCaseInsensitiveTrue(t *testing.T) {
	for _, header := range []string{"true", "True", "TRUE", " true "} {
		got := runOverride(t, &auth.Installation{ID: "inst-eval", IsEvalAllowlisted: true}, header)
		assert.Truef(t, got, "header=%q (case-insensitive true) must produce override", header)
	}
}

func TestEvalOverride_SilentlyIgnoresNonAllowListedInstallation(t *testing.T) {
	got := runOverride(t, &auth.Installation{ID: "inst-customer", IsEvalAllowlisted: false}, "true")
	assert.False(t, got, "non-allow-listed installation must not get override even with header=true")
}

func TestEvalOverride_NoOpsWhenInstallationMissing(t *testing.T) {
	got := runOverride(t, nil, "true")
	assert.False(t, got, "missing installation (WithAuth bypassed) must not produce override")
}
