package bandswap

import "testing"

func TestNewLoadsEmbeddedModel(t *testing.T) {
	clf, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(clf.classes) == 0 {
		t.Fatal("no classes loaded")
	}
	for _, a := range clf.classes {
		if _, ok := bandMap[a]; !ok {
			t.Fatalf("class %q missing from band map", a)
		}
	}
}

func TestPredictBandReturnsValidBand(t *testing.T) {
	clf, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	emb := make([]float32, EmbedDim)
	for i := range emb {
		emb[i] = 0.01
	}
	action, band, ok := clf.PredictBand(emb)
	if !ok {
		t.Fatal("PredictBand not ok for valid-width embedding")
	}
	if _, known := bandMap[action]; !known {
		t.Fatalf("predicted unknown action %q", action)
	}
	if band != Large && band != Small {
		t.Fatalf("invalid band %q", band)
	}
	if Band(action) != band {
		t.Fatalf("Band(%q)=%q != PredictBand band %q", action, Band(action), band)
	}
}

func TestPredictRejectsWrongWidth(t *testing.T) {
	clf, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := clf.PredictAction(make([]float32, EmbedDim-1)); ok {
		t.Fatal("expected ok=false for short embedding")
	}
	if _, _, ok := clf.PredictBand(nil); ok {
		t.Fatal("expected ok=false for nil embedding")
	}
}

func TestBandMapping(t *testing.T) {
	cases := map[string]string{
		"planning":       Large,
		"learning":       Large,
		"providing_info": Small,
		"tool_call":      Small,
		"confirmation":   Small,
		"refinement":     Small,
		"some_unknown":   Large, // unknown routes large (capability-preserving)
	}
	for action, want := range cases {
		if got := Band(action); got != want {
			t.Errorf("Band(%q)=%q, want %q", action, got, want)
		}
	}
}
