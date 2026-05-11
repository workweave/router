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
	// EnabledProviders is the set of provider names whose credentials are
	// resolvable for this request — boot-time env keys, BYOK keys on the
	// installation, or per-request client headers. When non-nil, routers
	// must restrict argmax to providers in this set so we never return a
	// decision the upstream call would 401 on. Nil means "no per-request
	// gating; use whatever the router was constructed with."
	EnabledProviders map[string]struct{}
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
	// CandidateModels is the eligible-model set the scorer's argmax ran
	// over for this request. Captured so routing observations can record
	// what was on the table at decision time, not just what was picked.
	CandidateModels []string
	// ChosenScore is the argmax score on the chosen model — the sum of
	// rankings across the top-p clusters. Surfaced so downstream
	// analytics can grade picks against margin-of-victory.
	ChosenScore float32
	// ClusterRouterVersion is the artifact version (e.g. "v0.27") that
	// produced this decision. Empty for non-cluster routers.
	ClusterRouterVersion string
}

type Router interface {
	Route(ctx context.Context, req Request) (Decision, error)
}
