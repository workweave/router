package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"workweave/router/internal/api/admin"
	"workweave/router/internal/providers"
	"workweave/router/internal/router/cluster"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeDeployedModels struct {
	entries []cluster.DeployedEntry
}

func (f fakeDeployedModels) DefaultDeployedModels() []cluster.DeployedEntry { return f.entries }

func TestCatalogModelsHandler_SortsByProviderThenModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	src := fakeDeployedModels{entries: []cluster.DeployedEntry{
		{Model: "gpt-5.5", Provider: providers.ProviderOpenAI},
		{Model: "claude-opus-4-7", Provider: providers.ProviderAnthropic},
		{Model: "claude-haiku-4-5", Provider: providers.ProviderAnthropic},
		{Model: "gpt-5.4-mini", Provider: providers.ProviderOpenAI},
	}}

	engine := gin.New()
	engine.GET("/v1/router/models", admin.CatalogModelsHandler(src, nil))

	req := httptest.NewRequest(http.MethodGet, "/v1/router/models", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var got admin.CatalogModelsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))

	require.Len(t, got.Models, 4)
	assert.Equal(t, providers.ProviderAnthropic, got.Models[0].Provider)
	assert.Equal(t, "claude-haiku-4-5", got.Models[0].Model)
	assert.Equal(t, providers.ProviderAnthropic, got.Models[1].Provider)
	assert.Equal(t, "claude-opus-4-7", got.Models[1].Model)
	assert.Equal(t, providers.ProviderOpenAI, got.Models[2].Provider)
	assert.Equal(t, "gpt-5.4-mini", got.Models[2].Model)
	assert.Equal(t, providers.ProviderOpenAI, got.Models[3].Provider)
	assert.Equal(t, "gpt-5.5", got.Models[3].Model)
}

func TestCatalogModelsHandler_EmptyListReturnsEmptyArray(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.GET("/v1/router/models", admin.CatalogModelsHandler(fakeDeployedModels{}, nil))

	req := httptest.NewRequest(http.MethodGet, "/v1/router/models", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	// Empty slice must round-trip as [], not null — the Weave control plane
	// distinguishes "no models" from "missing field".
	assert.JSONEq(t, `{"models":[]}`, rec.Body.String())
}

type fakeHMMRoster struct {
	entries []cluster.DeployedEntry
	err     error
	calls   int
}

func (f *fakeHMMRoster) HMMDeployedModels(context.Context) ([]cluster.DeployedEntry, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.entries, nil
}

func TestCatalogModelsHandler_HMMStrategyReturnsRosterNotCluster(t *testing.T) {
	gin.SetMode(gin.TestMode)

	clusterSrc := fakeDeployedModels{entries: []cluster.DeployedEntry{
		{Model: "gpt-5.5", Provider: providers.ProviderOpenAI},
	}}
	hmmSrc := &fakeHMMRoster{entries: []cluster.DeployedEntry{
		{Model: "gpt-5.6-sol", Provider: providers.ProviderOpenAI},
		{Model: "claude-opus-4-8", Provider: providers.ProviderAnthropic},
	}}

	engine := gin.New()
	engine.GET("/v1/router/models", admin.CatalogModelsHandler(clusterSrc, hmmSrc))

	req := httptest.NewRequest(http.MethodGet, "/v1/router/models?strategy=hmm", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, hmmSrc.calls)

	var got admin.CatalogModelsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got.Models, 2)
	// Sorted provider-then-model: anthropic first, then openai's 5.6 — and
	// crucially the cluster's gpt-5.5 does NOT appear.
	assert.Equal(t, "claude-opus-4-8", got.Models[0].Model)
	assert.Equal(t, "gpt-5.6-sol", got.Models[1].Model)
}

func TestCatalogModelsHandler_HMMStrategyFallsBackToClusterWhenNoSource(t *testing.T) {
	gin.SetMode(gin.TestMode)

	clusterSrc := fakeDeployedModels{entries: []cluster.DeployedEntry{
		{Model: "gpt-5.5", Provider: providers.ProviderOpenAI},
	}}

	engine := gin.New()
	engine.GET("/v1/router/models", admin.CatalogModelsHandler(clusterSrc, nil))

	req := httptest.NewRequest(http.MethodGet, "/v1/router/models?strategy=hmm", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got admin.CatalogModelsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got.Models, 1)
	assert.Equal(t, "gpt-5.5", got.Models[0].Model)
}

func TestCatalogModelsHandler_HMMRosterErrorReturns503(t *testing.T) {
	gin.SetMode(gin.TestMode)

	hmmSrc := &fakeHMMRoster{err: errors.New("sidecar unavailable")}

	engine := gin.New()
	engine.GET("/v1/router/models", admin.CatalogModelsHandler(fakeDeployedModels{}, hmmSrc))

	req := httptest.NewRequest(http.MethodGet, "/v1/router/models?strategy=hmm_embedding", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
