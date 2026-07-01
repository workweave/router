package percallband

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

// builderTol absorbs the fixture's 6-decimal rounding + float32 compute-order
// differences between numpy and Go. Comfortably tight enough to catch a wrong
// column order, off-by-one lag, or bad log/entropy.
const builderTol = 1e-4

type featureFixture struct {
	MarkovFeatureNames []string `json:"markov_feature_names"`
	Sessions           []struct {
		SessionID string       `json:"session_id"`
		Rows      []fixtureRow `json:"rows"`
		Expected  [][]float64  `json:"expected_markov"`
	} `json:"sessions"`
}

type fixtureRow struct {
	SessionID            string  `json:"session_id"`
	Ts                   int64   `json:"ts"`
	TurnType             string  `json:"turn_type"`
	StepIdx              int     `json:"step_idx"`
	Action               string  `json:"action"`
	OutputTokens         *int    `json:"output_tokens"`
	EstimatedInputTokens *int    `json:"estimated_input_tokens"`
	LastIsError          *bool   `json:"last_is_error"`
	LastFileExt          *string `json:"last_file_ext"`
}

// TestFeatureNamesMatchFixture asserts the Go column order equals the exported
// Python layout (a second guard beyond checkFeatureLayout in New()).
func TestFeatureNamesMatchFixture(t *testing.T) {
	fx := loadFeatureFixture(t)
	got := FeatureNames()
	if len(got) != len(fx.MarkovFeatureNames) {
		t.Fatalf("feature count: go=%d fixture=%d", len(got), len(fx.MarkovFeatureNames))
	}
	for i := range got {
		if got[i] != fx.MarkovFeatureNames[i] {
			t.Fatalf("feature[%d]: go=%q fixture=%q", i, got[i], fx.MarkovFeatureNames[i])
		}
	}
}

// TestFeatureBuilderParity replays each fixture session through the Go ring buffer
// and asserts every per-call markov vector matches the Python build() output. This
// tests the feature builder itself (not just leaves inference).
func TestFeatureBuilderParity(t *testing.T) {
	fx := loadFeatureFixture(t)
	if len(fx.Sessions) == 0 {
		t.Fatal("no fixture sessions")
	}
	var worst float64
	for _, sess := range fx.Sessions {
		st := NewState()
		for i, r := range sess.Rows {
			got := st.Features(callOf(r))
			exp := sess.Expected[i]
			if len(got) != len(exp) {
				t.Fatalf("session %s row %d: %d feats, want %d", sess.SessionID, i, len(got), len(exp))
			}
			for j := range got {
				d := math.Abs(float64(got[j]) - exp[j])
				if d > worst {
					worst = d
				}
				if d > builderTol {
					t.Errorf("session %s row %d feat %d (%s): go=%.6f want %.6f (Δ=%.2e)",
						sess.SessionID, i, j, FeatureNames()[j], got[j], exp[j], d)
				}
			}
			st.Advance(Action(r.Action), r.Ts, deref(r.OutputTokens), r.TurnType == "tool_result")
		}
	}
	t.Logf("feature builder parity: %d sessions, max Δ=%.2e", len(fx.Sessions), worst)
}

func callOf(r fixtureRow) Call {
	return Call{
		TurnType:             r.TurnType,
		StepIdx:              r.StepIdx,
		Ts:                   r.Ts,
		EstimatedInputTokens: deref(r.EstimatedInputTokens),
		LastIsError:          r.LastIsError != nil && *r.LastIsError,
		LastFileExt:          derefStr(r.LastFileExt),
	}
}

func loadFeatureFixture(t *testing.T) featureFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/percall_feature_fixture.json")
	if err != nil {
		t.Fatalf("read feature fixture: %v", err)
	}
	var fx featureFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse feature fixture: %v", err)
	}
	return fx
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
