// Package router defines the routing brain: the Router interface and its
// Decision/Request types. Implementations live in subpackages (heuristic, judge).
package router

import "context"

type Request struct {
	RequestedModel       string
	EstimatedInputTokens int
	HasTools             bool
	// PromptText is the concatenated user/system text from the request,
	// used by content-aware routers (e.g. RouteLLM). Empty for routers
	// that key only on token count or other features.
	PromptText string
}

type Decision struct {
	Provider string
	Model    string
	Reason   string
	// Metadata carries optional per-decision context populated by
	// content-aware routers (cluster scorer). Nil for routers that
	// don't compute it (heuristic, evalswitch passthrough); downstream
	// consumers (semantic cache, observability) must nil-check before
	// dereferencing.
	Metadata *RoutingMetadata
}

// RoutingMetadata is populated by cluster-based routers so downstream
// components can reuse the embedding + cluster context without
// recomputing it. Always nil-check before reading.
type RoutingMetadata struct {
	// Embedding is the L2-normalized prompt vector used for cluster
	// selection. Length matches the artifact's embed_dim (768 today).
	Embedding []float32
	// ClusterIDs are the top-p nearest cluster ids the scorer summed
	// over. ClusterIDs[0] is not necessarily the closest centroid:
	// the scorer sorts them ascending for log determinism, so callers
	// that care about "closest centroid" should not assume order.
	ClusterIDs []int
}

type Router interface {
	Route(ctx context.Context, req Request) (Decision, error)
}
