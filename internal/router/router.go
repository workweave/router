// Package router defines the Router interface and its Decision/Request types.
package router

import "context"

type Request struct {
	RequestedModel       string
	EstimatedInputTokens int
	HasTools             bool
	PromptText           string
	// Per-request provider gating — nil means unrestricted.
	EnabledProviders map[string]struct{}
	// Per-request model exclusion — nil or empty means no exclusion.
	// If filtering empties eligible set, scorer returns ErrNoEligibleProvider.
	ExcludedModels map[string]struct{}
}

type Decision struct {
	Provider string
	Model    string
	Reason   string
	// Nil for non-content-aware routers; nil-check before dereferencing.
	Metadata *RoutingMetadata
}

// RoutingMetadata lets downstream components reuse the embedding and
// cluster context without recomputing.
type RoutingMetadata struct {
	Embedding            []float32
	ClusterIDs           []int // Sorted ascending; [0] is NOT necessarily closest.
	CandidateModels      []string
	ChosenScore          float32
	ClusterRouterVersion string
}

type Router interface {
	Route(ctx context.Context, req Request) (Decision, error)
}
