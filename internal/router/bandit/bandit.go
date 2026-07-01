// Package bandit is a router.Router that wraps the cluster scorer and
// Thompson-samples a model from a frozen ts_posterior.json. Opt-in via the
// x-weave-router-strategy: bandit header; OFF at boot unless
// ROUTER_BANDIT_POSTERIOR_FILE is set.
//
// Every failure path returns ErrBanditUnavailable (HTTP 503) — no silent
// fallback to the cluster argmax, mirroring the rl and cluster contracts.
package bandit

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"sort"

	"workweave/router/internal/observability"
	"workweave/router/internal/router"
)

// ErrBanditUnavailable signals the bandit strategy could not produce a
// decision (posterior missing, inner scorer failed, or no servable candidate).
var ErrBanditUnavailable = errors.New("bandit: router unavailable")

// defaultPropensityTrials is the Monte-Carlo sample count used to estimate the
// Thompson-sampling selection probability (the true propensity of the served
// action). 1024 gives a standard error <= ~0.016 on any propensity, ample for
// the 1/propensity importance weights the off-policy (IPS/DR/WDR) estimators
// consume. Overridable per-Router for deterministic tests.
const defaultPropensityTrials = 1024

// normFunc draws a standard normal variate. Injected for deterministic tests.
type normFunc func() float64

// Router wraps an inner cluster scorer and resamples its argmax via Thompson
// sampling over (cluster, model) posterior arms.
type Router struct {
	inner router.Router
	post  *Posterior
	norm  normFunc
	// trials is the Monte-Carlo sample count for the served action's true TS
	// propensity. <= 0 disables MC and logs propensity 1.0 (single realized draw).
	trials int
}

// New constructs a bandit Router. post must be non-nil.
func New(inner router.Router, post *Posterior) *Router {
	return &Router{
		inner:  inner,
		post:   post,
		norm:   rand.NormFloat64,
		trials: defaultPropensityTrials,
	}
}

// Route delegates to the inner scorer, then Thompson-samples among eligible
// candidates using the posterior keyed by the decision's cluster set.
func (b *Router) Route(ctx context.Context, req router.Request) (router.Decision, error) {
	if b == nil || b.inner == nil || b.post == nil {
		return router.Decision{}, fmt.Errorf("bandit: not configured: %w", ErrBanditUnavailable)
	}

	dec, err := b.inner.Route(ctx, req)
	if err != nil {
		return dec, err
	}
	md := dec.Metadata
	if md == nil || len(md.CandidateScores) < 2 {
		return dec, nil
	}

	clusterIDs := md.ClusterIDs
	if len(clusterIDs) == 0 {
		observability.FromContext(ctx).Warn(
			"Bandit router: decision missing cluster_ids; keeping cluster argmax",
			"model", dec.Model,
		)
		return dec, nil
	}

	candidates := make([]candidate, 0, len(md.CandidateScores))
	for model := range md.CandidateScores {
		provider, ok := providerFor(dec, model)
		if !ok {
			continue
		}
		// fallback is the cluster scorer's blended score, served verbatim when
		// the model has no posterior arm. Stored so the propensity MC replays
		// the exact same draw-or-fallback selection the live path just ran.
		fallback := float64(md.CandidateScores[model])
		sample, ok := b.post.Sample(clusterIDs, model, b.norm)
		if !ok {
			sample = fallback
		}
		candidates = append(candidates, candidate{model: model, provider: provider, sample: sample, fallback: fallback})
	}
	if len(candidates) < 2 {
		return dec, nil
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].sample != candidates[j].sample {
			return candidates[i].sample > candidates[j].sample
		}
		return candidates[i].model < candidates[j].model
	})

	argmaxModel := dec.Model
	pick := candidates[0]
	b.annotate(&dec, pick.model, pick.provider, b.propensity(clusterIDs, candidates, pick.model), pick.sample)
	if pick.model != argmaxModel && md.EffectiveKnobsHash != 0 {
		md.EffectiveKnobsHash = mixModel(md.EffectiveKnobsHash, pick.model)
	}
	return dec, nil
}

func providerFor(dec router.Decision, model string) (string, bool) {
	if dec.Metadata != nil {
		if p, ok := dec.Metadata.CandidateProviders[model]; ok && p != "" {
			return p, true
		}
	}
	if model == dec.Model && dec.Provider != "" {
		return dec.Provider, true
	}
	return "", false
}

// candidate is a servable model paired with its provider, its single realized
// Thompson draw (used for the live pick), and its blended-score fallback (used
// when the model has no posterior arm, in both the live pick and the MC).
type candidate struct {
	model    string
	provider string
	sample   float64
	fallback float64
}

// propensity Monte-Carlo estimates P(served is the Thompson-sampling argmax) —
// the true propensity of the TS policy, which the off-policy estimators use as
// the 1/propensity importance weight. It replays the exact live selection
// (per-arm Gaussian draw via Sample, blended-score fallback for arm-less
// models, model-name tie-break) `trials` times and returns the served model's
// win fraction. Floored at 1/trials so a genuine winner never logs propensity
// 0 (which would make its importance weight blow up). Returns 1.0 when MC is
// disabled or the band is a singleton.
func (b *Router) propensity(clusterIDs []int, cands []candidate, served string) float32 {
	if b.trials <= 0 || len(cands) < 2 {
		return 1.0
	}
	wins := 0
	for t := 0; t < b.trials; t++ {
		bestModel := ""
		var bestVal float64
		for _, c := range cands {
			v, ok := b.post.Sample(clusterIDs, c.model, b.norm)
			if !ok {
				v = c.fallback
			}
			if bestModel == "" || v > bestVal || (v == bestVal && c.model < bestModel) {
				bestModel, bestVal = c.model, v
			}
		}
		if bestModel == served {
			wins++
		}
	}
	if wins == 0 {
		wins = 1
	}
	return float32(wins) / float32(b.trials)
}

func (b *Router) annotate(dec *router.Decision, model, provider string, propensity float32, sample float64) {
	dec.Model = model
	dec.Provider = provider
	dec.Reason = fmt.Sprintf("bandit:ts p=%.4g sample=%.4g %s provider=%s base=[%s]", propensity, sample, model, provider, dec.Reason)
	if dec.Metadata == nil {
		return
	}
	dec.Metadata.Propensity = propensity
	if s, ok := dec.Metadata.CandidateScores[model]; ok {
		dec.Metadata.ChosenScore = s
	}
}

func mixModel(h uint64, model string) uint64 {
	f := fnv.New64a()
	var b [8]byte
	for i := 0; i < 8; i++ {
		b[i] = byte(h >> (8 * i))
	}
	_, _ = f.Write(b[:])
	_, _ = f.Write([]byte(model))
	return f.Sum64()
}

var _ router.Router = (*Router)(nil)
