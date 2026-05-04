package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workweave/router/internal/auth"
	"workweave/router/internal/router/cluster"
	"workweave/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// runVersionOverride exercises WithClusterVersionOverride and reports
// the version (if any) the handler observed on the request context.
func runVersionOverride(t *testing.T, installation *auth.Installation, header string) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		if installation != nil {
			c.Set("router_installation", installation)
		}
		c.Next()
	})
	engine.Use(middleware.WithClusterVersionOverride())

	var observed string
	engine.GET("/probe", func(c *gin.Context) {
		observed = cluster.VersionFromContext(c.Request.Context())
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	if header != "" {
		req.Header.Set(middleware.ClusterVersionOverrideHeader, header)
	}
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)
	return observed
}

func TestClusterVersionOverride_AppliesForAllowListedInstallation(t *testing.T) {
	got := runVersionOverride(t, &auth.Installation{ID: "inst-eval", IsEvalAllowlisted: true}, "v0.1")
	assert.Equal(t, "v0.1", got, "allow-listed installation must propagate header value to context")
}

func TestClusterVersionOverride_TrimsWhitespace(t *testing.T) {
	got := runVersionOverride(t, &auth.Installation{ID: "inst-eval", IsEvalAllowlisted: true}, "  v0.2  ")
	assert.Equal(t, "v0.2", got, "leading/trailing whitespace must be trimmed before stashing")
}

func TestClusterVersionOverride_NoHeaderNoOp(t *testing.T) {
	got := runVersionOverride(t, &auth.Installation{ID: "inst-eval", IsEvalAllowlisted: true}, "")
	assert.Empty(t, got, "absent header must not stash anything on context")
}

func TestClusterVersionOverride_SilentlyIgnoresNonAllowListedInstallation(t *testing.T) {
	got := runVersionOverride(t, &auth.Installation{ID: "inst-customer", IsEvalAllowlisted: false}, "v0.1")
	assert.Empty(t, got, "non-allow-listed installation must not get override even with header set")
}

func TestClusterVersionOverride_NoOpsWhenInstallationMissing(t *testing.T) {
	got := runVersionOverride(t, nil, "v0.1")
	assert.Empty(t, got, "missing installation (WithAuth bypassed) must not produce override")
}
