package percallband

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

// inferenceTol bounds Go leaves vs Python lightgbm P(LARGE) drift. The Python
// reload of the same v3 boosters is 5e-9; 1e-6 leaves ample headroom for the
// float32-fixture round-trip while still catching any misparse.
const inferenceTol = 1e-6

type parityFixture struct {
	Heads map[string]struct {
		File      string `json:"file"`
		NFeatures int    `json:"n_features"`
		Cases     []struct {
			Features []float64 `json:"features"`
			PLarge   float64   `json:"p_large"`
		} `json:"cases"`
	} `json:"heads"`
}

// TestInferenceParity proves the pure-Go leaves reader reproduces the frozen
// Python booster P(LARGE) on both heads — the load-bearing Go<->Python contract.
func TestInferenceParity(t *testing.T) {
	raw, err := os.ReadFile("testdata/percall_band_parity.json")
	if err != nil {
		t.Fatalf("read parity fixture: %v", err)
	}
	var fx parityFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse parity fixture: %v", err)
	}
	h, err := New()
	if err != nil {
		t.Fatalf("load head: %v", err)
	}

	for key, hf := range fx.Heads {
		ens := h.markov
		if key == "markov_emb" {
			ens = h.markovEmb
		}
		if len(hf.Cases) == 0 {
			t.Fatalf("head %q: no parity cases", key)
		}
		var worst float64
		for i, c := range hf.Cases {
			if len(c.Features) != hf.NFeatures {
				t.Fatalf("head %q case %d: %d features, want %d", key, i, len(c.Features), hf.NFeatures)
			}
			got := ens.PredictSingle(c.Features, 0)
			d := math.Abs(got - c.PLarge)
			if d > worst {
				worst = d
			}
			if d > inferenceTol {
				t.Errorf("head %q case %d: p_large=%.10f want %.10f (Δ=%.2e)", key, i, got, c.PLarge, d)
			}
		}
		t.Logf("head %q: %d cases, max Δ=%.2e", key, len(hf.Cases), worst)
	}
}

// TestPredictBandSelectsHeadAndThreshold checks head selection (emb present ->
// markov+emb) and the frozen decision rule (SMALL iff P(LARGE) < threshold).
func TestPredictBandSelectsHeadAndThreshold(t *testing.T) {
	h, err := New()
	if err != nil {
		t.Fatalf("load head: %v", err)
	}
	raw, err := os.ReadFile("testdata/percall_band_parity.json")
	if err != nil {
		t.Fatalf("read parity fixture: %v", err)
	}
	var fx parityFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse parity fixture: %v", err)
	}

	// markov-only path: emb nil -> markov head, markov threshold.
	mk := fx.Heads["markov"]
	c := mk.Cases[0]
	f32 := to32(c.Features)
	band, p, usedEmb := h.PredictBand(f32, nil)
	if usedEmb {
		t.Errorf("expected markov head (usedEmb=false) when emb is nil")
	}
	if want := bandFor(p, h.markovT); band != want {
		t.Errorf("band=%v want %v (p=%.4f t=%.4f)", band, want, p, h.markovT)
	}

	// markov+emb path: split a markov_emb feature vector into markov[:80] + emb[80:].
	me := fx.Heads["markov_emb"]
	full := to32(me.Cases[0].Features)
	band2, p2, usedEmb2 := h.PredictBand(full[:FeatureCount], full[FeatureCount:])
	if !usedEmb2 {
		t.Errorf("expected markov+emb head (usedEmb=true) when emb matches dim")
	}
	if math.Abs(p2-me.Cases[0].PLarge) > 1e-5 {
		t.Errorf("markov+emb p_large=%.6f want %.6f", p2, me.Cases[0].PLarge)
	}
	if want := bandFor(p2, h.markovEmbT); band2 != want {
		t.Errorf("band=%v want %v", band2, want)
	}
}

func to32(f []float64) []float32 {
	out := make([]float32, len(f))
	for i, v := range f {
		out[i] = float32(v)
	}
	return out
}
