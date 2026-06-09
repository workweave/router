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
	"hash/fnv"
	"math/rand/v2"
	"sort"

	"workweave/router/internal/observability"
	"workweave/router/internal/router"
)

// ProviderForModel is a boot-time fallback resolver for a model's provider,
// used only when the decision metadata carries no per-request binding. ok is
// false when the model has no known deployment binding.
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
	// bandWidth is the score-unit half-width of the quality-tie band. A model
	// is in-band iff score >= maxScore - bandWidth. <= 0 disables exploration.
	bandWidth float32
	// intn returns a pseudo-random int in [0, n). Injected for deterministic
	// tests; defaults to the concurrency-safe top-level rand/v2 generator.
	intn func(n int) int
}

var _ router.Router = (*Explorer)(nil)

// New constructs an Explorer. A non-positive bandWidth makes the explorer a
// pure pass-through. providerFor is an optional boot-time fallback: when nil,
// the explorer relies solely on the per-request bindings in decision metadata.
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

	// Restrict the band to servable peers before sampling so the logged
	// propensity (1/|band|) is exact and a peer is only ever served via a
	// request-valid provider binding. A singleton band has no peer to explore.
	band := e.servableBand(dec)
	if len(band) < 2 {
		return dec, nil
	}

	argmaxModel := dec.Model
	pick := band[e.intn(len(band))]
	e.annotate(&dec, pick.model, pick.provider, len(band))
	// When exploration changes the served model, isolate the semantic-cache
	// key so a hit can't return another model's cached body. The cache keys on
	// EffectiveKnobsHash (among embedding/cluster/version); mixing the model in
	// only on a real switch keeps argmax-cache reuse intact when we draw it.
	if pick.model != argmaxModel && dec.Metadata != nil {
		dec.Metadata.EffectiveKnobsHash = mixModel(dec.Metadata.EffectiveKnobsHash, pick.model)
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

// servableBand returns in-band models (score >= max - bandWidth) paired with a
// request-valid provider, sorted by name for reproducible sampling. The argmax
// is always included; peers only when their provider resolves.
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
