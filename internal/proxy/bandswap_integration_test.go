package proxy

import (
	"context"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/bandswap"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/router/turntype"
)

// orderBandPair must put the stronger-tier model in `large` regardless of which
// half of the pin it is stored as.
func TestOrderBandPair_ByTier(t *testing.T) {
	want := func(large, small router.Decision, wantLarge, wantSmall string) {
		t.Helper()
		if large.Model != wantLarge || small.Model != wantSmall {
			t.Fatalf("large=%q small=%q, want large=%q small=%q", large.Model, small.Model, wantLarge, wantSmall)
		}
	}

	anchorLarge := sessionpin.Pin{
		Provider: "anthropic", Model: "claude-opus-4-7",
		PairedProvider: "anthropic", PairedModel: "claude-haiku-4-5",
	}
	l, s := orderBandPair(anchorLarge)
	want(l, s, "claude-opus-4-7", "claude-haiku-4-5")

	// Same pair, anchor and runner-up swapped -> identical large/small split.
	anchorSmall := sessionpin.Pin{
		Provider: "anthropic", Model: "claude-haiku-4-5",
		PairedProvider: "anthropic", PairedModel: "claude-opus-4-7",
	}
	l, s = orderBandPair(anchorSmall)
	want(l, s, "claude-opus-4-7", "claude-haiku-4-5")
}

// With the swap head disabled the sticky turn must serve the pin's anchor.
func TestBandSwapServed_DisabledServesAnchor(t *testing.T) {
	s := &Service{} // bandSwap nil
	pin := sessionpin.Pin{
		Provider: "anthropic", Model: "claude-opus-4-7",
		PairedProvider: "anthropic", PairedModel: "claude-haiku-4-5", Reason: "cluster",
	}
	got := s.bandSwapServed(context.Background(), turntype.MainLoop, pin, router.Decision{}, false, nil, nil)
	if got.Model != "claude-opus-4-7" {
		t.Fatalf("served %q, want anchor claude-opus-4-7", got.Model)
	}
}

// A pin with no runner-up can never swap, even if the head were enabled.
func TestBandSwapServed_NoPairServesAnchor(t *testing.T) {
	s := &Service{}
	pin := sessionpin.Pin{Provider: "anthropic", Model: "claude-opus-4-7", Reason: "cluster"}
	got := s.bandSwapServed(context.Background(), turntype.MainLoop, pin, router.Decision{}, false, nil, nil)
	if got.Model != "claude-opus-4-7" {
		t.Fatalf("served %q, want anchor", got.Model)
	}
}

// When the head picks the paired model but that model is unservable this turn
// — excluded by the context-window pre-filter, or bound to a provider the
// request can't use — the swap must fall back to the anchor rather than emit a
// decision that would fail downstream. Mirrors turnloop's sticky-pin guards.
func TestBandSwapServed_UnservableChoiceFallsBackToAnchor(t *testing.T) {
	clf, err := bandswap.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	emb := make([]float32, bandswap.EmbedDim)
	for i := range emb {
		emb[i] = 0.01
	}
	_, band, ok := clf.PredictBand(emb)
	if !ok {
		t.Fatal("PredictBand not ok for valid-width embedding")
	}

	// opus is the LARGE-tier member, haiku the SMALL-tier one, so orderBandPair
	// is deterministic. Anchor the pin on whichever member the head would NOT
	// pick, so honoring the head is a real swap away from the anchor.
	const large, small = "claude-opus-4-7", "claude-haiku-4-5"
	served := large
	if band == bandswap.Small {
		served = small
	}
	anchor, paired := small, large
	if served == small {
		anchor, paired = large, small
	}

	s := &Service{
		embedOnlyUserMessage: true,
		bandSwap:             clf,
		availableModels:      map[string]struct{}{large: {}, small: {}},
		providers:            map[string]providers.Client{"anthropic": nil},
	}
	pin := sessionpin.Pin{
		Provider: "anthropic", Model: anchor,
		PairedProvider: "anthropic", PairedModel: paired, Reason: "cluster",
	}
	fresh := router.Decision{Metadata: &router.RoutingMetadata{Embedding: emb}}

	// Baseline: with no restrictions the head swaps to the paired model.
	if got := s.bandSwapServed(context.Background(), turntype.MainLoop, pin, fresh, false, nil, nil); got.Model != served {
		t.Fatalf("baseline swap served %q, want %q", got.Model, served)
	}
	// Chosen model in the context-window deny set -> anchor.
	excluded := map[string]struct{}{served: {}}
	if got := s.bandSwapServed(context.Background(), turntype.MainLoop, pin, fresh, false, nil, excluded); got.Model != anchor {
		t.Fatalf("excluded-model swap served %q, want anchor %q", got.Model, anchor)
	}
	// Chosen model's provider not in the request's enabled set -> anchor.
	enabled := map[string]struct{}{"openrouter": {}}
	if got := s.bandSwapServed(context.Background(), turntype.MainLoop, pin, fresh, false, enabled, nil); got.Model != anchor {
		t.Fatalf("ineligible-provider swap served %q, want anchor %q", got.Model, anchor)
	}
}
