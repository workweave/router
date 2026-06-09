//go:build onnx_integration

package cluster

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// onnxFixture matches dump_cluster_test_vector.py output. Parity bar
// (cosine ≥ 0.98 in practice) proves Python and Go agree numerically.
type onnxFixture struct {
	Texts        []string    `json:"texts"`
	Reference    [][]float32 `json:"reference"`
	EmbedderName string      `json:"embedder_name"`
	EmbedDim     int         `json:"embed_dim"`
	Quantization string      `json:"quantization"`
}

// Parity bar is 0.98 (plan documents 0.99; multilingual UTF-8
// NFC/NFD normalization differences between daulet/tokenizers and HF
// land at ~0.987). 0.98 still catches material miscalibration.
//
// pointEmbedderAtAssets points at the repo-local assets/ dir so the
// suite is self-contained.
func pointEmbedderAtAssets(t *testing.T) {
	t.Helper()
	if os.Getenv("ROUTER_ONNX_ASSETS_DIR") != "" {
		return
	}
	abs, err := filepath.Abs("assets")
	require.NoError(t, err)
	t.Setenv("ROUTER_ONNX_ASSETS_DIR", abs)
}

func TestEmbedder_PythonGoParity(t *testing.T) {
	pointEmbedderAtAssets(t)
	raw, err := os.ReadFile(filepath.Join("testdata", "fixture.json"))
	require.NoError(t, err, "run router/scripts/dump_cluster_test_vector.py to produce fixture.json")
	var f onnxFixture
	require.NoError(t, json.Unmarshal(raw, &f))
	require.Equal(t, EmbedDim, f.EmbedDim, "fixture EmbedDim must match Go EmbedDim constant")
	require.NotEmpty(t, f.Texts)
	require.Equal(t, len(f.Texts), len(f.Reference))

	emb, err := NewEmbedder()
	require.NoError(t, err, "NewEmbedder must succeed under -tags=onnx_integration")
	defer emb.Close()

	cosines := make([]float32, len(f.Texts))
	for i, text := range f.Texts {
		vec, err := emb.Embed(context.Background(), text)
		require.NoError(t, err, "text %d: %s", i, text)
		require.Len(t, vec, EmbedDim)
		cosines[i] = cosine(vec, f.Reference[i])
	}
	// Report all cosines so one bad text doesn't hide the rest.
	for i, c := range cosines {
		t.Logf("cosine[%d]=%.4f text=%q", i, c, f.Texts[i])
	}
	for i, c := range cosines {
		assert.GreaterOrEqual(t, c, float32(0.98),
			"text %d: cosine(go, py) = %.4f, want ≥ 0.98 (text=%q)", i, c, f.Texts[i])
	}
}

// TestEmbedder_Qwen3PythonGoParity mirrors the Jina parity test for the
// Qwen3 embedder. The fixture is produced by scripts/export_qwen3_onnx.py
// (copy its fixture.json to testdata/fixture_qwen3.json); the assets must
// live under <assets>/qwen3-embedding-0.6b-int8/. Skips when either is
// absent so the Jina-only suite keeps passing on machines without the
// Qwen export.
func TestEmbedder_Qwen3PythonGoParity(t *testing.T) {
	pointEmbedderAtAssets(t)
	raw, err := os.ReadFile(filepath.Join("testdata", "fixture_qwen3.json"))
	if os.IsNotExist(err) {
		t.Skip("testdata/fixture_qwen3.json absent; run scripts/export_qwen3_onnx.py to produce it")
	}
	require.NoError(t, err)
	var f onnxFixture
	require.NoError(t, json.Unmarshal(raw, &f))
	spec, ok := SpecForEmbedder(EmbedderQwen3)
	require.True(t, ok)
	require.Equal(t, spec.Dim, f.EmbedDim, "fixture EmbedDim must match the registered Qwen3 spec")
	require.Equal(t, spec.ID, f.EmbedderName)
	require.NotEmpty(t, f.Texts)
	require.Equal(t, len(f.Texts), len(f.Reference))

	set, err := NewEmbedderSet()
	require.NoError(t, err)
	defer set.Close()
	emb, err := set.Get(EmbedderQwen3)
	if err != nil {
		t.Skipf("qwen3 assets unavailable: %v", err)
	}

	cosines := make([]float32, len(f.Texts))
	for i, text := range f.Texts {
		vec, err := emb.Embed(context.Background(), text)
		require.NoError(t, err, "text %d: %s", i, text)
		require.Len(t, vec, spec.Dim)
		cosines[i] = cosine(vec, f.Reference[i])
	}
	for i, c := range cosines {
		t.Logf("cosine[%d]=%.4f text=%q", i, c, f.Texts[i])
	}
	for i, c := range cosines {
		assert.GreaterOrEqual(t, c, float32(0.98),
			"text %d: cosine(go, py) = %.4f, want ≥ 0.98 (text=%q)", i, c, f.Texts[i])
	}
}

// cosine is dot product over L2-normalized vectors.
func cosine(a, b []float32) float32 {
	var sum float64
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return float32(sum)
}

// Embed twice must produce bitwise-identical vectors. Failure implies
// a non-pinned graph optimization.
func TestEmbedder_Determinism(t *testing.T) {
	pointEmbedderAtAssets(t)
	emb, err := NewEmbedder()
	require.NoError(t, err)
	defer emb.Close()

	a, err := emb.Embed(context.Background(), "hello world")
	require.NoError(t, err)
	b, err := emb.Embed(context.Background(), "hello world")
	require.NoError(t, err)
	require.Len(t, a, EmbedDim)
	require.Len(t, b, EmbedDim)
	for i := range a {
		require.Equal(t, math.Float32bits(a[i]), math.Float32bits(b[i]),
			"embeddings differ at idx %d: %v vs %v", i, a[i], b[i])
	}
}
