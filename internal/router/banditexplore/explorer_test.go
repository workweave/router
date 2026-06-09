package banditexplore

import (
	"context"
	"errors"
	"testing"

	"workweave/router/internal/router"
)

// fakeRouter returns a fixed decision (or error) and records nothing.
type fakeRouter struct {
	dec router.Decision
	err error
}

func (f *fakeRouter) Route(context.Context, router.Request) (router.Decision, error) {
	return f.dec, f.err
}

func clusterDecision(scores map[string]float32, model, provider string) router.Decision {
	return router.Decision{
		Provider: provider,
		Model:    model,
		Reason:   "cluster:v-test",
		Metadata: &router.RoutingMetadata{
			CandidateModels: keys(scores),
			ChosenScore:     scores[model],
			CandidateScores: scores,
			Propensity:      1.0,
		},
	}
}

func keys(m map[string]float32) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func staticProvider(m map[string]string) ProviderForModel {
	return func(model string) (string, bool) {
		p, ok := m[model]
		return p, ok
	}
}

// withIntn forces the explorer's sampler to a fixed index for determinism.
func withIntn(e *Explorer, idx int) {
	e.intn = func(int) int { return idx }
}

func TestRoute_PropagatesInnerError(t *testing.T) {
	sentinel := errors.New("boom")
	e := New(&fakeRouter{err: sentinel}, staticProvider(nil), 0.1)
	_, err := e.Route(context.Background(), router.Request{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected inner error to propagate, got %v", err)
	}
}

func TestRoute_NoExplorationWhenBandWidthZero(t *testing.T) {
	scores := map[string]float32{"haiku": 0.9, "opus": 0.89}
	inner := clusterDecision(scores, "haiku", "anthropic")
	e := New(&fakeRouter{dec: inner}, staticProvider(map[string]string{"opus": "anthropic"}), 0)
	dec, err := e.Route(context.Background(), router.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Model != "haiku" {
		t.Fatalf("bandWidth=0 must not explore; got model %q", dec.Model)
	}
	if dec.Metadata.Propensity != 1.0 {
		t.Fatalf("expected propensity 1.0, got %v", dec.Metadata.Propensity)
	}
}

func TestRoute_NilMetadataPassesThrough(t *testing.T) {
	inner := router.Decision{Provider: "anthropic", Model: "haiku", Reason: "pin"}
	e := New(&fakeRouter{dec: inner}, staticProvider(nil), 0.1)
	dec, err := e.Route(context.Background(), router.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Model != "haiku" || dec.Metadata != nil {
		t.Fatalf("non-cluster decision must pass through unchanged, got %+v", dec)
	}
}

func TestRoute_SingletonBandKeepsArgmax(t *testing.T) {
	// opus is far below the band, so the band is just {haiku}: no exploration.
	scores := map[string]float32{"haiku": 0.9, "opus": 0.5}
	inner := clusterDecision(scores, "haiku", "anthropic")
	e := New(&fakeRouter{dec: inner}, staticProvider(map[string]string{"opus": "openai"}), 0.05)
	dec, err := e.Route(context.Background(), router.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Model != "haiku" || dec.Metadata.Propensity != 1.0 {
		t.Fatalf("singleton band must keep argmax at propensity 1.0, got model=%q prop=%v", dec.Model, dec.Metadata.Propensity)
	}
}

func TestRoute_SwitchesToInBandPeerWithPropensity(t *testing.T) {
	// Band = {haiku, opus, sonnet} (all within 0.1 of max 0.9), sorted ->
	// [haiku, opus, sonnet]. Force index 1 -> opus.
	scores := map[string]float32{"haiku": 0.90, "opus": 0.85, "sonnet": 0.82}
	inner := clusterDecision(scores, "haiku", "anthropic")
	e := New(&fakeRouter{dec: inner}, staticProvider(map[string]string{
		"opus":   "anthropic",
		"sonnet": "anthropic",
	}), 0.1)
	withIntn(e, 1)
	dec, err := e.Route(context.Background(), router.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Model != "opus" {
		t.Fatalf("expected switch to opus, got %q", dec.Model)
	}
	if dec.Provider != "anthropic" {
		t.Fatalf("expected resolved provider anthropic, got %q", dec.Provider)
	}
	const wantProp = 1.0 / 3.0
	if diff := dec.Metadata.Propensity - wantProp; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("expected propensity %v, got %v", wantProp, dec.Metadata.Propensity)
	}
	if dec.Metadata.ChosenScore != 0.85 {
		t.Fatalf("ChosenScore must track the served model, got %v", dec.Metadata.ChosenScore)
	}
}

func TestRoute_UnknownProviderFallsBackToArgmax(t *testing.T) {
	scores := map[string]float32{"haiku": 0.90, "opus": 0.88}
	inner := clusterDecision(scores, "haiku", "anthropic")
	// Resolver knows nothing about opus -> must keep argmax.
	e := New(&fakeRouter{dec: inner}, staticProvider(map[string]string{}), 0.1)
	withIntn(e, 1) // would pick opus (index 1 of [haiku, opus]) if resolvable
	dec, err := e.Route(context.Background(), router.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Model != "haiku" {
		t.Fatalf("unknown provider must fall back to argmax, got %q", dec.Model)
	}
}

func TestRoute_PrefersPerRequestProviderBinding(t *testing.T) {
	// Under BYOK the scorer resolves opus to openrouter; the boot-time default
	// resolver says bedrock. The explorer must serve the per-request binding.
	scores := map[string]float32{"haiku": 0.90, "opus": 0.88}
	inner := clusterDecision(scores, "haiku", "anthropic")
	inner.Metadata.CandidateProviders = map[string]string{
		"haiku": "anthropic",
		"opus":  "openrouter",
	}
	e := New(&fakeRouter{dec: inner}, staticProvider(map[string]string{"opus": "bedrock"}), 0.1)
	withIntn(e, 1) // [haiku, opus] -> opus
	dec, err := e.Route(context.Background(), router.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Model != "opus" {
		t.Fatalf("expected switch to opus, got %q", dec.Model)
	}
	if dec.Provider != "openrouter" {
		t.Fatalf("must use per-request binding openrouter, got %q", dec.Provider)
	}
}

func TestRoute_MetadataProviderUsedWhenResolverNil(t *testing.T) {
	scores := map[string]float32{"haiku": 0.90, "opus": 0.88}
	inner := clusterDecision(scores, "haiku", "anthropic")
	inner.Metadata.CandidateProviders = map[string]string{"opus": "anthropic"}
	e := New(&fakeRouter{dec: inner}, nil, 0.1) // nil boot-time resolver
	withIntn(e, 1)
	dec, err := e.Route(context.Background(), router.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Model != "opus" || dec.Provider != "anthropic" {
		t.Fatalf("metadata binding must drive the switch, got model=%q provider=%q", dec.Model, dec.Provider)
	}
}

func TestRoute_SwitchIsolatesCacheKey(t *testing.T) {
	scores := map[string]float32{"haiku": 0.90, "opus": 0.88}
	inner := clusterDecision(scores, "haiku", "anthropic")
	inner.Metadata.EffectiveKnobsHash = 42
	e := New(&fakeRouter{dec: inner}, staticProvider(map[string]string{"opus": "anthropic"}), 0.1)
	withIntn(e, 1) // -> opus (a switch)
	dec, err := e.Route(context.Background(), router.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Model != "opus" {
		t.Fatalf("expected switch to opus, got %q", dec.Model)
	}
	if dec.Metadata.EffectiveKnobsHash == 42 {
		t.Fatal("switching models must perturb EffectiveKnobsHash to isolate the cache key")
	}
}

func TestRoute_ArgmaxDrawKeepsCacheKey(t *testing.T) {
	scores := map[string]float32{"haiku": 0.90, "opus": 0.88}
	inner := clusterDecision(scores, "haiku", "anthropic")
	inner.Metadata.EffectiveKnobsHash = 42
	e := New(&fakeRouter{dec: inner}, staticProvider(map[string]string{"opus": "anthropic"}), 0.1)
	withIntn(e, 0) // -> haiku (the argmax; no switch)
	dec, err := e.Route(context.Background(), router.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Metadata.EffectiveKnobsHash != 42 {
		t.Fatalf("drawing the argmax must preserve the cache key, got %d", dec.Metadata.EffectiveKnobsHash)
	}
}

func TestRoute_DrawingArgmaxRecordsTruePropensity(t *testing.T) {
	scores := map[string]float32{"haiku": 0.90, "opus": 0.88}
	inner := clusterDecision(scores, "haiku", "anthropic")
	e := New(&fakeRouter{dec: inner}, staticProvider(map[string]string{"opus": "anthropic"}), 0.1)
	withIntn(e, 0) // [haiku, opus] -> haiku (the argmax)
	dec, err := e.Route(context.Background(), router.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Model != "haiku" {
		t.Fatalf("expected argmax haiku, got %q", dec.Model)
	}
	if dec.Metadata.Propensity != 0.5 {
		t.Fatalf("drawing argmax in a 2-model band must log propensity 0.5, got %v", dec.Metadata.Propensity)
	}
}
