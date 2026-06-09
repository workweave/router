// Package banditexplore wraps a content-aware router.Router with bounded
// exploration over the quality-tie band, for collecting the propensity-logged
// trajectories an off-policy estimator (and, later, a contextual bandit)
// needs. It is OFF by default and ships behind an env flag — prod keeps
// serving the wrapped router's deterministic argmax until a bake-off proves
// exploration helps.
//
// The exploration rule is deliberately the lowest-risk one: among the models
// whose blended score is within `bandWidth` of the argmax (the "quality-tie
// band" — models the scorer considers near-equivalent), pick uniformly at
// random. Models clearly below the band are never touched, so a hard query is
// never gambled on an inferior model. The probability the chosen model was
// selected (1/|band|) is recorded as the decision's Propensity, which is the
// importance weight Phase 2's IPS / doubly-robust estimator requires.
//
// This package depends only on the inner `router` interface and a provider
// resolver injected at construction — it knows nothing about clustering,
// artifacts, or the proxy. The wrapped router is unchanged; this is pure
// composition.
package banditexplore

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"

	"workweave/router/internal/observability"
	"workweave/router/internal/router"
)

// ProviderForModel resolves the upstream provider for a candidate model name.
// ok is false when the model has no known deployment binding, in which case
// the explorer declines to switch to it (falling back to the argmax pick).
type ProviderForModel func(model string) (provider string, ok bool)

// Explorer wraps an inner Router and randomizes within the quality-tie band.
type Explorer struct {
	inner       router.Router
	providerFor ProviderForModel
	// bandWidth is the score-unit half-width of the quality-tie band. A model
	// is in-band iff score >= maxScore - bandWidth. <= 0 disables exploration.
	bandWidth float32
	// intn returns a pseudo-random int in [0, n). Injected for deterministic
	// tests; defaults to the concurrency-safe top-level rand/v2 generator.
	intn func(n int) int
}

var _ router.Router = (*Explorer)(nil)

// New constructs an Explorer. providerFor must be non-nil; a nil resolver or a
// non-positive bandWidth makes the explorer a pure pass-through.
func New(inner router.Router, providerFor ProviderForModel, bandWidth float32) *Explorer {
	return &Explorer{
		inner:       inner,
		providerFor: providerFor,
		bandWidth:   bandWidth,
		intn:        rand.IntN,
	}
}

// Route delegates to the inner router, then — when exploration is enabled and
// the decision exposes a multi-model score vector — may substitute an in-band
// peer of the argmax pick. Any condition that makes exploration unsafe or
// undefined returns the inner decision verbatim.
func (e *Explorer) Route(ctx context.Context, req router.Request) (router.Decision, error) {
	dec, err := e.inner.Route(ctx, req)
	if err != nil {
		return dec, err
	}
	if !e.shouldExplore(dec) {
		return dec, nil
	}

	band := e.tieBand(dec.Metadata.CandidateScores)
	// A singleton band means the argmax has no near-equivalent peer; there is
	// nothing to explore, so the deterministic pick stands (propensity 1.0).
	if len(band) < 2 {
		return dec, nil
	}

	chosen := band[e.intn(len(band))]
	if chosen == dec.Model {
		// Drew the argmax model itself: still record the true propensity
		// (1/|band|) so the logged decision is honest about the policy that
		// produced it, but no model/provider substitution is needed.
		e.annotate(&dec, dec.Model, dec.Provider, len(band))
		return dec, nil
	}

	provider, ok := e.providerFor(chosen)
	if !ok {
		// Unknown deployment binding — declining to switch keeps us from
		// routing to a provider the request may not have enabled.
		observability.Get().Debug(
			"banditexplore: no provider for in-band model; keeping argmax",
			"model", chosen,
		)
		return dec, nil
	}

	e.annotate(&dec, chosen, provider, len(band))
	return dec, nil
}

// shouldExplore gates exploration on a positive band width, a usable provider
// resolver, and a decision carrying a multi-model score vector.
func (e *Explorer) shouldExplore(dec router.Decision) bool {
	if e.bandWidth <= 0 || e.providerFor == nil {
		return false
	}
	md := dec.Metadata
	return md != nil && len(md.CandidateScores) >= 2
}

// tieBand returns the in-band models (score >= max - bandWidth), sorted by name
// so sampling is reproducible given a fixed intn.
func (e *Explorer) tieBand(scores map[string]float32) []string {
	maxScore := float32(0)
	first := true
	for _, v := range scores {
		if first || v > maxScore {
			maxScore = v
			first = false
		}
	}
	threshold := maxScore - e.bandWidth
	band := make([]string, 0, len(scores))
	for m, v := range scores {
		if v >= threshold {
			band = append(band, m)
		}
	}
	sort.Strings(band)
	return band
}

// annotate rewrites the served model/provider and records the exploration
// propensity (1/bandSize) and the chosen model's score on the metadata. The
// scorer allocates Metadata fresh per call, so in-place mutation is safe.
func (e *Explorer) annotate(dec *router.Decision, model, provider string, bandSize int) {
	dec.Model = model
	dec.Provider = provider
	dec.Reason = fmt.Sprintf("explore:band=%d %s provider=%s base=[%s]", bandSize, model, provider, dec.Reason)
	if dec.Metadata != nil {
		dec.Metadata.Propensity = 1.0 / float32(bandSize)
		if s, ok := dec.Metadata.CandidateScores[model]; ok {
			dec.Metadata.ChosenScore = s
		}
	}
}
