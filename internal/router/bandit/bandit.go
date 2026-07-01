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

// normFunc draws a standard normal variate. Injected for deterministic tests.
type normFunc func() float64

// Router wraps an inner cluster scorer and resamples its argmax via Thompson
// sampling over (cluster, model) posterior arms.
type Router struct {
	inner router.Router
	post  *Posterior
	norm  normFunc
}

// New constructs a bandit Router. post must be non-nil.
func New(inner router.Router, post *Posterior) *Router {
	return &Router{
		inner: inner,
		post:  post,
		norm:  rand.NormFloat64,
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

	type candidate struct {
		model    string
		provider string
		sample   float64
	}
	candidates := make([]candidate, 0, len(md.CandidateScores))
	for model := range md.CandidateScores {
		provider, ok := providerFor(dec, model)
		if !ok {
			continue
		}
		sample, ok := b.post.Sample(clusterIDs, model, b.norm)
		if !ok {
			// No posterior arm: fall back to the cluster scorer's blended score.
			sample = float64(md.CandidateScores[model])
		}
		candidates = append(candidates, candidate{model: model, provider: provider, sample: sample})
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
	b.annotate(&dec, pick.model, pick.provider, len(candidates), pick.sample)
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

func (b *Router) annotate(dec *router.Decision, model, provider string, n int, sample float64) {
	dec.Model = model
	dec.Provider = provider
	dec.Reason = fmt.Sprintf("bandit:ts n=%d sample=%.4g %s provider=%s base=[%s]", n, sample, model, provider, dec.Reason)
	if dec.Metadata == nil {
		return
	}
	if n >= 2 {
		dec.Metadata.Propensity = 1.0 / float32(n)
	} else {
		dec.Metadata.Propensity = 1.0
	}
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
