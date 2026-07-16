// Package banditexplore wraps a router.Router with bounded exploration over
// the quality-tie band, collecting the propensity-logged trajectories an
// off-policy estimator needs. OFF by default behind an env flag until a
// bake-off proves it helps.
//
// Exploration only picks uniformly among models within `bandWidth` of the
// argmax score (near-equivalent per the scorer); models clearly below the
// band are never gambled on. The chosen model's selection probability
// (1/|band|) is recorded as Propensity, the importance weight the IPS /
// doubly-robust estimator requires.
package banditexplore

import (
	"context"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"sort"

	"workweave/router/internal/observability"
	"workweave/router/internal/router"
)

// ProviderForModel is a boot-time fallback resolver for a model's provider,
// used when decision metadata carries no per-request binding.
type ProviderForModel func(model string) (provider string, ok bool)

// bandEntry is an in-band model paired with the provider that serves it.
type bandEntry struct {
	model    string
	provider string
}

// Explorer wraps an inner Router and randomizes within the quality-tie band.
type Explorer struct {
	inner       router.Router
	providerFor ProviderForModel
	// bandWidth: a model is in-band iff score >= maxScore - bandWidth. <= 0 disables exploration.
	bandWidth float32
	// intn returns a pseudo-random int in [0, n); injected for deterministic tests.
	intn func(n int) int
}

var _ router.Router = (*Explorer)(nil)

// New constructs an Explorer. A non-positive bandWidth makes it a pure
// pass-through. providerFor is optional; when nil, only per-request bindings
// in decision metadata are used.
func New(inner router.Router, providerFor ProviderForModel, bandWidth float32) *Explorer {
	return &Explorer{
		inner:       inner,
		providerFor: providerFor,
		bandWidth:   bandWidth,
		intn:        rand.IntN,
	}
}

// Route delegates to the inner router, then may substitute an in-band peer of
// the argmax pick when exploration is enabled and safe. Otherwise returns the
// inner decision verbatim.
func (e *Explorer) Route(ctx context.Context, req router.Request) (router.Decision, error) {
	dec, err := e.inner.Route(ctx, req)
	if err != nil {
		return dec, err
	}
	if req.ForceModel != "" {
		return dec, nil
	}
	if !e.shouldExplore(dec) {
		return dec, nil
	}

	// Restrict to servable peers first so the logged propensity (1/|band|) is
	// exact and every peer has a request-valid provider binding.
	band := e.servableBand(dec)
	if len(band) < 2 {
		return dec, nil
	}

	argmaxModel := dec.Model
	pick := band[e.intn(len(band))]
	e.annotate(&dec, pick.model, pick.provider, len(band))
	// On a real model switch, mix the model into the semantic-cache key so a
	// hit can't return another model's cached body (leave it untouched when we
	// draw the argmax, to preserve argmax-cache reuse).
	if pick.model != argmaxModel && dec.Metadata != nil {
		dec.Metadata.EffectiveKnobsHash = mixModel(dec.Metadata.EffectiveKnobsHash, pick.model)
		// The scorer's runner-up was computed against the argmax and may now
		// equal the served peer; recompute it against the served model.
		e.repairBandPair(&dec.Metadata.PairedModel, &dec.Metadata.PairedProvider, &dec.Metadata.PairedScore, dec, pick.model)
	}
	return dec, nil
}

// mixModel folds a model name into the cache-isolation hash.
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

// shouldExplore gates on a positive band width and a multi-model score vector;
// provider resolvability is enforced later in servableBand.
func (e *Explorer) shouldExplore(dec router.Decision) bool {
	if e.bandWidth <= 0 {
		return false
	}
	md := dec.Metadata
	return md != nil && len(md.CandidateScores) >= 2
}

// servableBand returns in-band models paired with a request-valid provider,
// sorted by name for reproducible sampling. The argmax is always included;
// peers only when their provider resolves.
func (e *Explorer) servableBand(dec router.Decision) []bandEntry {
	scores := dec.Metadata.CandidateScores
	maxScore, ok := maxScore(scores)
	if !ok {
		return nil
	}
	threshold := maxScore - e.bandWidth
	models := make([]string, 0, len(scores))
	for m, v := range scores {
		if v >= threshold {
			models = append(models, m)
		}
	}
	sort.Strings(models)

	band := make([]bandEntry, 0, len(models))
	for _, m := range models {
		if m == dec.Model {
			band = append(band, bandEntry{model: m, provider: dec.Provider})
			continue
		}
		provider, ok := e.providerForRequest(dec, m)
		if !ok {
			observability.Get().Debug(
				"banditexplore: no provider for in-band model; excluding from band",
				"model", m,
			)
			continue
		}
		band = append(band, bandEntry{model: m, provider: provider})
	}
	return band
}

// providerForRequest prefers the per-request binding on the decision metadata
// (correct under BYOK), falling back to the boot-time providerFor resolver.
func (e *Explorer) providerForRequest(dec router.Decision, model string) (string, bool) {
	if md := dec.Metadata; md != nil {
		if p, ok := md.CandidateProviders[model]; ok && p != "" {
			return p, true
		}
	}
	if e.providerFor == nil {
		return "", false
	}
	return e.providerFor(model)
}

// maxScore returns the largest score and whether the map was non-empty.
func maxScore(scores map[string]float32) (float32, bool) {
	max := float32(0)
	found := false
	for _, v := range scores {
		if !found || v > max {
			max = v
			found = true
		}
	}
	return max, found
}

// annotate rewrites the served model/provider and records the exploration
// propensity (1/bandSize) and chosen score on the metadata (safe to mutate
// in place: the scorer allocates Metadata fresh per call).
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

// repairBandPair recomputes the band pair's runner-up against the served
// model, writing it back through the metadata pointers. Picks the
// highest-scoring servable peer other than served (ties broken by name);
// clears the pair if none has a resolvable provider.
func (e *Explorer) repairBandPair(outModel, outProvider *string, outScore *float32, dec router.Decision, served string) {
	scores := dec.Metadata.CandidateScores
	models := make([]string, 0, len(scores))
	for m := range scores {
		models = append(models, m)
	}
	sort.Strings(models)

	bestModel, bestProvider := "", ""
	var bestScore float32
	for _, m := range models {
		if m == served {
			continue
		}
		sc := scores[m]
		if bestModel != "" && sc <= bestScore {
			continue
		}
		provider, ok := e.providerForRequest(dec, m)
		if !ok {
			continue
		}
		bestModel, bestProvider, bestScore = m, provider, sc
	}
	*outModel, *outProvider, *outScore = bestModel, bestProvider, bestScore
}
