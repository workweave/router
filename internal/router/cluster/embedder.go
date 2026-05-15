// Package cluster implements a content-aware Router based on AvengersPro
// (arxiv 2508.12631, DAI 2025): embed the prompt, find top-p nearest
// centroids, and argmax α-blended per-(cluster, model) scores.
//
// Runtime path is fully in-process.
package cluster

import "context"

// EmbedDim is the Jina v2 base-code output dimensionality.
const EmbedDim = 768

// Embedder produces an L2-normalized [EmbedDim]float32 for prompt text.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}
