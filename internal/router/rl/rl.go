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
	"workweave/router/internal/router/catalog"
)

// ErrPolicyUnavailable signals the RL strategy could not produce a decision
// (sidecar unreachable/errored, no eligible candidate, or an unrecognized
// selection). API handlers map it to HTTP 503, mirroring the cluster scorer's
// fail-closed contract — no silent fallback to a default model.
var ErrPolicyUnavailable = errors.New("rl: policy router unavailable")

// Candidate is one eligible model offered to the policy: the roster ID the
// policy was trained on plus the provider the router would dispatch it to.
type Candidate struct {
	RosterID string
	Provider string
}

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
	decider   Decider
	deployed  map[string]struct{}
	available map[string]struct{}
	toolLow   map[string]struct{}
	imageLow  map[string]struct{}
}

// New builds an RL Router. deployed is the set of deployable catalog model IDs
// (the same source the planner's available-models set is drawn from); available
// is the deployment's keyed-provider set (the cluster scorer's
// availableProviders), used to resolve a model's dispatch binding when the
// request does not restrict providers.
func New(decider Decider, deployed, available map[string]struct{}) *Router {
	return &Router{
		decider:   decider,
		deployed:  deployed,
		available: available,
		toolLow:   catalog.ToolUseLowSet(),
		imageLow:  catalog.ImageUnsupportedSet(),
	}
}

// eligibleCand pairs the offered roster ID with the catalog model + dispatch
// provider it maps back to.
type eligibleCand struct {
	catalogID string
	rosterID  string
	provider  string
}

// eligible builds the candidate list and a roster-ID → catalog model index for
// mapping the policy's choice back. It mirrors the cluster scorer's
// eligibility: deployed models resolvable under the request's enabled providers
// (nil = unrestricted), minus excluded models, minus image-unsupported models
// on image turns, minus ToolUseLow models on tool turns — each soft filter
// relaxed only if it would empty the pool. The index is built from the FINAL
// returned slice so a soft-filtered model can never sneak back via the
// response-mapping guard.
func (r *Router) eligible(req router.Request) ([]Candidate, map[string]candidateBinding) {
	base := make([]eligibleCand, 0, len(r.deployed))
	for id := range r.deployed {
		if req.ExcludedModels != nil {
			if _, excluded := req.ExcludedModels[id]; excluded {
				continue
			}
		}
		model, ok := catalog.ByID(id)
		if !ok {
			continue
		}
		// nil EnabledProviders means unrestricted (router.Request contract); the
		// cluster scorer's unrestricted path resolves the binding against the
		// deployment's keyed providers, not the catalog primary, so mirror that
		// by resolving against r.available. A non-nil set gates per-request.
		providerSet := req.EnabledProviders
		if providerSet == nil {
			providerSet = r.available
		}
		binding, ok := catalog.ResolveBinding(id, providerSet)
		if !ok {
			continue
		}
		base = append(base, eligibleCand{catalogID: id, rosterID: rosterIDFor(model), provider: binding.Provider})
	}

	base = r.softFilter(base, req.HasImages, r.imageLow)
	base = r.softFilter(base, req.HasTools, r.toolLow)

	candidates := make([]Candidate, 0, len(base))
	idx := make(map[string]candidateBinding, len(base))
	for _, c := range base {
		candidates = append(candidates, Candidate{RosterID: c.rosterID, Provider: c.provider})
		idx[c.rosterID] = candidateBinding{catalogID: c.catalogID, provider: c.provider}
	}
	return candidates, idx
}

// softFilter drops candidates whose catalog ID is in drop when active, but
// keeps the unfiltered pool if the filter would empty it — the same empty-pool
// fallback the cluster scorer uses for its tool-use and image filters.
func (r *Router) softFilter(in []eligibleCand, active bool, drop map[string]struct{}) []eligibleCand {
	if !active || len(drop) == 0 {
		return in
	}
	kept := make([]eligibleCand, 0, len(in))
	for _, c := range in {
		if _, bad := drop[c.catalogID]; bad {
			continue
		}
		kept = append(kept, c)
	}
	if len(kept) == 0 {
		return in
	}
	return kept
}

type candidateBinding struct {
	catalogID string
	provider  string
}

// Route asks the policy to choose among the eligible catalog candidates and
// returns the choice as a router.Decision. Failure paths return
// ErrPolicyUnavailable; the proxy never falls back to the cluster scorer.
func (r *Router) Route(ctx context.Context, req router.Request) (router.Decision, error) {
	log := observability.FromContext(ctx)
	candidates, idx := r.eligible(req)
	if len(candidates) == 0 {
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
		Candidates: candidates,
	})
	if err != nil {
		log.Error("RL router: policy decide failed; returning ErrPolicyUnavailable", "err", err)
		return router.Decision{}, fmt.Errorf("rl: policy decide: %w: %w", err, ErrPolicyUnavailable)
	}

	binding, ok := idx[res.Model]
	if !ok {
		log.Error("RL router: policy returned model not in candidate set; returning ErrPolicyUnavailable",
			"returned_model", res.Model,
		)
		return router.Decision{}, fmt.Errorf("rl: policy returned unknown model %q: %w", res.Model, ErrPolicyUnavailable)
	}

	catalogIDs := make([]string, 0, len(idx))
	for _, b := range idx {
		catalogIDs = append(catalogIDs, b.catalogID)
	}
	log.Info("RL router decided",
		"model", binding.catalogID,
		"provider", binding.provider,
		"roster_model", res.Model,
		"score", res.Score,
		"state", res.StateLabel,
		"candidate_count", len(candidates),
	)
	return router.Decision{
		Provider: binding.provider,
		Model:    binding.catalogID,
		Reason:   reasonFor(res),
		Metadata: &router.RoutingMetadata{
			CandidateModels: catalogIDs,
			ChosenScore:     float32(res.Score),
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
