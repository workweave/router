package proxy

import (
	"context"
	"testing"

	"workweave/router/internal/router"
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
	got := s.bandSwapServed(context.Background(), turntype.MainLoop, pin, router.Decision{}, false)
	if got.Model != "claude-opus-4-7" {
		t.Fatalf("served %q, want anchor claude-opus-4-7", got.Model)
	}
}

// A pin with no runner-up can never swap, even if the head were enabled.
func TestBandSwapServed_NoPairServesAnchor(t *testing.T) {
	s := &Service{}
	pin := sessionpin.Pin{Provider: "anthropic", Model: "claude-opus-4-7", Reason: "cluster"}
	got := s.bandSwapServed(context.Background(), turntype.MainLoop, pin, router.Decision{}, false)
	if got.Model != "claude-opus-4-7" {
		t.Fatalf("served %q, want anchor", got.Model)
	}
}
