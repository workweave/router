package server_test

import (
	"testing"

	"workweave/router/internal/router/cluster"
	"workweave/router/internal/server"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// fakeDeployedModelsSource is a stand-in for *cluster.Multiversion in route
// registration tests; the handler closures it backs are never invoked.
type fakeDeployedModelsSource struct{}

func (fakeDeployedModelsSource) DefaultDeployedModels() []cluster.DeployedEntry { return nil }

// routeSet collects "METHOD path" pairs so assertions are robust to additions of unrelated product routes.
func routeSet(engine *gin.Engine) map[string]struct{} {
	out := make(map[string]struct{}, len(engine.Routes()))
	for _, r := range engine.Routes() {
		out[r.Method+" "+r.Path] = struct{}{}
	}
	return out
}

func TestRegister_DeploymentMode(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Product surface — always mounted regardless of deployment mode.
	productRoutes := []string{
		"GET /health",
		"GET /validate",
		"POST /v1/messages",
		"POST /v1/chat/completions",
		"POST /v1/responses",
		"POST /v1/route",
		"POST /v1/messages/count_tokens",
		"GET /v1/models",
		"GET /v1/models/:model",
	}

	// Self-hoster dashboard surface — gated by DeploymentModeSelfHosted.
	dashboardRoutes := []string{
		"GET /",
		"GET /ui/*filepath",
		"HEAD /ui/*filepath",
		"POST /admin/v1/auth/login",
		"POST /admin/v1/auth/logout",
		"GET /admin/v1/auth/me",
		"GET /admin/v1/metrics/summary",
		"GET /admin/v1/metrics/timeseries",
		"GET /admin/v1/keys",
		"POST /admin/v1/keys",
		"DELETE /admin/v1/keys/:id",
		"GET /admin/v1/provider-keys",
		"POST /admin/v1/provider-keys",
		"DELETE /admin/v1/provider-keys/:id",
		"GET /admin/v1/config",
		"GET /admin/v1/excluded-models",
		"PUT /admin/v1/excluded-models",
	}

	t.Run("selfhosted mounts dashboard and product routes", func(t *testing.T) {
		engine := gin.New()
		// Nil services are fine: engine.Routes() inspection never invokes the closure-captured handlers.
		server.Register(engine, nil, nil, fakeDeployedModelsSource{}, server.DeploymentModeSelfHosted)
		got := routeSet(engine)
		for _, want := range productRoutes {
			assert.Contains(t, got, want, "product route missing in selfhosted mode")
		}
		for _, want := range dashboardRoutes {
			assert.Contains(t, got, want, "dashboard route missing in selfhosted mode")
		}
	})

	t.Run("managed skips dashboard but keeps product routes", func(t *testing.T) {
		engine := gin.New()
		server.Register(engine, nil, nil, nil, server.DeploymentModeManaged)
		got := routeSet(engine)
		for _, want := range productRoutes {
			assert.Contains(t, got, want, "product route missing in managed mode")
		}
		for _, unwanted := range dashboardRoutes {
			assert.NotContains(t, got, unwanted, "dashboard route must not be mounted in managed mode")
		}
	})
}
