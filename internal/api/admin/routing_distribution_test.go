package admin_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"weave-os/router/internal/api/admin"
	"weave-os/router/internal/router/cluster"
	"weave-os/router/internal/router/hmm/rosterdata"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeDistributionSource struct {
	points            []cluster.DistributionPoint
	err               error
	lastGrid          int
	lastExcludedMods  map[string]struct{}
	lastExcludedProvs map[string]struct{}
}

func (f *fakeDistributionSource) DefaultRoutingDistribution(gridN int, excludedModels, excludedProviders map[string]struct{}) ([]cluster.DistributionPoint, error) {
	f.lastGrid = gridN
	f.lastExcludedMods = excludedModels
	f.lastExcludedProvs = excludedProviders
	return f.points, f.err
}

func newDistributionEngine(src admin.RoutingDistributionSource) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/v1/router/routing-distribution", admin.RoutingDistributionHandler(src))
	return engine
}

func TestRoutingDistributionHandler_ReturnsPoints(t *testing.T) {
	src := &fakeDistributionSource{points: []cluster.DistributionPoint{
		{QualityBias: 0, Models: []cluster.ModelShare{{Model: "deepseek-v4-flash", Share: 1}}, ProjectedCostPer1KInputUSD: 0.00014},
		{QualityBias: 1, Models: []cluster.ModelShare{{Model: "claude-opus-4-8", Share: 1}}, ProjectedCostPer1KInputUSD: 0.005},
	}}
	engine := newDistributionEngine(src)

	req := httptest.NewRequest(http.MethodGet, "/v1/router/routing-distribution?grid=2", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 2, src.lastGrid, "grid query param should reach the source")

	var got struct {
		Points []cluster.DistributionPoint `json:"points"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got.Points, 2)
	assert.Equal(t, "deepseek-v4-flash", got.Points[0].Models[0].Model)
	assert.Equal(t, "claude-opus-4-8", got.Points[1].Models[0].Model)
}

func TestRoutingDistributionHandler_DefaultsGridWhenAbsent(t *testing.T) {
	src := &fakeDistributionSource{lastGrid: -999}
	engine := newDistributionEngine(src)

	req := httptest.NewRequest(http.MethodGet, "/v1/router/routing-distribution", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 0, src.lastGrid, "absent grid param should pass 0 (scorer default)")
}

func TestRoutingDistributionHandler_RejectsBadGrid(t *testing.T) {
	engine := newDistributionEngine(&fakeDistributionSource{})
	for _, bad := range []string{"1", "0", "-5", "abc", "1000"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/router/routing-distribution?grid="+bad, nil)
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "grid=%q should be rejected", bad)
	}
}

func TestRoutingDistributionHandler_ParsesExclusionParams(t *testing.T) {
	src := &fakeDistributionSource{}
	engine := newDistributionEngine(src)

	req := httptest.NewRequest(http.MethodGet, "/v1/router/routing-distribution?excluded_models=claude-opus-4-8,%20deepseek-v4-flash%20&excluded_providers=fireworks", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, map[string]struct{}{"claude-opus-4-8": {}, "deepseek-v4-flash": {}}, src.lastExcludedMods, "comma-separated models should reach the source, trimmed")
	assert.Equal(t, map[string]struct{}{"fireworks": {}}, src.lastExcludedProvs)
}

func TestRoutingDistributionHandler_NilExclusionsWhenAbsent(t *testing.T) {
	src := &fakeDistributionSource{}
	engine := newDistributionEngine(src)

	req := httptest.NewRequest(http.MethodGet, "/v1/router/routing-distribution", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Nil(t, src.lastExcludedMods, "absent excluded_models should pass nil so the scorer keeps the full roster")
	assert.Nil(t, src.lastExcludedProvs)
}

func TestRoutingDistributionHandler_SourceErrorIs503(t *testing.T) {
	engine := newDistributionEngine(&fakeDistributionSource{err: errors.New("v1 bundle")})
	req := httptest.NewRequest(http.MethodGet, "/v1/router/routing-distribution", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestRoutingDistributionHandler_EmptyPoolIs400(t *testing.T) {
	// Exclusions that empty the eligible pool are a client config error, not a
	// server outage — the handler maps ErrNoEligibleProvider to 4xx.
	engine := newDistributionEngine(&fakeDistributionSource{err: cluster.ErrNoEligibleProvider})
	req := httptest.NewRequest(http.MethodGet, "/v1/router/routing-distribution?excluded_models=a,b,c", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRoutingDistributionHandler_UsesHMMRosterForHMMStrategy(t *testing.T) {
	src := &fakeDistributionSource{err: errors.New("legacy cluster source must not run")}
	roster := &rosterdata.Roster{
		SchemaVersion: rosterdata.SchemaVersionV7,
		Ranking: rosterdata.Ranking{
			Alpha: map[string]float64{"low": 0.4}, AlphaMin: map[string]float64{"low": 0.05},
			AlphaMax: map[string]float64{"low": 0.8}, QualityBiasNeutral: 0.7,
		},
		Clusters: map[string]rosterdata.Cluster{
			"low": {
				Arms:      []string{"openai/gpt-5.6-luna"},
				ArmScores: map[string]float64{"openai/gpt-5.6-luna": 20},
				ArmIndices: map[string]rosterdata.ArmIndices{
					"openai/gpt-5.6-luna": {WII: 50, WPI: 0},
				},
			},
		},
	}
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/v1/router/routing-distribution", admin.RoutingDistributionHandler(src, roster))

	req := httptest.NewRequest(http.MethodGet, "/v1/router/routing-distribution?strategy=hmm_embedding&grid=2", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Zero(t, src.lastGrid)
}
