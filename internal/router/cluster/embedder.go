// Package cluster implements a content-aware Router based on AvengersPro
// (arxiv 2508.12631, DAI 2025): embed the prompt with Jina v2 base-code
// (768-dim, INT8-quantized ONNX), find the top-p nearest cluster
// centroids, and sum the per-(cluster, model) α-blended scores from the
// committed ranking matrix to pick a deployed Anthropic model.
//
// The runtime path is fully in-process: no Modal sidecar, no GCS, no
// network. Any error path falls open to a heuristic fallback so the
// router stays available.
//
// See router/docs/plans/archive/CLUSTER_ROUTING_PLAN.md for the full design and the
// offline scripts (router/scripts/) that produce the committed
// artifacts (centroids.bin, rankings.json, model_registry.json) plus
// the HF-Hub-hosted assets/{model.onnx, tokenizer.json} that the
// Dockerfile pulls into the runtime image at build time.
package cluster

import "context"

// EmbedDim is the output dimensionality of the Jina v2 base-code
// embedder we ship. Pinned in code as an extra sanity check against
// shipping a centroids.bin or model.onnx that disagrees on dimension.
const EmbedDim = 768

// Embedder produces an L2-normalized [EmbedDim]float32 for a string of
// prompt text. Implemented as an interface so unit tests can swap in a
// deterministic fake; the production implementation lives in
// embedder_onnx.go and wraps a hugot sentence-transformer pipeline over
// an INT8-quantized ONNX export of jinaai/jina-embeddings-v2-base-code.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}
