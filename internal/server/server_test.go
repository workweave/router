package server_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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

type healthCheckerFunc func(context.Context) error

func (f healthCheckerFunc) CheckHealth(ctx context.Context) error {
	return f(ctx)
}

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
		"GET /readyz",
		"GET /validate",
		"GET /v1/router/models",
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
		server.Register(engine, nil, nil, fakeDeployedModelsSource{}, server.DeploymentModeSelfHosted, nil, nil)
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
		// Pass a non-nil DeployedModelsSource: managed prod always boots a
		// *cluster.Multiversion router, so the catalog endpoint must mount
		// even though the dashboard does not.
		server.Register(engine, nil, nil, fakeDeployedModelsSource{}, server.DeploymentModeManaged, nil, nil)
		got := routeSet(engine)
		for _, want := range productRoutes {
			assert.Contains(t, got, want, "product route missing in managed mode")
		}
		for _, unwanted := range dashboardRoutes {
			assert.NotContains(t, got, unwanted, "dashboard route must not be mounted in managed mode")
		}
	})

	t.Run("nil deployed-models source skips catalog endpoint", func(t *testing.T) {
		engine := gin.New()
		server.Register(engine, nil, nil, nil, server.DeploymentModeManaged, nil, nil)
		got := routeSet(engine)
		assert.NotContains(t, got, "GET /v1/router/models", "catalog endpoint must not mount without a deployed-models source")
	})
}

func TestRegisterSeparatesLivenessFromReadiness(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	checker := healthCheckerFunc(func(context.Context) error {
		return errors.New("dependency unavailable")
	})
	server.Register(engine, nil, nil, nil, server.DeploymentModeManaged, nil, checker)

	for _, test := range []struct {
		path       string
		wantStatus int
	}{
		{path: "/health", wantStatus: http.StatusOK},
		{path: "/readyz", wantStatus: http.StatusServiceUnavailable},
	} {
		t.Run(test.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
			assert.Equal(t, test.wantStatus, response.Code)
		})
	}
}
