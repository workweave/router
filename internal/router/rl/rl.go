// Package rl is a router.Router implementation that delegates model selection
// to a trained RL/DPO policy served by an out-of-process policy sidecar
// (ml_dev/rl_router/router_policy_server.py, deployed as the router-rl-sidecar
// Cloud Run service).
//
// The sidecar only answers "which eligible model should we route to?": this
// package builds the candidate set from the router's own catalog (so dispatch
// stays on Weave's providers, never OpenRouter), asks the policy to pick one,
// and maps the choice back to a router.Decision. Auth, request translation,
// provider dispatch, failover, and telemetry all stay in proxy.Service exactly
// as for the cluster scorer.
//
// It is opt-in via the x-weave-router-strategy: rl header and is never a silent
// fallback — every failure path returns ErrPolicyUnavailable so the API layer
// maps it to HTTP 503 (the same contract as cluster.ErrClusterUnavailable),
// rather than masking a regression by quietly serving a default model.
package rl

import (
	"context"
	"errors"
	"fmt"

	"workweave/router/internal/observability"
	"workweave/router/internal/router"
	"workweave/router/internal/router/policy"
)

// ErrPolicyUnavailable signals the RL strategy could not produce a decision
// (sidecar unreachable/errored, no eligible candidate, or an unrecognized
// selection). API handlers map it to HTTP 503, mirroring the cluster scorer's
// fail-closed contract — no silent fallback to a default model.
var ErrPolicyUnavailable = errors.New("rl: policy router unavailable")

// Candidate is one eligible model offered to the policy: the roster ID the
// policy was trained on plus the provider the router would dispatch it to.
type Candidate = policy.Candidate

// Query is the decision request handed to a Decider.
type Query struct {
	PromptText string
	TurnIndex  int
	Candidates []Candidate
}

// Result is the policy's selection. Model echoes one of the Query's
// Candidate.RosterID values; the score/label/state fields are informational
// and surface in the routing reason.
type Result struct {
	Model      string
	Score      float64
	ScoreLabel string
	Reason     string
	StateLabel string
}

// Decider asks the out-of-process policy which candidate to route to. The HTTP
// implementation lives in client.go; tests inject a fake.
type Decider interface {
	Decide(ctx context.Context, q Query) (Result, error)
}

// Router selects a model via the RL policy and returns a router.Decision whose
// provider is resolved from the catalog so dispatch uses Weave's own providers.
type Router struct {
	decider  Decider
	resolver *policy.Resolver
}

// New builds an RL Router. deployed is the set of deployable catalog model IDs
// (the same source the planner's available-models set is drawn from); available
// is the deployment's keyed-provider set (the cluster scorer's
// availableProviders), used to resolve a model's dispatch binding when the
// request does not restrict providers.
func New(decider Decider, deployed, available map[string]struct{}) *Router {
	return &Router{
		decider:  decider,
		resolver: policy.NewResolver(deployed, available, rosterIDFor, policy.ManagedProviderPolicy()),
	}
}

// Route asks the policy to choose among the eligible catalog candidates and
// returns the choice as a router.Decision. Failure paths return
// ErrPolicyUnavailable; the proxy never falls back to the cluster scorer.
func (r *Router) Route(ctx context.Context, req router.Request) (router.Decision, error) {
	log := observability.FromContext(ctx)
	resolved := r.resolver.Resolve(req)
	if len(resolved.Candidates) == 0 {
		log.Warn("RL router: no eligible candidate for request; returning ErrPolicyUnavailable",
			"requested_model", req.RequestedModel,
			"has_tools", req.HasTools,
		)
		return router.Decision{}, fmt.Errorf("rl: no eligible candidate: %w", ErrPolicyUnavailable)
	}

	// TurnIndex is always 0: router.Request carries no turn index (the proxy
	// classifies turn TYPE but not depth), so we cannot populate it server-side
	// without new plumbing. The policy's scoring is dominated by the prompt
	// embedding; turn index is a minor feature. Threading a real index through
	// router.Request is a deliberate follow-up, not done here.
	res, err := r.decider.Decide(ctx, Query{
		PromptText: req.PromptText,
		TurnIndex:  0,
		Candidates: resolved.Candidates,
	})
	if err != nil {
		log.Error("RL router: policy decide failed; returning ErrPolicyUnavailable", "err", err)
		return router.Decision{}, fmt.Errorf("rl: policy decide: %w: %w", err, ErrPolicyUnavailable)
	}

	binding, ok := resolved.ByRosterID[res.Model]
	if !ok {
		log.Error("RL router: policy returned model not in candidate set; returning ErrPolicyUnavailable",
			"returned_model", res.Model,
		)
		return router.Decision{}, fmt.Errorf("rl: policy returned unknown model %q: %w", res.Model, ErrPolicyUnavailable)
	}

	log.Info("RL router decided",
		"model", binding.CatalogID,
		"provider", binding.Provider,
		"roster_model", res.Model,
		"score", res.Score,
		"state", res.StateLabel,
		"candidate_count", len(resolved.Candidates),
	)
	return router.Decision{
		Provider: binding.Provider,
		Model:    binding.CatalogID,
		Reason:   reasonFor(res),
		Metadata: &router.RoutingMetadata{
			CandidateModels:    resolved.CandidateModels(),
			CandidateProviders: resolved.CandidateProviders(),
			ChosenScore:        float32(res.Score),
			Strategy:           string(router.StrategyRL),
			Propensity:         1,
		},
	}, nil
}

// reasonFor renders a compact routing reason from the policy result, e.g.
// "rl_policy(DPO score=1.83; state=implementing a fix)".
func reasonFor(res Result) string {
	label := res.ScoreLabel
	if label == "" {
		label = "score"
	}
	reason := fmt.Sprintf("rl_policy(%s=%.3g", label, res.Score)
	if res.StateLabel != "" {
		reason += "; state=" + res.StateLabel
	}
	return reason + ")"
}

var _ router.Router = (*Router)(nil)
