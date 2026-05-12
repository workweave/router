package cluster

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildCentroidsBlob produces centroids.bin bytes from in-memory data,
// bypassing the committed placeholder.
func buildCentroidsBlob(t *testing.T, k, dim int, data []float32) []byte {
	t.Helper()
	require.Len(t, data, k*dim, "data must be k*dim long")
	var b bytes.Buffer
	b.WriteString(centroidsMagic)
	require.NoError(t, binary.Write(&b, binary.LittleEndian, centroidsVersion))
	require.NoError(t, binary.Write(&b, binary.LittleEndian, uint32(k)))
	require.NoError(t, binary.Write(&b, binary.LittleEndian, uint32(dim)))
	for _, v := range data {
		require.NoError(t, binary.Write(&b, binary.LittleEndian, math.Float32bits(v)))
	}
	return b.Bytes()
}

func TestLoadCentroids_Roundtrip(t *testing.T) {
	data := []float32{1, 2, 3, 4, 5, 6, 7, 8}
	// Loader rejects dim != EmbedDim; build at real dim and zero-pad.
	dim := EmbedDim
	k := 2
	full := make([]float32, k*dim)
	copy(full, data)
	blob := buildCentroidsBlob(t, k, dim, full)

	got, err := loadCentroids(blob)
	require.NoError(t, err)
	assert.Equal(t, k, got.K)
	assert.Equal(t, dim, got.Dim)
	assert.InDeltaSlice(t, full, got.Data, 1e-6)
	assert.InDeltaSlice(t, full[:dim], got.Row(0), 1e-6)
}

func TestLoadCentroids_BadMagic(t *testing.T) {
	_, err := loadCentroids([]byte("XXXX\x01\x00\x00\x00\x01\x00\x00\x00\x00\x03\x00\x00"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad magic")
}

func TestLoadCentroids_TooShort(t *testing.T) {
	_, err := loadCentroids([]byte("CRT1"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too short")
}

func TestLoadCentroids_DimMismatch(t *testing.T) {
	// dim=128 must be rejected; EmbedDim is 768.
	dim := 128
	blob := buildCentroidsBlob(t, 1, dim, make([]float32, dim))
	_, err := loadCentroids(blob)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "embedder mismatch")
}

func TestLoadCentroids_ZeroK(t *testing.T) {
	blob := buildCentroidsBlob(t, 0, EmbedDim, []float32{})
	_, err := loadCentroids(blob)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "K=0")
}

func TestLoadCentroids_SizeMismatch(t *testing.T) {
	// Header K=2 but only 1 centroid of data follows.
	var b bytes.Buffer
	b.WriteString(centroidsMagic)
	binary.Write(&b, binary.LittleEndian, centroidsVersion)
	binary.Write(&b, binary.LittleEndian, uint32(2))
	binary.Write(&b, binary.LittleEndian, uint32(EmbedDim))
	for i := 0; i < EmbedDim; i++ {
		binary.Write(&b, binary.LittleEndian, math.Float32bits(0))
	}
	_, err := loadCentroids(b.Bytes())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "size")
}

func TestLoadRankings_Roundtrip(t *testing.T) {
	raw := []byte(`{
		"meta": {"router_version": "weave-router-v0.1-bootstrap"},
		"rankings": {
			"0": {"claude-opus-4-7": 0.8, "claude-sonnet-4-5": 0.5},
			"1": {"claude-opus-4-7": 0.3, "claude-sonnet-4-5": 0.6}
		}
	}`)
	got, err := loadRankings(raw)
	require.NoError(t, err)
	assert.Len(t, got, 2)
	assert.InDelta(t, 0.8, got[0]["claude-opus-4-7"], 1e-6)
	assert.InDelta(t, 0.6, got[1]["claude-sonnet-4-5"], 1e-6)
}

func TestLoadRankings_NonIntegerKey(t *testing.T) {
	raw := []byte(`{"rankings": {"oops": {"m": 0.5}}}`)
	_, err := loadRankings(raw)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-integer")
}

func TestLoadRankings_Empty(t *testing.T) {
	_, err := loadRankings([]byte(`{"rankings": {}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no clusters")
}

func TestLoadRankings_EmptyClusterRow(t *testing.T) {
	_, err := loadRankings([]byte(`{"rankings": {"0": {}}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no models")
}

func TestLoadRegistry_Roundtrip(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"deployed_models": []any{
			map[string]any{"model": "claude-opus-4-7", "provider": "anthropic", "bench_column": "gpt-5", "proxy": true},
			map[string]any{"model": "gemini-2.5-flash", "provider": "google", "bench_column": "gemini-2.5-flash"},
		},
	})
	got, err := loadRegistry(raw)
	require.NoError(t, err)
	require.Len(t, got.DeployedModels, 2)
	assert.Equal(t, "anthropic", got.DeployedModels[0].Provider)
	assert.Equal(t, "gpt-5", got.DeployedModels[0].BenchColumn)
	assert.True(t, got.DeployedModels[0].Proxy)
	assert.Equal(t, "google", got.DeployedModels[1].Provider)
	assert.ElementsMatch(t, []string{"claude-opus-4-7", "gemini-2.5-flash"}, got.Models())
}

func TestLoadRegistry_EmptyMapping(t *testing.T) {
	_, err := loadRegistry([]byte(`{"deployed_models": []}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deployed_models is empty")
}

func TestLoadRegistry_MissingProvider(t *testing.T) {
	raw := []byte(`{"deployed_models": [{"model": "x", "bench_column": "x"}]}`)
	_, err := loadRegistry(raw)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing provider")
}

func TestLoadRegistry_MissingBenchColumn(t *testing.T) {
	raw := []byte(`{"deployed_models": [{"model": "x", "provider": "anthropic"}]}`)
	_, err := loadRegistry(raw)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing bench_column")
}

// Catches in CI the "ship one bad bundle, lose the cluster scorer
// entirely" footgun — production refuses to boot on any malformed
// committed version.
func TestEmbeddedArtifacts_AllVersionsLoadable(t *testing.T) {
	versions, err := ListVersions()
	require.NoError(t, err)
	require.NotEmpty(t, versions, "expected at least one version directory under artifacts/")
	for _, v := range versions {
		v := v
		t.Run(v, func(t *testing.T) {
			bundle, err := LoadBundle(v)
			require.NoError(t, err, "committed bundle %s must parse end-to-end", v)
			assert.Equal(t, v, bundle.Version)
			assert.NotNil(t, bundle.Centroids)
			assert.NotEmpty(t, bundle.Rankings)
			assert.NotNil(t, bundle.Registry)
			assert.NotEmpty(t, bundle.Registry.DeployedModels)
		})
	}
}

// Catches a typo'd latest pointer.
func TestResolveVersion_Latest(t *testing.T) {
	resolved, err := ResolveVersion(LatestVersion)
	require.NoError(t, err)
	assert.NotEmpty(t, resolved)
	versions, err := ListVersions()
	require.NoError(t, err)
	assert.Contains(t, versions, resolved, "latest pointer must name a committed version directory")
}

func TestResolveVersion_UnknownErrors(t *testing.T) {
	_, err := ResolveVersion("v99.99")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "v99.99")
}

func TestCheapestModel(t *testing.T) {
	meta := &ArtifactMetadata{
		CostPer1KInputUSD: map[string]float64{
			"model-a": 3.00,
			"model-b": 0.50,
			"model-c": 0.10,
		},
	}
	registry := &ModelRegistry{
		DeployedModels: []DeployedEntry{
			{Model: "model-a", Provider: "anthropic", BenchColumn: "col-a"},
			{Model: "model-b", Provider: "google", BenchColumn: "col-b"},
			{Model: "model-c", Provider: "google", BenchColumn: "col-c"},
		},
	}

	t.Run("picks cheapest across providers", func(t *testing.T) {
		available := map[string]struct{}{"anthropic": {}, "google": {}}
		p, m, ok := CheapestModel(meta, registry, available)
		require.True(t, ok)
		assert.Equal(t, "google", p)
		assert.Equal(t, "model-c", m)
	})

	t.Run("filters by available providers", func(t *testing.T) {
		available := map[string]struct{}{"anthropic": {}}
		p, m, ok := CheapestModel(meta, registry, available)
		require.True(t, ok)
		assert.Equal(t, "anthropic", p)
		assert.Equal(t, "model-a", m)
	})

	t.Run("returns false when no provider matches", func(t *testing.T) {
		available := map[string]struct{}{"openai": {}}
		_, _, ok := CheapestModel(meta, registry, available)
		assert.False(t, ok)
	})

	t.Run("skips entries with no cost annotation", func(t *testing.T) {
		metaNoCost := &ArtifactMetadata{
			CostPer1KInputUSD: map[string]float64{"model-a": 1.00},
		}
		available := map[string]struct{}{"anthropic": {}, "google": {}}
		p, m, ok := CheapestModel(metaNoCost, registry, available)
		require.True(t, ok)
		assert.Equal(t, "anthropic", p)
		assert.Equal(t, "model-a", m)
	})
}
