package cluster

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/router/catalog"
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

func TestLoadCentroids_ArbitraryDimAccepted(t *testing.T) {
	// Dim is per-bundle now; loadCentroids accepts any non-zero dim and
	// validateDeclaredDim cross-checks it against metadata.
	dim := 1024
	blob := buildCentroidsBlob(t, 1, dim, make([]float32, dim))
	got, err := loadCentroids(blob)
	require.NoError(t, err)
	assert.Equal(t, dim, got.Dim)
}

func TestValidateDeclaredDim(t *testing.T) {
	t.Run("legacy bundle without metadata must match Jina default", func(t *testing.T) {
		c := &Centroids{K: 1, Dim: EmbedDim}
		require.NoError(t, validateDeclaredDim("v-test", c, nil))

		bad := &Centroids{K: 1, Dim: 1024}
		err := validateDeclaredDim("v-test", bad, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "embedder mismatch")
	})

	t.Run("declared dim must match centroids header", func(t *testing.T) {
		meta := &ArtifactMetadata{Embedder: ArtifactEmbedder{Model: EmbedderQwen3, EmbedDim: 1024}}
		require.NoError(t, validateDeclaredDim("v-test", &Centroids{K: 1, Dim: 1024}, meta))

		err := validateDeclaredDim("v-test", &Centroids{K: 1, Dim: EmbedDim}, meta)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "embedder mismatch")
	})
}

func TestBundleEmbedderDefaults(t *testing.T) {
	t.Run("nil metadata defaults to Jina", func(t *testing.T) {
		b := &Bundle{}
		assert.Equal(t, EmbedderJinaV2, b.EmbedderID())
		assert.Equal(t, EmbedDim, b.EmbedDim())
	})

	t.Run("metadata embedder block wins", func(t *testing.T) {
		b := &Bundle{Metadata: &ArtifactMetadata{
			Embedder: ArtifactEmbedder{Model: EmbedderQwen3, EmbedDim: 1024},
		}}
		assert.Equal(t, EmbedderQwen3, b.EmbedderID())
		assert.Equal(t, 1024, b.EmbedDim())
	})
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

// Guards against shipping one bad bundle that boots production with no
// cluster scorer at all.
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
			if bundle.IsV2 {
				assert.NotEmpty(t, bundle.QualityMeans)
			} else {
				assert.NotEmpty(t, bundle.Rankings)
			}
			assert.NotNil(t, bundle.Registry)
			assert.NotEmpty(t, bundle.Registry.DeployedModels)
		})
	}
}

// Enforces one deployed model per family on the latest bundle only; frozen
// historical bundles intentionally retain predecessors (BYOK, /force-model).
func TestLatestBundle_OneDeployedModelPerFamily(t *testing.T) {
	version, err := ResolveVersion(LatestVersion)
	require.NoError(t, err)
	bundle, err := LoadBundle(version)
	require.NoError(t, err)

	dups := catalog.FamilyDuplicates(bundle.Registry.Models())
	if len(dups) == 0 {
		return
	}
	msg := fmt.Sprintf("latest bundle %s deploys more than one model per family — see docs/plans/ROUTER_MODEL_LIFECYCLE.md and the add-router-model skill for how to retire a superseded model from deployed_models:", version)
	for _, d := range dups {
		msg += "\n  - " + d.String()
	}
	t.Error(msg)
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

// ListVersions must surface bundles in artifacts/ AND artifacts/legacy/,
// flattened into the same list, without leaking the "legacy" pseudo-name.
func TestListVersions_FlattensLegacyAndOmitsPseudoName(t *testing.T) {
	versions, err := ListVersions()
	require.NoError(t, err)
	require.NotEmpty(t, versions)
	for _, v := range versions {
		assert.NotEqual(t, "legacy", v, "the legacy subdirectory must not appear as a version")
	}
	// Sanity: at least one known legacy version is reachable.
	assert.Contains(t, versions, "v0.21", "legacy v0.21 must remain reachable after the move")
}

// bundleDirForVersion must resolve legacy bundles transparently.
func TestResolveVersion_LegacyBundleIsReachable(t *testing.T) {
	resolved, err := ResolveVersion("v0.21")
	require.NoError(t, err)
	assert.Equal(t, "v0.21", resolved)
	bundle, err := LoadBundle(resolved)
	require.NoError(t, err)
	assert.False(t, bundle.IsV2, "v0.21 is v1 format")
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

func TestCheapestModelInSet(t *testing.T) {
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
	available := map[string]struct{}{"anthropic": {}, "google": {}}

	t.Run("respects allowSet — cheapest within allow", func(t *testing.T) {
		allow := map[string]struct{}{"model-a": {}, "model-b": {}}
		p, m, ok := CheapestModelInSet(meta, registry, available, nil, allow)
		require.True(t, ok)
		assert.Equal(t, "google", p)
		assert.Equal(t, "model-b", m, "model-c is cheaper but excluded by allowSet")
	})

	t.Run("ok=false when allowSet has no available model", func(t *testing.T) {
		allow := map[string]struct{}{"nope": {}}
		_, _, ok := CheapestModelInSet(meta, registry, available, nil, allow)
		assert.False(t, ok)
	})

	t.Run("nil allowSet behaves like CheapestModel", func(t *testing.T) {
		p, m, ok := CheapestModelInSet(meta, registry, available, nil, nil)
		require.True(t, ok)
		assert.Equal(t, "google", p)
		assert.Equal(t, "model-c", m)
	})

	t.Run("denySet excludes models even when allowed by allowSet", func(t *testing.T) {
		// Guards PR #100: tier clamping bypassed the request's denylist.
		allow := map[string]struct{}{"model-b": {}, "model-c": {}}
		deny := map[string]struct{}{"model-c": {}}
		p, m, ok := CheapestModelInSet(meta, registry, available, deny, allow)
		require.True(t, ok)
		assert.Equal(t, "google", p)
		assert.Equal(t, "model-b", m, "model-c is cheaper but denylisted")
	})

	t.Run("denySet emptying pool yields ok=false", func(t *testing.T) {
		deny := map[string]struct{}{"model-a": {}, "model-b": {}, "model-c": {}}
		_, _, ok := CheapestModelInSet(meta, registry, available, deny, nil)
		assert.False(t, ok)
	})
}

func TestFastestModel(t *testing.T) {
	// model-c is cheapest but slowest; model-a is fastest. A cost-only
	// selector returns model-c — FastestModel must return model-a.
	meta := &ArtifactMetadata{
		CostPer1KInputUSD: map[string]float64{
			"model-a": 3.00,
			"model-b": 0.50,
			"model-c": 0.10,
		},
		TokPerS: map[string]map[string]float64{
			"anthropic": {"model-a": 150.0},
			"google":    {"model-b": 80.0, "model-c": 20.0},
		},
	}
	registry := &ModelRegistry{
		DeployedModels: []DeployedEntry{
			{Model: "model-a", Provider: "anthropic", BenchColumn: "col-a"},
			{Model: "model-b", Provider: "google", BenchColumn: "col-b"},
			{Model: "model-c", Provider: "google", BenchColumn: "col-c"},
		},
	}

	t.Run("picks fastest across providers, not cheapest", func(t *testing.T) {
		available := map[string]struct{}{"anthropic": {}, "google": {}}
		p, m, ok := FastestModel(meta, registry, available)
		require.True(t, ok)
		assert.Equal(t, "anthropic", p)
		assert.Equal(t, "model-a", m, "model-c is cheapest but slowest")
	})

	t.Run("speed is provider-keyed", func(t *testing.T) {
		// Only google available: model-a's anthropic speed is unreachable,
		// so the fastest reachable model is model-b (80) over model-c (20).
		available := map[string]struct{}{"google": {}}
		p, m, ok := FastestModel(meta, registry, available)
		require.True(t, ok)
		assert.Equal(t, "google", p)
		assert.Equal(t, "model-b", m)
	})

	t.Run("falls back to cheapest when bundle has no tok_per_s", func(t *testing.T) {
		metaNoSpeed := &ArtifactMetadata{CostPer1KInputUSD: meta.CostPer1KInputUSD}
		available := map[string]struct{}{"anthropic": {}, "google": {}}
		p, m, ok := FastestModel(metaNoSpeed, registry, available)
		require.True(t, ok)
		assert.Equal(t, "google", p)
		assert.Equal(t, "model-c", m, "no speed data → cost-only cheapest")
	})

	t.Run("ok=false when no provider matches", func(t *testing.T) {
		available := map[string]struct{}{"openai": {}}
		_, _, ok := FastestModel(meta, registry, available)
		assert.False(t, ok)
	})
}

func TestFastestModelInSet(t *testing.T) {
	meta := &ArtifactMetadata{
		CostPer1KInputUSD: map[string]float64{
			"flash-lite": 0.20, // cheapest-ish, fastest
			"v4-flash":   0.10, // cheapest, slowest
			"mid-model":  1.00,
		},
		TokPerS: map[string]map[string]float64{
			"google":    {"flash-lite": 158.0, "mid-model": 35.0},
			"deepinfra": {"v4-flash": 24.0},
		},
	}
	registry := &ModelRegistry{
		DeployedModels: []DeployedEntry{
			{Model: "flash-lite", Provider: "google", BenchColumn: "col-fl"},
			{Model: "v4-flash", Provider: "deepinfra", BenchColumn: "col-vf"},
			{Model: "mid-model", Provider: "google", BenchColumn: "col-mid"},
		},
	}
	available := map[string]struct{}{"google": {}, "deepinfra": {}}

	t.Run("low-tier clamp prefers fast flash-lite over cheap v4-flash", func(t *testing.T) {
		// Mirrors the real haiku-tier clamp, where cost-only picks the slow v4-flash.
		allow := map[string]struct{}{"flash-lite": {}, "v4-flash": {}}
		p, m, ok := FastestModelInSet(meta, registry, available, nil, allow)
		require.True(t, ok)
		assert.Equal(t, "google", p)
		assert.Equal(t, "flash-lite", m)
	})

	t.Run("falls back to cheapest within allowSet when none annotated", func(t *testing.T) {
		metaNoSpeed := &ArtifactMetadata{CostPer1KInputUSD: meta.CostPer1KInputUSD}
		allow := map[string]struct{}{"flash-lite": {}, "v4-flash": {}}
		p, m, ok := FastestModelInSet(metaNoSpeed, registry, available, nil, allow)
		require.True(t, ok)
		assert.Equal(t, "deepinfra", p)
		assert.Equal(t, "v4-flash", m, "no speed → cheapest in allowSet")
	})

	t.Run("denySet excludes the fastest, falls to next fastest", func(t *testing.T) {
		allow := map[string]struct{}{"flash-lite": {}, "mid-model": {}}
		deny := map[string]struct{}{"flash-lite": {}}
		p, m, ok := FastestModelInSet(meta, registry, available, deny, allow)
		require.True(t, ok)
		assert.Equal(t, "google", p)
		assert.Equal(t, "mid-model", m, "flash-lite faster but denylisted")
	})

	t.Run("ok=false when allowSet has no available model", func(t *testing.T) {
		allow := map[string]struct{}{"nope": {}}
		_, _, ok := FastestModelInSet(meta, registry, available, nil, allow)
		assert.False(t, ok)
	})
}

// Pins the fix on the real bundle: low-tier clamp must pick the fastest
// annotated model, not the cheapest slow incumbent.
func TestFastestModel_RealLatestBundle_LowTierPrefersFastFlash(t *testing.T) {
	version, err := ResolveVersion(LatestVersion)
	require.NoError(t, err)
	bundle, err := LoadBundle(version)
	require.NoError(t, err)
	if len(bundle.Metadata.TokPerS) == 0 {
		t.Skip("latest bundle carries no tok_per_s annotations yet")
	}
	available := make(map[string]struct{}, len(bundle.Metadata.DeployedProviders))
	for _, p := range bundle.Metadata.DeployedProviders {
		available[p] = struct{}{}
	}
	allow := catalog.AllowedAtOrBelow(catalog.TierLow)

	fastP, fastM, ok := FastestModelInSet(bundle.Metadata, bundle.Registry, available, nil, allow)
	require.True(t, ok)
	_, cheapM, ok := CheapestModelInSet(bundle.Metadata, bundle.Registry, available, nil, allow)
	require.True(t, ok)

	assert.Equal(t, "gemini-3.1-flash-lite-preview", fastM, "fastest low-tier model")
	assert.Equal(t, "google", fastP)
	assert.Equal(t, "deepseek/deepseek-v4-flash", cheapM, "cheapest low-tier model (the slow incumbent)")
	assert.NotEqual(t, cheapM, fastM, "fastest must diverge from cheapest on the low-tier clamp")
}
