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
func runStrategyOverride(t *testing.T, installation *auth.Installation, header string) router.Strategy {
	t.Helper()
	return runStrategyOverrideWithKey(t, installation, nil, header)
}

// runStrategyOverrideWithKey is runStrategyOverride plus an authed API key, so
// tests can exercise the key-default fallback (APIKeyFrom(c)).
func runStrategyOverrideWithKey(t *testing.T, installation *auth.Installation, apiKey *auth.APIKey, header string) router.Strategy {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		if installation != nil {
			c.Set("router_installation", installation)
		}
		if apiKey != nil {
			c.Set("router_api_key", apiKey)
		}
		c.Next()
	})
	engine.Use(middleware.WithRouterStrategyOverride())

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
	got := runStrategyOverride(t, &auth.Installation{ID: "inst-eval"}, "rl")
	assert.Equal(t, router.StrategyRL, got, "rl header must select the RL strategy")
}

func TestRouterStrategyOverride_AppliesBandit(t *testing.T) {
	got := runStrategyOverride(t, &auth.Installation{ID: "inst-eval"}, "bandit")
	assert.Equal(t, router.StrategyBandit, got, "bandit header must select the bandit strategy")
}

func TestRouterStrategyOverride_AppliesHMM(t *testing.T) {
	got := runStrategyOverride(t, &auth.Installation{ID: "inst-eval"}, "hmm")
	assert.Equal(t, router.StrategyHMM, got, "hmm header must select the HMM strategy")
}

func TestRouterStrategyOverride_CaseInsensitiveAndTrimmed(t *testing.T) {
	got := runStrategyOverride(t, &auth.Installation{ID: "inst-eval"}, "  RL  ")
	assert.Equal(t, router.StrategyRL, got, "value must be lowercased and trimmed before matching")
}

func TestRouterStrategyOverride_NoHeaderDefaultsToCluster(t *testing.T) {
	got := runStrategyOverride(t, &auth.Installation{ID: "inst-eval"}, "")
	assert.Equal(t, router.StrategyCluster, got, "absent header must leave the default cluster strategy")
}

func TestRouterStrategyOverride_UnknownValueIgnored(t *testing.T) {
	got := runStrategyOverride(t, &auth.Installation{ID: "inst-eval"}, "bogus")
	assert.Equal(t, router.StrategyCluster, got, "unrecognized strategy must fall through to the default")
}

func TestRouterStrategyOverride_NoOpsWhenInstallationMissing(t *testing.T) {
	got := runStrategyOverride(t, nil, "rl")
	assert.Equal(t, router.StrategyCluster, got, "missing installation (WithAuth bypassed) must not flip strategy")
}

func TestRouterStrategyOverride_HeaderAbsentFallsBackToKeyDefault(t *testing.T) {
	apiKey := &auth.APIKey{ID: "key-1", DefaultStrategy: "hmm"}
	got := runStrategyOverrideWithKey(t, &auth.Installation{ID: "inst-eval"}, apiKey, "")
	assert.Equal(t, router.StrategyHMM, got, "absent header must fall back to the key's default_strategy (the Cursor path)")
}

func TestRouterStrategyOverride_HeaderWinsOverKeyDefault(t *testing.T) {
	apiKey := &auth.APIKey{ID: "key-1", DefaultStrategy: "hmm"}
	got := runStrategyOverrideWithKey(t, &auth.Installation{ID: "inst-eval"}, apiKey, "cluster")
	assert.Equal(t, router.StrategyCluster, got, "an explicit header must win over the key's default_strategy")
}

func TestRouterStrategyOverride_UnrecognizedHeaderFallsBackToKeyDefault(t *testing.T) {
	apiKey := &auth.APIKey{ID: "key-1", DefaultStrategy: "hmm"}
	got := runStrategyOverrideWithKey(t, &auth.Installation{ID: "inst-eval"}, apiKey, "bogus")
	assert.Equal(t, router.StrategyHMM, got, "an unrecognized header must fall through to the key's default_strategy, not straight to cluster")
}

func TestRouterStrategyOverride_UnknownKeyDefaultIgnored(t *testing.T) {
	apiKey := &auth.APIKey{ID: "key-1", DefaultStrategy: "not-a-real-strategy"}
	got := runStrategyOverrideWithKey(t, &auth.Installation{ID: "inst-eval"}, apiKey, "")
	assert.Equal(t, router.StrategyCluster, got, "an unrecognized stored default_strategy must fall through to cluster, not error")
}

func TestRouterStrategyOverride_EmptyKeyDefaultLeavesCluster(t *testing.T) {
	apiKey := &auth.APIKey{ID: "key-1", DefaultStrategy: ""}
	got := runStrategyOverrideWithKey(t, &auth.Installation{ID: "inst-eval"}, apiKey, "")
	assert.Equal(t, router.StrategyCluster, got, "a key with no default_strategy set must leave the deployment default")
}
