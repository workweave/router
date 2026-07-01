package bandit

import (
	"context"
	"errors"
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
	if dec.Metadata.Propensity != 0.5 {
		t.Fatalf("expected propensity 0.5 for 2 candidates, got %v", dec.Metadata.Propensity)
	}
	if dec.Metadata.EffectiveKnobsHash == 99 {
		t.Fatal("model switch must perturb cache key")
	}
}

func TestRoute_KeepsArgmaxWhenSameModel(t *testing.T) {
	scores := map[string]float32{"claude-haiku-4-5": 0.9, "claude-sonnet-4-6": 0.5}
	inner := clusterDecision(scores, []int{0}, "claude-haiku-4-5", "anthropic")
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
