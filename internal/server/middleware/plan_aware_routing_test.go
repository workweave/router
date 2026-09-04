package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"weave-os/router/internal/auth"
	"weave-os/router/internal/flags"
	"weave-os/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithAuthPropagatesPlanAwareOrganizationFlag(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const routerToken = "rk_plan_aware_test"
	hash, prefix, suffix := auth.APITokenFingerprint(routerToken)
	installation := &auth.Installation{ID: "test-installation", ExternalID: "test-organization"}
	repo := &fakeAPIKeyRepository{byHash: map[string]fakeKeyRow{
		hash: {
			apiKey:       &auth.APIKey{ID: "test-key", KeyHash: hash, KeyPrefix: prefix, KeySuffix: suffix},
			installation: installation,
		},
	}}
	svc := auth.NewService(fakeInstallationRepository{}, repo, nil, nil, auth.NoOpAPIKeyCache{}, nil, time.Now)
	engine := gin.New()
	engine.Use(middleware.WithAuth(svc, false))
	var enabled bool
	engine.GET("/probe", func(c *gin.Context) {
		enabled = flags.BoolOr(c.Request.Context(), flags.KeySubscriptionPlanAwareRouting, false)
		c.Status(http.StatusOK)
	})

	for _, tc := range []struct {
		stored string
		want   bool
	}{
		{stored: `{}`},
		{stored: `{"subscription_plan_aware_routing_enabled":true}`, want: true},
		{stored: `{"subscription_plan_aware_routing_enabled":false}`},
		{stored: `{}`},
	} {
		overrides, err := flags.ParseOverrides([]byte(tc.stored))
		require.NoError(t, err)
		installation.FlagOverrides = overrides
		req := httptest.NewRequest(http.MethodGet, "/probe", nil)
		req.Header.Set(middleware.RouterKeyHeader, routerToken)
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, req)
		require.Equal(t, http.StatusOK, recorder.Code)
		assert.Equal(t, tc.want, enabled, "stored override: %s", tc.stored)
	}
}
