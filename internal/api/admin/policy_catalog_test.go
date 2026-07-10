package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workweave/router/internal/api/admin"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
	"workweave/router/internal/router/policy"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type policyCatalogRouter struct{}

func (policyCatalogRouter) Route(context.Context, router.Request) (router.Decision, error) {
	return router.Decision{}, nil
}

func TestPolicyCatalogHandlerReportsDefaultAndCapabilities(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := proxy.NewService(
		policyCatalogRouter{}, nil, nil, false, nil, nil, false, "", "", nil,
	).WithPolicyStrategy(policy.StrategySpec{
		Strategy: router.StrategyHMM,
		Router:   policyCatalogRouter{},
		Capabilities: policy.Capabilities{
			SchemaVersion:          policy.SchemaVersionV1,
			HonorsPreferredModels:  true,
			HonorsQualityPriceBias: true,
		},
	})
	engine := gin.New()
	engine.GET(
		"/v1/router/policies",
		admin.PolicyCatalogHandler(service, router.StrategyHMM),
	)

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodGet, "/v1/router/policies", nil),
	)

	require.Equal(t, http.StatusOK, recorder.Code)
	var payload struct {
		SchemaVersion   string `json:"schema_version"`
		DefaultStrategy string `json:"default_strategy"`
		Strategies      []struct {
			Strategy     string              `json:"strategy"`
			Available    bool                `json:"available"`
			Capabilities policy.Capabilities `json:"capabilities"`
		} `json:"strategies"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	assert.Equal(t, policy.SchemaVersionV1, payload.SchemaVersion)
	assert.Equal(t, "hmm", payload.DefaultStrategy)
	require.Len(t, payload.Strategies, 2)
	assert.Equal(t, "cluster", payload.Strategies[0].Strategy)
	assert.True(t, payload.Strategies[0].Capabilities.SupportsPreview)
	assert.Equal(t, "hmm", payload.Strategies[1].Strategy)
	assert.True(t, payload.Strategies[1].Available)
}
