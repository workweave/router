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
}

type Router interface {
	Route(ctx context.Context, req Request) (Decision, error)
}
