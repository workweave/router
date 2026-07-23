package bandit

import (
	"context"
	"errors"
	"math/rand/v2"
	"testing"

	"workweave/router/internal/router"
)

type fakeInner struct {
	dec router.Decision
	err error
}

func (f *fakeInner) Route(context.Context, router.Request) (router.Decision, error) {
	return f.dec, f.err
}

func clusterDecision(scores map[string]float32, clusterIDs []int, model, provider string) router.Decision {
	providers := make(map[string]string, len(scores))
	for m := range scores {
		providers[m] = provider
	}
	return router.Decision{
		Provider: provider,
		Model:    model,
		Reason:   "cluster:v-test",
		Metadata: &router.RoutingMetadata{
			ClusterIDs:         clusterIDs,
			CandidateScores:    scores,
			CandidateProviders: providers,
			ChosenScore:        scores[model],
			Propensity:         1.0,
			EffectiveKnobsHash: 99,
		},
	}
}

func TestRoute_PropagatesInnerError(t *testing.T) {
	sentinel := errors.New("boom")
	post, err := LoadPosterior("testdata/ts_posterior.json")
	if err != nil {
		t.Fatal(err)
	}
	b := New(&fakeInner{err: sentinel}, post)
	_, err = b.Route(context.Background(), router.Request{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected inner error, got %v", err)
	}
}

func TestRoute_NotConfigured(t *testing.T) {
	var b *Router
	_, err := b.Route(context.Background(), router.Request{})
	if !errors.Is(err, ErrBanditUnavailable) {
		t.Fatalf("expected ErrBanditUnavailable, got %v", err)
	}
}

func TestRoute_PassthroughWithoutMetadata(t *testing.T) {
	post, err := LoadPosterior("testdata/ts_posterior.json")
	if err != nil {
		t.Fatal(err)
	}
	inner := router.Decision{Provider: "anthropic", Model: "haiku", Reason: "pin"}
	b := New(&fakeInner{dec: inner}, post)
	dec, err := b.Route(context.Background(), router.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Model != "haiku" || dec.Metadata != nil {
		t.Fatalf("expected passthrough, got %+v", dec)
	}
}

func TestRoute_SamplesHigherPosteriorMean(t *testing.T) {
	// haiku mean 0.8 > sonnet 0.75; zero noise -> pick haiku even if argmax was sonnet.
	scores := map[string]float32{"claude-haiku-4-5": 0.5, "claude-sonnet-4-6": 0.9}
	inner := clusterDecision(scores, []int{0}, "claude-sonnet-4-6", "anthropic")
	post, err := LoadPosterior("testdata/ts_posterior.json")
	if err != nil {
		t.Fatal(err)
	}
	b := New(&fakeInner{dec: inner}, post)
	b.norm = func() float64 { return 0 }
	dec, err := b.Route(context.Background(), router.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Model != "claude-haiku-4-5" {
		t.Fatalf("expected Thompson pick haiku, got %q", dec.Model)
	}
	// With zero noise the higher-mean arm wins every MC trial, so the true TS
	// propensity is 1.0 — not the old bogus 1/n = 0.5.
	if dec.Metadata.Propensity != 1.0 {
		t.Fatalf("expected propensity 1.0 for a deterministically dominant arm, got %v", dec.Metadata.Propensity)
	}
	if dec.Metadata.EffectiveKnobsHash == 99 {
		t.Fatal("model switch must perturb cache key")
	}
}

func TestRoute_PropensityReflectsPosteriorOverlap(t *testing.T) {
	// Two arms with identical posteriors -> each is the sampled argmax ~50% of
	// the time, so the logged propensity must be ~0.5 (the true TS propensity),
	// regardless of which one the single realized draw happened to serve. A
	// naive 1/n would also read 0.5 here; the dominant-arm test above is what
	// distinguishes the fix, and this pins the estimator's calibration.
	scores := map[string]float32{"claude-haiku-4-5": 0.5, "claude-sonnet-4-6": 0.5}
	inner := clusterDecision(scores, []int{0}, "claude-haiku-4-5", "anthropic")
	post := &Posterior{cells: map[int]map[string]Arm{
		0: {
			"claude-haiku-4-5":  {Mean: 0.5, Variance: 0.04},
			"claude-sonnet-4-6": {Mean: 0.5, Variance: 0.04},
		},
	}}
	b := New(&fakeInner{dec: inner}, post)
	// Seeded generator keeps the MC estimate deterministic across runs.
	g := rand.New(rand.NewPCG(1, 2))
	b.norm = g.NormFloat64
	dec, err := b.Route(context.Background(), router.Request{})
	if err != nil {
		t.Fatal(err)
	}
	p := dec.Metadata.Propensity
	if p < 0.4 || p > 0.6 {
		t.Fatalf("expected propensity ~0.5 for symmetric arms, got %v", p)
	}
}

func TestRoute_KeepsArgmaxWhenSameModel(t *testing.T) {
	// Keeping the argmax must leave the scorer's PairedModel untouched.
	scores := map[string]float32{"claude-haiku-4-5": 0.9, "claude-sonnet-4-6": 0.5}
	inner := clusterDecision(scores, []int{0}, "claude-haiku-4-5", "anthropic")
	inner.Metadata.PairedModel = "claude-sonnet-4-6"
	inner.Metadata.PairedProvider = "anthropic"
	inner.Metadata.PairedScore = 0.5
	post, err := LoadPosterior("testdata/ts_posterior.json")
	if err != nil {
		t.Fatal(err)
	}
	b := New(&fakeInner{dec: inner}, post)
	b.norm = func() float64 { return 0 }
	dec, err := b.Route(context.Background(), router.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Model != "claude-haiku-4-5" {
		t.Fatalf("expected haiku, got %q", dec.Model)
	}
	if dec.Metadata.EffectiveKnobsHash != 99 {
		t.Fatal("keeping argmax must preserve cache key")
	}
	if dec.Metadata.PairedModel != "claude-sonnet-4-6" {
		t.Fatalf("argmax keep must preserve scorer runner-up sonnet, got %q", dec.Metadata.PairedModel)
	}
	if dec.Metadata.PairedScore != 0.5 {
		t.Fatalf("argmax keep must preserve scorer paired score, got %v", dec.Metadata.PairedScore)
	}
}

func TestRoute_RepairsBandPairWhenServingRunnerUp(t *testing.T) {
	// Serving the former runner-up must recompute PairedModel to the prior argmax.
	scores := map[string]float32{
		"claude-sonnet-4-6": 0.90,
		"claude-haiku-4-5":  0.85,
	}
	inner := clusterDecision(scores, []int{0}, "claude-sonnet-4-6", "anthropic")
	inner.Metadata.PairedModel = "claude-haiku-4-5"
	inner.Metadata.PairedProvider = "anthropic"
	inner.Metadata.PairedScore = 0.85
	post := &Posterior{cells: map[int]map[string]Arm{
		0: {
			"claude-haiku-4-5":  {Mean: 0.9, Variance: 0},
			"claude-sonnet-4-6": {Mean: 0.1, Variance: 0},
		},
	}}
	b := New(&fakeInner{dec: inner}, post)
	b.norm = func() float64 { return 0 }
	b.trials = 0

	dec, err := b.Route(context.Background(), router.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Model != "claude-haiku-4-5" {
		t.Fatalf("expected Thompson pick haiku (runner-up), got %q", dec.Model)
	}
	if dec.Metadata.PairedModel == dec.Model {
		t.Fatalf("band pair collapsed: Model and PairedModel are both %q", dec.Model)
	}
	if dec.Metadata.PairedModel != "claude-sonnet-4-6" {
		t.Fatalf("expected runner-up recomputed to sonnet, got %q", dec.Metadata.PairedModel)
	}
	if dec.Metadata.PairedProvider != "anthropic" {
		t.Fatalf("expected paired provider anthropic, got %q", dec.Metadata.PairedProvider)
	}
	if dec.Metadata.PairedScore != 0.90 {
		t.Fatalf("expected paired score 0.90, got %v", dec.Metadata.PairedScore)
	}
}

func TestRoute_UsesPerRequestProviderBinding(t *testing.T) {
	scores := map[string]float32{"claude-haiku-4-5": 0.5, "claude-sonnet-4-6": 0.9}
	inner := clusterDecision(scores, []int{0}, "claude-sonnet-4-6", "anthropic")
	inner.Metadata.CandidateProviders["claude-haiku-4-5"] = "openrouter"
	post, err := LoadPosterior("testdata/ts_posterior.json")
	if err != nil {
		t.Fatal(err)
	}
	b := New(&fakeInner{dec: inner}, post)
	b.norm = func() float64 { return 0 }
	dec, err := b.Route(context.Background(), router.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Model != "claude-haiku-4-5" || dec.Provider != "openrouter" {
		t.Fatalf("expected haiku via openrouter, got model=%q provider=%q", dec.Model, dec.Provider)
	}
}
