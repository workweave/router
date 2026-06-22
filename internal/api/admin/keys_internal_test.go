package admin

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"workweave/router/internal/auth"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestIsEnvKeyed_TrueWhenEnvVarSet(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-value")
	assert.True(t, isEnvKeyed("anthropic"))
}

func TestIsEnvKeyed_FalseWhenEnvVarUnset(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	os.Unsetenv("OPENAI_API_KEY")
	assert.False(t, isEnvKeyed("openai"))
}

func TestIsEnvKeyed_FalseForUnknownProvider(t *testing.T) {
	assert.False(t, isEnvKeyed("not-a-real-provider"))
}

func TestUpsertExternalKeyHandler_RejectsEnvKeyedProvider(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-env-set")
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	// Inject a fake installation so resolveInstallation passes auth,
	// letting the isEnvKeyed guard fire.
	engine.Use(func(c *gin.Context) {
		c.Set("router_installation", &auth.Installation{ID: "test-install"})
		c.Next()
	})
	// nil auth.Service is safe — isEnvKeyed fires before any service call.
	engine.POST("/admin/v1/provider-keys", UpsertExternalKeyHandler(nil))

	body := `{"provider":"anthropic","key":"sk-ant-should-be-rejected"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/provider-keys", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	engine.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "error")
}
