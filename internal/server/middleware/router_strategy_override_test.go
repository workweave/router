package middleware_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workweave/router/internal/auth"
	"workweave/router/internal/router"
	"workweave/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runStrategyOverride reports the strategy observed on the request context after WithRouterStrategyOverride runs.
func runStrategyOverride(t *testing.T, installation *auth.Installation, header string, available ...router.Strategy) router.Strategy {
	t.Helper()
	_, observed, _ := runStrategyOverrideAt(t, "/probe", installation, header, available...)
	return observed
}

func runStrategyOverrideResult(t *testing.T, installation *auth.Installation, header string, available ...router.Strategy) (int, router.Strategy) {
	t.Helper()
	status, observed, _ := runStrategyOverrideAt(t, "/v1/route", installation, header, available...)
	return status, observed
}

// runStrategyOverrideAt drives WithRouterStrategyOverride against path so
// detectAPIFormat can pick the correct error envelope on fail-closed aborts.
func runStrategyOverrideAt(t *testing.T, path string, installation *auth.Installation, header string, available ...router.Strategy) (int, router.Strategy, *httptest.ResponseRecorder) {
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
	engine.Any("/*any", func(c *gin.Context) {
		observed = router.StrategyFromContext(c.Request.Context())
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, path, nil)
	if header != "" {
		req.Header.Set(middleware.RouterStrategyOverrideHeader, header)
	}
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)
	return rr.Code, observed, rr
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

func TestRouterStrategyOverride_RejectsUnregisteredPersistedStrategy(t *testing.T) {
	unregistered := router.Strategy("quality-v2")
	status, got, rr := runStrategyOverrideAt(t, "/v1/route", &auth.Installation{
		ID:              "inst-custom",
		RoutingStrategy: unregistered,
	}, "")
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, "1", rr.Header().Get("Retry-After"))
	assert.NotEqual(t, router.StrategyCluster, got, "must not silently rewrite to cluster")
	assertAnthropicUnavailableEnvelope(t, rr.Body.Bytes())
}

func TestRouterStrategyOverride_UnregisteredPersisted_HeaderDisabledReturns503(t *testing.T) {
	status, got, rr := runStrategyOverrideAt(t, "/v1/route", &auth.Installation{
		ID:                           "inst-custom",
		RoutingStrategy:              router.Strategy("quality-v2"),
		PolicyHeaderOverridesEnabled: false,
	}, "cluster")
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, "1", rr.Header().Get("Retry-After"))
	assert.NotEqual(t, router.StrategyCluster, got, "disabled header must not rescue an unregistered persisted strategy")
	assertAnthropicUnavailableEnvelope(t, rr.Body.Bytes())
}

func TestRouterStrategyOverride_UnregisteredPersisted_ValidHeaderOverrideWins(t *testing.T) {
	status, got := runStrategyOverrideResult(t, &auth.Installation{
		ID:                           "inst-custom",
		RoutingStrategy:              router.Strategy("quality-v2"),
		PolicyHeaderOverridesEnabled: true,
	}, "cluster")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, router.StrategyCluster, got, "authorized valid header must override an unregistered persisted strategy")
}

func TestRouterStrategyOverride_UnregisteredPersisted_InvalidHeaderStill503(t *testing.T) {
	status, got, rr := runStrategyOverrideAt(t, "/v1/route", &auth.Installation{
		ID:                           "inst-custom",
		RoutingStrategy:              router.Strategy("quality-v2"),
		PolicyHeaderOverridesEnabled: true,
	}, "bogus")
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, "1", rr.Header().Get("Retry-After"))
	assert.NotEqual(t, router.StrategyCluster, got, "invalid header must not silently rewrite to cluster")
	assertAnthropicUnavailableEnvelope(t, rr.Body.Bytes())
}

func TestRouterStrategyOverride_FailClosedEnvelopeOpenAI(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			status, _, rr := runStrategyOverrideAt(t, path, &auth.Installation{
				ID:              "inst-custom",
				RoutingStrategy: router.Strategy("quality-v2"),
			}, "")
			assert.Equal(t, http.StatusServiceUnavailable, status)
			assert.Equal(t, "1", rr.Header().Get("Retry-After"))

			body := parseJSONBody(t, rr.Body.Bytes())
			errObj, ok := body["error"].(map[string]any)
			require.True(t, ok, "OpenAI envelope must have top-level 'error' object")
			assert.Equal(t, "api_error", errObj["type"])
			assert.Contains(t, errObj["message"], "selected policy router is not configured")
			_, hasParam := errObj["param"]
			_, hasCode := errObj["code"]
			assert.True(t, hasParam, "OpenAI envelope must include 'param' field")
			assert.True(t, hasCode, "OpenAI envelope must include 'code' field")
			_, hasOuterType := body["type"]
			assert.False(t, hasOuterType, "OpenAI envelope must not include outer 'type' (that's the Anthropic shape)")
		})
	}
}

func TestRouterStrategyOverride_FailClosedEnvelopeAnthropic(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/route"} {
		t.Run(path, func(t *testing.T) {
			status, _, rr := runStrategyOverrideAt(t, path, &auth.Installation{
				ID:              "inst-custom",
				RoutingStrategy: router.Strategy("quality-v2"),
			}, "")
			assert.Equal(t, http.StatusServiceUnavailable, status)
			assert.Equal(t, "1", rr.Header().Get("Retry-After"))
			assertAnthropicUnavailableEnvelope(t, rr.Body.Bytes())
		})
	}
}

func TestRouterStrategyOverride_FailClosedEnvelopeGemini(t *testing.T) {
	status, _, rr := runStrategyOverrideAt(t, "/v1beta/models/gemini-2.5-pro:generateContent", &auth.Installation{
		ID:              "inst-custom",
		RoutingStrategy: router.Strategy("quality-v2"),
	}, "")
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, "1", rr.Header().Get("Retry-After"))

	body := parseJSONBody(t, rr.Body.Bytes())
	errObj, ok := body["error"].(map[string]any)
	require.True(t, ok, "Gemini envelope must have nested 'error' object")
	assert.EqualValues(t, http.StatusServiceUnavailable, errObj["code"], "Gemini envelope echoes HTTP status as numeric 'code'")
	assert.Equal(t, "UNAVAILABLE", errObj["status"], "Gemini envelope must include machine-readable 'status'")
	assert.Contains(t, errObj["message"], "selected policy router is not configured")
}

func TestRouterStrategyOverride_NoOpsWhenInstallationMissing(t *testing.T) {
	got := runStrategyOverride(t, nil, "rl")
	assert.Equal(t, router.StrategyCluster, got, "missing installation (WithAuth bypassed) must not flip strategy")
}

func overrideEnabledInstallation() *auth.Installation {
	return &auth.Installation{ID: "inst-eval", PolicyHeaderOverridesEnabled: true}
}

func parseJSONBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body), "response body must be JSON")
	return body
}

func assertAnthropicUnavailableEnvelope(t *testing.T, raw []byte) {
	t.Helper()
	body := parseJSONBody(t, raw)
	assert.Equal(t, "error", body["type"], "Anthropic envelope must have top-level 'type': 'error'")
	errObj, ok := body["error"].(map[string]any)
	require.True(t, ok, "Anthropic envelope must have nested 'error' object")
	assert.Equal(t, "api_error", errObj["type"])
	assert.Contains(t, errObj["message"], "selected policy router is not configured")
}
