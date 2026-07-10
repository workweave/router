// Package hmm delegates model selection to a policy sidecar.
//
// The router builds the eligible candidate set from its catalog, delegates the
// choice, and dispatches through its normal provider machinery.
package hmm

import (
	"errors"
	"strings"

	"workweave/router/internal/router"
	"workweave/router/internal/router/policy"
)

var ErrHMMUnavailable = errors.New("hmm: policy router unavailable")

type Candidate = policy.Candidate

type Query = policy.Query
type Result = policy.Result
type Decider = policy.Decider
type OutcomeReporter = policy.OutcomeReporter
type FeedbackReporter = policy.FeedbackReporter

type Router struct {
	*policy.SidecarRouter
	resolver *policy.Resolver
}

func New(decider Decider, deployed, available map[string]struct{}) *Router {
	resolver := policy.NewResolver(deployed, available, rosterIDFor, policy.ManagedProviderPolicy())
	return &Router{
		SidecarRouter: policy.NewSidecarRouter(policy.SidecarRouterConfig{
			Strategy:    router.StrategyHMM,
			Unavailable: ErrHMMUnavailable,
			Reason:      reasonFor,
		}, decider, resolver),
		resolver: resolver,
	}
}

func reasonFor(res Result) string {
	prefix := "hmm_policy"
	if isToolExecutionResult(res) {
		prefix = "hmm_policy:tool_execution"
	}
	if res.Reason != "" {
		return prefix + "(" + res.Reason + ")"
	}
	if res.PolicyLabel != "" {
		return prefix + "(label=" + res.PolicyLabel + ")"
	}
	return prefix
}

func isToolExecutionResult(res Result) bool {
	group := strings.TrimSpace(strings.ToLower(res.PolicyGroup))
	if group == "explore" {
		return true
	}
	label := strings.TrimSpace(strings.ToLower(res.PolicyLabel))
	return label == "spawn_explore" || strings.Contains(label, "tool_call")
}

var _ router.Router = (*Router)(nil)
var _ OutcomeReporter = (*Router)(nil)
