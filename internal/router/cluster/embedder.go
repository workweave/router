// Package cluster implements a content-aware Router based on AvengersPro
// (arxiv 2508.12631, DAI 2025): embed the prompt, find top-p nearest
// centroids, and argmax α-blended per-(cluster, model) scores.
//
// Runtime path is fully in-process.
package cluster

import "context"

// EmbedDim is the Jina v2 base-code output dimensionality. Retained as
// the legacy default for bundles whose metadata.yaml lacks an embedder
// block; per-bundle dims are otherwise read from metadata.
const EmbedDim = 768

// Embedder model IDs as recorded in each bundle's metadata.yaml
// embedder.model field. The runtime refuses to score a bundle with an
// embedder whose ID differs (see NewScorer).
const (
	// EmbedderJinaV2 is the original mean-pooled BERT encoder (768d).
	EmbedderJinaV2 = "jina-v2-base-code-int8"
	// EmbedderQwen3 is the Qwen3-Embedding-0.6B export with last-token
	// pooling baked into the ONNX graph (1024d).
	EmbedderQwen3 = "qwen3-embedding-0.6b-int8"
)

// EmbedderSpec describes one loadable embedding model: its identity,
// output dimensionality, and where its assets live under the assets root.
type EmbedderSpec struct {
	ID          string
	Dim         int
	AssetSubdir string
}

// embedderSpecs is the registry of embedders the runtime knows how to
// construct, keyed by ID.
var embedderSpecs = map[string]EmbedderSpec{
	EmbedderJinaV2: {ID: EmbedderJinaV2, Dim: 768, AssetSubdir: EmbedderJinaV2},
	EmbedderQwen3:  {ID: EmbedderQwen3, Dim: 1024, AssetSubdir: EmbedderQwen3},
}

// SpecForEmbedder returns the registered spec for an embedder ID.
func SpecForEmbedder(id string) (EmbedderSpec, bool) {
	s, ok := embedderSpecs[id]
	return s, ok
}

// Embedder produces an L2-normalized [Dim()]float32 for prompt text.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	// ID is the embedder model identity, matched against the bundle's
	// metadata.yaml embedder.model at NewScorer time.
	ID() string
	// Dim is the output dimensionality, matched against centroids.bin.
	Dim() int
}
