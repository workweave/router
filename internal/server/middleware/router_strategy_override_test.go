package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workweave/router/internal/auth"
	"workweave/router/internal/router"
	"workweave/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// runStrategyOverride reports the strategy observed on the request context after WithRouterStrategyOverride runs.
func runStrategyOverride(t *testing.T, installation *auth.Installation, header string, available ...router.Strategy) router.Strategy {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		if installation != nil {
			c.Set("router_installation", installation)
		}
		c.Next()
	})
	engine.Use(middleware.WithRouterStrategyOverride(available...))

	var observed router.Strategy
	engine.GET("/probe", func(c *gin.Context) {
		observed = router.StrategyFromContext(c.Request.Context())
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	if header != "" {
		req.Header.Set(middleware.RouterStrategyOverrideHeader, header)
	}
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)
	return observed
}

func TestRouterStrategyOverride_AppliesRL(t *testing.T) {
	got := runStrategyOverride(t, overrideEnabledInstallation(), "rl")
	assert.Equal(t, router.StrategyRL, got, "rl header must select the RL strategy")
}

func TestRouterStrategyOverride_AppliesBandit(t *testing.T) {
	got := runStrategyOverride(t, overrideEnabledInstallation(), "bandit")
	assert.Equal(t, router.StrategyBandit, got, "bandit header must select the bandit strategy")
}

func TestRouterStrategyOverride_AppliesHMM(t *testing.T) {
	got := runStrategyOverride(t, overrideEnabledInstallation(), "hmm")
	assert.Equal(t, router.StrategyHMM, got, "hmm header must select the HMM strategy")
}

func TestRouterStrategyOverride_CaseInsensitiveAndTrimmed(t *testing.T) {
	got := runStrategyOverride(t, overrideEnabledInstallation(), "  RL  ")
	assert.Equal(t, router.StrategyRL, got, "value must be lowercased and trimmed before matching")
}

func TestRouterStrategyOverride_NoHeaderDefaultsToCluster(t *testing.T) {
	got := runStrategyOverride(t, &auth.Installation{ID: "inst-eval"}, "")
	assert.Equal(t, router.StrategyCluster, got, "absent header must leave the default cluster strategy")
}

func TestRouterStrategyOverride_UsesDeploymentDefaultWithoutInstallationOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set("router_installation", &auth.Installation{ID: "inst-global"})
		c.Next()
	})
	engine.Use(middleware.WithRouterStrategyDefault(
		router.StrategyHMM,
		router.StrategyHMM,
	))
	var observed router.Strategy
	engine.GET("/probe", func(c *gin.Context) {
		observed = router.StrategyFromContext(c.Request.Context())
		c.Status(http.StatusOK)
	})

	request := httptest.NewRequest(http.MethodGet, "/probe", nil)
	engine.ServeHTTP(httptest.NewRecorder(), request)

	assert.Equal(t, router.StrategyHMM, observed)
}

func TestRouterStrategyOverride_ExplicitClusterWinsOverDeploymentDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set("router_installation", &auth.Installation{
			ID: "inst-cluster-holdout", RoutingStrategy: router.StrategyCluster,
		})
		c.Next()
	})
	engine.Use(middleware.WithRouterStrategyDefault(
		router.StrategyHMM,
		router.StrategyHMM,
	))
	var observed router.Strategy
	engine.GET("/probe", func(c *gin.Context) {
		observed = router.StrategyFromContext(c.Request.Context())
		c.Status(http.StatusOK)
	})

	request := httptest.NewRequest(http.MethodGet, "/probe", nil)
	engine.ServeHTTP(httptest.NewRecorder(), request)

	assert.Equal(t, router.StrategyCluster, observed)
}

func TestNormalizeRouterStrategyDefault(t *testing.T) {
	assert.Equal(t, router.StrategyHMM, middleware.NormalizeRouterStrategyDefault(router.StrategyHMM, router.StrategyHMM))
	assert.Equal(t, router.StrategyCluster, middleware.NormalizeRouterStrategyDefault(router.Strategy("typo"), router.StrategyHMM))
}

func TestRouterStrategyOverride_UnknownValueIgnored(t *testing.T) {
	got := runStrategyOverride(t, overrideEnabledInstallation(), "bogus")
	assert.Equal(t, router.StrategyCluster, got, "unrecognized strategy must fall through to the default")
}

func TestRouterStrategyOverride_AppliesPersistedStrategyWithoutHeader(t *testing.T) {
	got := runStrategyOverride(t, &auth.Installation{ID: "inst-allowlisted", RoutingStrategy: router.StrategyHMM}, "")
	assert.Equal(t, router.StrategyHMM, got)
}

func TestRouterStrategyOverride_NormalizesPersistedStrategy(t *testing.T) {
	got := runStrategyOverride(t, &auth.Installation{ID: "inst-allowlisted", RoutingStrategy: router.Strategy("  HMM  ")}, "")
	assert.Equal(t, router.StrategyHMM, got)
}

func TestRouterStrategyOverride_UnauthorizedHeaderPreservesPersistedStrategy(t *testing.T) {
	got := runStrategyOverride(t, &auth.Installation{ID: "inst-customer", RoutingStrategy: router.StrategyHMM}, "rl")
	assert.Equal(t, router.StrategyHMM, got)
}

func TestRouterStrategyOverride_AcceptsFutureRegisteredStrategy(t *testing.T) {
	future := router.Strategy("future-policy")
	installation := overrideEnabledInstallation()
	got := runStrategyOverride(t, installation, string(future), future)
	assert.Equal(t, future, got)
}

func TestRouterStrategyOverride_NoOpsWhenInstallationMissing(t *testing.T) {
	got := runStrategyOverride(t, nil, "rl")
	assert.Equal(t, router.StrategyCluster, got, "missing installation (WithAuth bypassed) must not flip strategy")
}

func overrideEnabledInstallation() *auth.Installation {
	return &auth.Installation{ID: "inst-eval", PolicyHeaderOverridesEnabled: true}
}
