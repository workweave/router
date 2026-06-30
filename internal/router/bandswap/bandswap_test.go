package bandswap

import (
	"encoding/json"
	"os"
	"testing"
)

// TestParityWithTrainedModel asserts the Go `leaves` inference reproduces the
// exported LightGBM booster's predictions on real embeddings.
// testdata/parity_cases.json is generated from dataset.npz + the exported
// booster (multiclass predict -> argmax over class order) — if Go drifts from
// the trained head, this fails.
func TestParityWithTrainedModel(t *testing.T) {
	clf, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	raw, err := os.ReadFile("testdata/parity_cases.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var cases []struct {
		Embedding      []float32 `json:"embedding"`
		ExpectedAction string    `json:"expected_action"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("no parity cases")
	}
	for i, c := range cases {
		got, ok := clf.PredictAction(c.Embedding)
		if !ok {
			t.Fatalf("case %d: predict not ok", i)
		}
		if got != c.ExpectedAction {
			t.Errorf("case %d: got %q, want %q", i, got, c.ExpectedAction)
		}
	}
}

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
