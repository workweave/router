package percallband

import (
	"encoding/json"
	"testing"
)

// A representative agent trajectory: reason -> explore -> reason -> edit -> execute...
// with realistic timings + output sizes, long enough to exercise every ring
// (lastK=5, lastKWide=12) and the cumulative aggregates.
var snapshotReplay = []struct {
	action Action
	ts     int64
	outTok int
	isTR   bool
}{
	{Reason, 1000, 120, false},
	{Explore, 1002, 40, false},
	{Reason, 1003, 200, true},
	{Edit, 1010, 60, false},
	{Execute, 1012, 15, false},
	{Reason, 1015, 300, true},
	{Explore, 1400, 55, false}, // >300s gap
	{Edit, 1405, 90, false},
	{Orchestrate, 1406, 10, false},
	{Integrate, 1408, 25, true},
	{Reason, 1410, 180, false},
	{Edit, 1412, 70, false},
	{Execute, 1415, 20, false},
	{Reason, 1420, 140, true},
}

var snapshotProbe = Call{
	TurnType:             "main_loop",
	StepIdx:              7,
	Ts:                   1425,
	EstimatedInputTokens: 4096,
	LastFileExt:          "go",
}

func featuresEqual(a, b []float32) (int, bool) {
	if len(a) != len(b) {
		return -1, false
	}
	for i := range a {
		if a[i] != b[i] {
			return i, false
		}
	}
	return -1, true
}

// TestSnapshotRoundTrip proves that persisting the ring and rehydrating it — even
// through a JSON encode/decode, as the session_pins column will — is bit-for-bit
// lossless for feature computation: the restored ring must produce the exact same
// feature vector as an uninterrupted ring at every subsequent step.
func TestSnapshotRoundTrip(t *testing.T) {
	live := NewState()
	for cut := 0; cut <= len(snapshotReplay); cut++ {
		// Fresh uninterrupted ring replayed up to `cut`.
		want := NewState()
		for _, r := range snapshotReplay[:cut] {
			want.Advance(r.action, r.ts, r.outTok, r.isTR)
		}

		// Snapshot -> JSON -> restore, mirroring the persistence boundary exactly.
		raw, err := json.Marshal(live.Snapshot())
		if err != nil {
			t.Fatalf("cut=%d marshal snapshot: %v", cut, err)
		}
		var snap Snapshot
		if err := json.Unmarshal(raw, &snap); err != nil {
			t.Fatalf("cut=%d unmarshal snapshot: %v", cut, err)
		}
		restored := StateFromSnapshot(snap)

		if idx, ok := featuresEqual(restored.Features(snapshotProbe), want.Features(snapshotProbe)); !ok {
			t.Fatalf("cut=%d: restored features diverge at index %d", cut, idx)
		}

		// Advance BOTH the live and restored rings by one more call and confirm the
		// restored ring keeps producing identical features — a stateful field that
		// failed to serialize would only surface after a post-restore advance.
		if cut < len(snapshotReplay) {
			r := snapshotReplay[cut]
			live.Advance(r.action, r.ts, r.outTok, r.isTR)
			restored.Advance(r.action, r.ts, r.outTok, r.isTR)
			wantNext := NewState()
			for _, rr := range snapshotReplay[:cut+1] {
				wantNext.Advance(rr.action, rr.ts, rr.outTok, rr.isTR)
			}
			if idx, ok := featuresEqual(restored.Features(snapshotProbe), wantNext.Features(snapshotProbe)); !ok {
				t.Fatalf("cut=%d: restored+advance features diverge at index %d", cut, idx)
			}
		}
	}
}

// TestSnapshotZeroValue confirms the zero Snapshot rehydrates to an empty session
// (a NULL/absent action_history column must behave as a cold start, not panic).
func TestSnapshotZeroValue(t *testing.T) {
	restored := StateFromSnapshot(Snapshot{})
	fresh := NewState()
	if idx, ok := featuresEqual(restored.Features(snapshotProbe), fresh.Features(snapshotProbe)); !ok {
		t.Fatalf("zero snapshot not equal to fresh state at index %d", idx)
	}
	// And it must be safe to advance.
	restored.Advance(Reason, 1, 10, false)
}
