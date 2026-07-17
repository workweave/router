package proxy

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"workweave/router/internal/router"
	"workweave/router/internal/router/sessionpin"
)

// repinFakeStore is a minimal sessionpin.Store for the re-pin unit tests.
type repinFakeStore struct {
	getPin  sessionpin.Pin
	hasPin  bool
	upserts []sessionpin.Pin
}

func (f *repinFakeStore) Get(_ context.Context, key [sessionpin.SessionKeyLen]byte, role string) (sessionpin.Pin, bool, error) {
	if !f.hasPin {
		return sessionpin.Pin{}, false, nil
	}
	p := f.getPin
	p.SessionKey = key
	p.Role = role
	return p, true, nil
}
func (f *repinFakeStore) Upsert(_ context.Context, p sessionpin.Pin) error {
	f.upserts = append(f.upserts, p)
	return nil
}
func (f *repinFakeStore) UpdateUsage(context.Context, [sessionpin.SessionKeyLen]byte, string, sessionpin.Usage) error {
	return nil
}
func (f *repinFakeStore) IncrementUpstreamErrors(context.Context, [sessionpin.SessionKeyLen]byte, string) (int, error) {
	return 0, nil
}
func (f *repinFakeStore) ResetUpstreamErrors(context.Context, [sessionpin.SessionKeyLen]byte, string) error {
	return nil
}
func (f *repinFakeStore) SweepExpired(context.Context) error { return nil }

func activeCyberRefusalUpsert(upserts []sessionpin.Pin) (sessionpin.Pin, bool) {
	for _, u := range upserts {
		if u.Reason == cyberRefusalRepinReason && u.PinnedUntil.After(time.Now()) {
			return u, true
		}
	}
	return sessionpin.Pin{}, false
}

// A realistic Anthropic-native refusal SSE (the router-visible wire shape:
// stop_reason "refusal" on HTTP 200; see catalog.go). The real opus cyber
// refusal also carries api_refusal_category "cyber" / "safeguards flagged".
const refusalSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-4-8","stop_reason":null}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"refusal","stop_sequence":null},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}
`

const normalSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_2","model":"claude-opus-4-8"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null}}
`

func TestDetectRefusalSignal(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"stop_reason refusal", `"stop_reason":"refusal"`, true},
		{"refusal content block", `{"type":"refusal","text":"..."}`, true},
		{"api_refusal_category", `"api_refusal_category":"cyber"`, true},
		{"safeguard text mixed case", "This request triggered cyber-related SAFEGUARDS FLAGGED for review", true},
		{"normal end_turn", `"stop_reason":"end_turn"`, false},
		{"benign cybersecurity mention", "here is a summary of cybersecurity best practices", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := detectRefusalSignal([]byte(c.in)); got != c.want {
				t.Fatalf("detectRefusalSignal(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestRefusalObserver_ForwardsUnchangedAndDetects(t *testing.T) {
	// Signal split across two Write calls must still be caught, and every byte
	// must be forwarded to inner unchanged (observe-only).
	rec := httptest.NewRecorder()
	obs := newRefusalObserver(rec)
	half := len(refusalSSE) / 2
	if _, err := obs.Write([]byte(refusalSSE[:half])); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	if _, err := obs.Write([]byte(refusalSSE[half:])); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	if !obs.refused {
		t.Fatal("observer did not detect the refusal split across writes")
	}
	if got := rec.Body.String(); got != refusalSSE {
		t.Fatalf("observer altered forwarded bytes:\n got=%q\nwant=%q", got, refusalSSE)
	}

	rec2 := httptest.NewRecorder()
	obs2 := newRefusalObserver(rec2)
	if _, err := obs2.Write([]byte(normalSSE)); err != nil {
		t.Fatalf("write normal: %v", err)
	}
	if obs2.refused {
		t.Fatal("observer false-positived on a normal end_turn response")
	}
	if rec2.Body.String() != normalSSE {
		t.Fatal("observer altered a normal response")
	}
}

func repinCtx() context.Context {
	return context.WithValue(context.Background(), InstallationIDContextKey{}, uuid.New().String())
}

func TestMaybeRepinOnRefusal_RepinsToFallbackModel(t *testing.T) {
	store := &repinFakeStore{} // no existing pin -> no PairedModel
	s := &Service{pinStore: store, cyberRefusalFallbackModel: "claude-sonnet-5"}
	obs := &refusalObserver{refused: true}
	served := router.Decision{Provider: "anthropic", Model: "claude-opus-4-8"}

	s.maybeRepinOnRefusal(repinCtx(), obs, [sessionpin.SessionKeyLen]byte{1, 2, 3}, "main_loop", served)

	got, ok := activeCyberRefusalUpsert(store.upserts)
	if !ok {
		t.Fatalf("expected cyber-refusal-repin upsert, got %d upserts", len(store.upserts))
	}
	if got.Model != "claude-sonnet-5" {
		t.Fatalf("re-pinned to %q, want claude-sonnet-5", got.Model)
	}
	if got.Provider == "" {
		t.Fatal("re-pin has no provider (catalog resolution failed)")
	}
	if got.Reason != cyberRefusalRepinReason {
		t.Fatalf("re-pin reason = %q, want cyber-refusal-repin", got.Reason)
	}
}

func TestMaybeRepinOnRefusal_PrefersPairedModel(t *testing.T) {
	store := &repinFakeStore{
		hasPin: true,
		getPin: sessionpin.Pin{PairedModel: "claude-haiku-4-5", PairedProvider: "anthropic"},
	}
	s := &Service{pinStore: store, cyberRefusalFallbackModel: "claude-sonnet-5"}
	obs := &refusalObserver{refused: true}
	served := router.Decision{Provider: "anthropic", Model: "claude-opus-4-8"}

	s.maybeRepinOnRefusal(repinCtx(), obs, [sessionpin.SessionKeyLen]byte{4, 5, 6}, "main_loop", served)

	got, ok := activeCyberRefusalUpsert(store.upserts)
	if !ok {
		t.Fatalf("expected cyber-refusal-repin upsert, got %d upserts", len(store.upserts))
	}
	if got.Model != "claude-haiku-4-5" {
		t.Fatalf("re-pinned to %q, want the pin's PairedModel claude-haiku-4-5", got.Model)
	}
	if got.Provider != "anthropic" {
		t.Fatalf("re-pin provider = %q, want anthropic (from PairedProvider)", got.Provider)
	}
}

func TestMaybeRepinOnRefusal_ExpiresHMMHistory(t *testing.T) {
	store := &repinFakeStore{}
	s := &Service{pinStore: store, cyberRefusalFallbackModel: "claude-sonnet-5"}
	obs := &refusalObserver{refused: true}
	served := router.Decision{Provider: "anthropic", Model: "claude-opus-4-8"}

	s.maybeRepinOnRefusal(repinCtx(), obs, [sessionpin.SessionKeyLen]byte{8, 9}, "main_loop", served)

	var historyExpired bool
	for _, u := range store.upserts {
		if u.Role == hmmHistoryRole("main_loop") && u.PinnedUntil.Before(time.Now()) {
			historyExpired = true
		}
	}
	if !historyExpired {
		t.Fatalf("expected expired hmm_history upsert, got upserts: %+v", store.upserts)
	}
	if _, ok := activeCyberRefusalUpsert(store.upserts); !ok {
		t.Fatal("expected active cyber-refusal-repin upsert alongside hmm_history expiry")
	}
}

func TestMaybeRepinOnRefusal_NoOpCases(t *testing.T) {
	served := router.Decision{Provider: "anthropic", Model: "claude-opus-4-8"}
	key := [sessionpin.SessionKeyLen]byte{7}

	t.Run("nil observer (flag off)", func(t *testing.T) {
		store := &repinFakeStore{}
		s := &Service{pinStore: store, cyberRefusalFallbackModel: "claude-sonnet-5"}
		s.maybeRepinOnRefusal(repinCtx(), nil, key, "main_loop", served)
		if len(store.upserts) != 0 {
			t.Fatalf("nil observer should not re-pin, got %d upserts", len(store.upserts))
		}
	})

	t.Run("no refusal observed", func(t *testing.T) {
		store := &repinFakeStore{}
		s := &Service{pinStore: store, cyberRefusalFallbackModel: "claude-sonnet-5"}
		s.maybeRepinOnRefusal(repinCtx(), &refusalObserver{refused: false}, key, "main_loop", served)
		if len(store.upserts) != 0 {
			t.Fatalf("no refusal should not re-pin, got %d upserts", len(store.upserts))
		}
	})

	t.Run("served model already the fallback", func(t *testing.T) {
		store := &repinFakeStore{}
		s := &Service{pinStore: store, cyberRefusalFallbackModel: "claude-sonnet-5"}
		alreadyFallback := router.Decision{Provider: "anthropic", Model: "claude-sonnet-5"}
		s.maybeRepinOnRefusal(repinCtx(), &refusalObserver{refused: true}, key, "main_loop", alreadyFallback)
		if len(store.upserts) != 0 {
			t.Fatalf("re-pin to the same model should be skipped, got %d upserts", len(store.upserts))
		}
	})
}
