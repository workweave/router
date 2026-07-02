package admin_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workweave/router/internal/api/admin"
	"workweave/router/internal/version"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVersionHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)

	origCommit, origPR := version.Commit, version.PR
	t.Cleanup(func() { version.Commit, version.PR = origCommit, origPR })
	version.Commit = "0fb46ee9707c8db7d0ef69b7308a79a95d559e25"
	version.PR = "572"
	t.Setenv("ROUTER_CLUSTER_VERSION", "v0.71")

	engine := gin.New()
	engine.GET("/v1/version", admin.VersionHandler)

	req := httptest.NewRequest(http.MethodGet, "/v1/version", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Commit         string `json:"commit"`
		CommitShort    string `json:"commit_short"`
		PR             string `json:"pr"`
		Display        string `json:"display"`
		BuildTime      string `json:"build_time"`
		ClusterVersion string `json:"cluster_version"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	assert.Equal(t, "0fb46ee9707c8db7d0ef69b7308a79a95d559e25", body.Commit)
	assert.Equal(t, "0fb46ee", body.CommitShort)
	assert.Equal(t, "572", body.PR)
	assert.Equal(t, "#572 (0fb46ee)", body.Display)
	assert.Equal(t, "v0.71", body.ClusterVersion)
}
