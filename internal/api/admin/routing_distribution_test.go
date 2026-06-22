package admin_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"workweave/router/internal/api/admin"
	"workweave/router/internal/router/cluster"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeDistributionSource struct {
	points   []cluster.DistributionPoint
	err      error
	lastGrid int
}

func (f *fakeDistributionSource) DefaultRoutingDistribution(gridN int) ([]cluster.DistributionPoint, error) {
	f.lastGrid = gridN
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

func TestRoutingDistributionHandler_SourceErrorIs503(t *testing.T) {
	engine := newDistributionEngine(&fakeDistributionSource{err: errors.New("v1 bundle")})
	req := httptest.NewRequest(http.MethodGet, "/v1/router/routing-distribution", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
