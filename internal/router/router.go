// Package router defines the Router interface and its Decision/Request types.
package router

import "context"

type Request struct {
	RequestedModel       string
	EstimatedInputTokens int
	HasTools             bool
	// PromptText is the concatenated user/system text, used by content-aware routers.
	PromptText string
	// EnabledProviders restricts argmax so we never return a decision the
	// upstream call would 401 on. Nil means no per-request gating.
	EnabledProviders map[string]struct{}
	// ExcludedModels drops the named models from argmax. Per-installation
	// or env-var-driven; sibling to EnabledProviders but at model granularity.
	// Nil or empty means no exclusion. If filtering empties the eligible
	// set, the scorer returns ErrNoEligibleProvider rather than falling back.
	ExcludedModels map[string]struct{}
}

type Decision struct {
	Provider string
	Model    string
	Reason   string
	// Metadata is populated by content-aware routers; nil for others.
	// Downstream consumers must nil-check before dereferencing.
	Metadata *RoutingMetadata
}

// RoutingMetadata lets downstream components reuse the embedding + cluster
// context without recomputing. Always nil-check before reading.
type RoutingMetadata struct {
	Embedding []float32
	// ClusterIDs are sorted ascending for log determinism; ClusterIDs[0]
	// is NOT necessarily the closest centroid.
	ClusterIDs []int
	// CandidateModels is the eligible-model set argmax ran over; captured
	// so observations record what was on the table, not just what was picked.
	CandidateModels []string
	// ChosenScore is the sum of rankings across top-p clusters; used for
	// margin-of-victory analytics.
	ChosenScore          float32
	ClusterRouterVersion string
}

type Router interface {
	Route(ctx context.Context, req Request) (Decision, error)
}
