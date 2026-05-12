// Package cluster implements a content-aware Router based on AvengersPro
// (arxiv 2508.12631, DAI 2025): embed the prompt with Jina v2 base-code
// (768-dim, INT8-quantized ONNX), find top-p nearest centroids, and sum
// α-blended per-(cluster, model) scores to pick a deployed model.
//
// Runtime path is fully in-process. See docs/plans/archive/CLUSTER_ROUTING_PLAN.md.
package cluster

import "context"

// EmbedDim is the Jina v2 base-code output dimensionality. Pinned as a
// sanity check against shipping a centroids.bin or model.onnx with a
// disagreeing dimension.
const EmbedDim = 768

// Embedder produces an L2-normalized [EmbedDim]float32 for prompt text.
// Production impl wraps a hugot pipeline over an INT8-quantized ONNX
// export of jinaai/jina-embeddings-v2-base-code.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}
