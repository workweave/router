package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestDetectToolCallLoop_TripsAtMaxRepeats(t *testing.T) {
	// 5 identical (ls, /tmp) tool calls in a row → trip on the 5th.
	body := buildBodyWithToolCalls(t, []toolCall{
		{name: "ls", input: map[string]any{"path": "/tmp"}},
		{name: "ls", input: map[string]any{"path": "/tmp"}},
		{name: "ls", input: map[string]any{"path": "/tmp"}},
		{name: "ls", input: map[string]any{"path": "/tmp"}},
		{name: "ls", input: map[string]any{"path": "/tmp"}},
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	loop, sig, count := detectToolCallLoop(env)
	assert.True(t, loop)
	assert.Equal(t, "ls", sig.Name)
	assert.GreaterOrEqual(t, count, loopDetectionMaxRepeats)
}

func TestDetectToolCallLoop_NoLoopBelowThreshold(t *testing.T) {
	body := buildBodyWithToolCalls(t, []toolCall{
		{name: "ls", input: map[string]any{"path": "/tmp"}},
		{name: "ls", input: map[string]any{"path": "/tmp"}},
		{name: "ls", input: map[string]any{"path": "/tmp"}},
		{name: "ls", input: map[string]any{"path": "/tmp"}},
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	loop, _, _ := detectToolCallLoop(env)
	assert.False(t, loop, "4 identical calls must not trip the detector (threshold is 5)")
}

func TestDetectToolCallLoop_DifferentArgsDoNotTrip(t *testing.T) {
	body := buildBodyWithToolCalls(t, []toolCall{
		{name: "ls", input: map[string]any{"path": "/a"}},
		{name: "ls", input: map[string]any{"path": "/b"}},
		{name: "ls", input: map[string]any{"path": "/c"}},
		{name: "ls", input: map[string]any{"path": "/d"}},
		{name: "ls", input: map[string]any{"path": "/e"}},
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	loop, _, _ := detectToolCallLoop(env)
	assert.False(t, loop, "same tool name but distinct args must not trip the detector")
}

func TestDetectToolCallLoop_WindowedOldEntriesDropOut(t *testing.T) {
	// Window is 10. Put 4 (ls,/tmp) entries spaced far apart (separated by
	// many distinct calls). The window should be small enough that the
	// (ls,/tmp) count drops below threshold by the time we sample it.
	calls := []toolCall{
		{name: "ls", input: map[string]any{"path": "/tmp"}},
		{name: "ls", input: map[string]any{"path": "/tmp"}},
	}
	for i := range 15 {
		calls = append(calls, toolCall{name: "read", input: map[string]any{"path": "/etc", "n": i}})
	}
	body := buildBodyWithToolCalls(t, calls)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	loop, _, _ := detectToolCallLoop(env)
	assert.False(t, loop, "stale repeats outside the window must not trip the detector")
}

func TestDetectToolCallLoop_AlternatingPairStillTripsOnRepeats(t *testing.T) {
	// An A/B alternating loop (Hermes-style qwen3 failure mode) still trips
	// because each leg accrues count independently.
	calls := []toolCall{
		{name: "ls", input: map[string]any{"path": "/tmp"}},
		{name: "read", input: map[string]any{"path": "/etc/hosts"}},
		{name: "ls", input: map[string]any{"path": "/tmp"}},
		{name: "read", input: map[string]any{"path": "/etc/hosts"}},
		{name: "ls", input: map[string]any{"path": "/tmp"}},
		{name: "read", input: map[string]any{"path": "/etc/hosts"}},
		{name: "ls", input: map[string]any{"path": "/tmp"}},
		{name: "read", input: map[string]any{"path": "/etc/hosts"}},
		{name: "ls", input: map[string]any{"path": "/tmp"}},
	}
	body := buildBodyWithToolCalls(t, calls)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	loop, sig, count := detectToolCallLoop(env)
	assert.True(t, loop)
	assert.Equal(t, "ls", sig.Name)
	assert.GreaterOrEqual(t, count, loopDetectionMaxRepeats)
}

// --- helpers ---

type toolCall struct {
	name  string
	input map[string]any
}

func buildBodyWithToolCalls(t *testing.T, calls []toolCall) []byte {
	t.Helper()
	msgs := []any{
		map[string]any{"role": "user", "content": "do the thing"},
	}
	for i, c := range calls {
		id := "toolu_" + itoa(i)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": id, "name": c.name, "input": c.input},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": id, "content": "result"},
			}},
		)
	}
	body, err := json.Marshal(map[string]any{
		"model":      "claude-sonnet-4-6",
		"max_tokens": 256,
		"messages":   msgs,
	})
	require.NoError(t, err)
	return body
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string('0'+byte(n%10)) + digits
		n /= 10
	}
	return digits
}

// ---- cyclic (wide re-read) loop detector ----

func cyclicReads(nFiles, total int) []toolCall {
	calls := make([]toolCall, 0, total)
	for i := 0; i < total; i++ {
		calls = append(calls, toolCall{name: "Read", input: map[string]any{"file_path": "/app/f" + itoa(i%nFiles) + ".go"}})
	}
	return calls
}

func TestDetectCyclicToolCallLoop_TripsOnLowDiversityCycle(t *testing.T) {
	// 30 Reads cycling over 5 files (each 6x) → distinct ratio 5/30 ≈ 0.17 < 0.4.
	env, err := translate.ParseAnthropic(buildBodyWithToolCalls(t, cyclicReads(5, 30)))
	require.NoError(t, err)
	looped, top, count, ratio, total := detectCyclicToolCallLoop(env)
	assert.True(t, looped, "low-diversity re-read cycle must trip")
	assert.Equal(t, "Read", top.Name)
	assert.GreaterOrEqual(t, count, 2)
	assert.Less(t, ratio, cyclicLoopMaxDistinctRatio)
	assert.Equal(t, cyclicLoopWindowSize, total)
}

func TestDetectCyclicToolCallLoop_BroadDistinctReadsDoNotTrip(t *testing.T) {
	// A healthy Explore reads MANY DISTINCT files → high diversity → no trip.
	env, err := translate.ParseAnthropic(buildBodyWithToolCalls(t, cyclicReads(30, 30)))
	require.NoError(t, err)
	looped, _, _, ratio, _ := detectCyclicToolCallLoop(env)
	assert.False(t, looped, "broad distinct exploration must not trip (the #271 guard)")
	assert.GreaterOrEqual(t, ratio, cyclicLoopMaxDistinctRatio)
}

func TestDetectCyclicToolCallLoop_EditInWindowIsProgress(t *testing.T) {
	// Same low-diversity cycle but with a real Edit in the window → progress, no trip.
	calls := cyclicReads(5, 29)
	calls = append(calls, toolCall{name: "Edit", input: map[string]any{"file_path": "/app/f0.go", "old_string": "a", "new_string": "b"}})
	env, err := translate.ParseAnthropic(buildBodyWithToolCalls(t, calls))
	require.NoError(t, err)
	looped, _, _, _, _ := detectCyclicToolCallLoop(env)
	assert.False(t, looped, "an edit in the window means the agent is progressing, not stuck")
}

func TestDetectCyclicToolCallLoop_BelowMinCallsDoesNotTrip(t *testing.T) {
	// Fewer than cyclicLoopMinCalls tool calls → too early to call it a loop.
	env, err := translate.ParseAnthropic(buildBodyWithToolCalls(t, cyclicReads(3, 20)))
	require.NoError(t, err)
	looped, _, _, _, _ := detectCyclicToolCallLoop(env)
	assert.False(t, looped, "below the min-calls floor must not trip")
}

// --- handleLoopEscalation observability: kill switch, holdout, budget, events ---

// recordingLoopStore is an in-memory LoopEscalationStore that captures inserts
// and serves a configurable budget count.
type recordingLoopStore struct {
	mu       sync.Mutex
	events   []LoopEscalationEvent
	count    int64
	countErr error
}

func (r *recordingLoopStore) InsertLoopEscalationEvent(_ context.Context, p LoopEscalationEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, p)
	return nil
}

func (r *recordingLoopStore) CountLoopEscalationEvents(context.Context, []byte, string) (count int64, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count, r.countErr
}

// newLoopEscalationSvc wires a Service with just the pieces
// handleLoopEscalation touches.
func newLoopEscalationSvc(pins *stubPinStore, events *recordingLoopStore) *Service {
	svc := NewService(nil, nil, nil, false, nil, pins, false, "anthropic", "claude-haiku-4-5", nil)
	if events != nil {
		svc = svc.WithLoopEscalationStore(events)
	}
	return svc
}

func loopTestKey(seed byte) [sessionpin.SessionKeyLen]byte {
	var key [sessionpin.SessionKeyLen]byte
	sum := sha256.Sum256([]byte{seed})
	copy(key[:], sum[:])
	return key
}

var loopTestSig = translate.ToolCallSig{Name: "Read", InputHash: "abc123"}

func TestHandleLoopEscalation_RecordsEventAndPins(t *testing.T) {
	pins := newStubPinStore()
	events := &recordingLoopStore{}
	svc := newLoopEscalationSvc(pins, events)

	svc.handleLoopEscalation(context.Background(), loopTestSig, 12, 0.2, 30, uuid.New(), loopTestKey(1), "default", "claude-haiku-4-5")

	require.Len(t, events.events, 1, "detection must leave a durable event row")
	ev := events.events[0]
	assert.Equal(t, loopActionEscalated, ev.Action)
	assert.Equal(t, "claude-haiku-4-5", ev.LoopingModel)
	assert.Equal(t, escalateModel, ev.EscalationTarget)
	assert.Equal(t, "Read", ev.LoopTool)
	assert.Equal(t, int32(12), ev.RepeatCount)

	require.Len(t, pins.upserts, 1, "escalation must write the opus pin")
	assert.Equal(t, escalateModel, pins.upserts[0].Model)
	assert.Equal(t, translate.ReasonLoopEscalation, pins.upserts[0].Reason)
}

func TestHandleLoopEscalation_KillSwitchRecordsButDoesNotPin(t *testing.T) {
	pins := newStubPinStore()
	events := &recordingLoopStore{}
	svc := newLoopEscalationSvc(pins, events).WithLoopEscalationConfig(false, 0)

	svc.handleLoopEscalation(context.Background(), loopTestSig, 12, 0.2, 30, uuid.New(), loopTestKey(2), "default", "claude-haiku-4-5")

	require.Len(t, events.events, 1, "kill switch must not silence detection telemetry")
	assert.Equal(t, loopActionDisabled, events.events[0].Action)
	assert.Empty(t, pins.upserts, "kill switch must suppress the escalation pin")
}

func TestHandleLoopEscalation_HoldoutRecordsButDoesNotPin(t *testing.T) {
	pins := newStubPinStore()
	events := &recordingLoopStore{}
	svc := newLoopEscalationSvc(pins, events).WithLoopEscalationConfig(true, 100)

	svc.handleLoopEscalation(context.Background(), loopTestSig, 12, 0.2, 30, uuid.New(), loopTestKey(3), "default", "claude-haiku-4-5")

	require.Len(t, events.events, 1)
	assert.Equal(t, loopActionHoldout, events.events[0].Action)
	assert.Empty(t, pins.upserts, "holdout sessions must keep their original route")
}

func TestHandleLoopEscalation_HoldoutWithoutStoreStillEscalates(t *testing.T) {
	// A withheld rescue with no durable row is pure loss, not a measurement —
	// with no store wired the holdout must not apply.
	pins := newStubPinStore()
	svc := newLoopEscalationSvc(pins, nil).WithLoopEscalationConfig(true, 100)

	svc.handleLoopEscalation(context.Background(), loopTestSig, 12, 0.2, 30, uuid.New(), loopTestKey(4), "default", "claude-haiku-4-5")

	require.Len(t, pins.upserts, 1, "no store wired -> holdout disabled -> escalate")
	assert.Equal(t, translate.ReasonLoopEscalation, pins.upserts[0].Reason)
}

func TestHandleLoopEscalation_BudgetSuppressesRepeatFire(t *testing.T) {
	pins := newStubPinStore()
	events := &recordingLoopStore{count: 1}
	svc := newLoopEscalationSvc(pins, events)

	svc.handleLoopEscalation(context.Background(), loopTestSig, 12, 0.2, 30, uuid.New(), loopTestKey(5), "default", "claude-haiku-4-5")

	assert.Empty(t, events.events, "a session that already fired must not emit a second event")
	assert.Empty(t, pins.upserts, "a session that already fired must not re-pin")
}

func TestHandleLoopEscalation_AlreadyStrongRecordsOnly(t *testing.T) {
	pins := newStubPinStore()
	events := &recordingLoopStore{}
	svc := newLoopEscalationSvc(pins, events)

	svc.handleLoopEscalation(context.Background(), loopTestSig, 12, 0.2, 30, uuid.New(), loopTestKey(6), "default", escalateModel)

	require.Len(t, events.events, 1, "an opus loop is a training signal, record it")
	assert.Equal(t, loopActionAlreadyStrong, events.events[0].Action)
	assert.Empty(t, pins.upserts, "already on the escalation target -> nothing to pin")
}

func TestHandleLoopEscalation_UserForcedRecordsOnly(t *testing.T) {
	pins := newStubPinStore()
	pins.getFound = true
	pins.getPin = sessionpin.Pin{Model: "claude-haiku-4-5", Reason: translate.ReasonUserForceModel}
	events := &recordingLoopStore{}
	svc := newLoopEscalationSvc(pins, events)

	svc.handleLoopEscalation(context.Background(), loopTestSig, 12, 0.2, 30, uuid.New(), loopTestKey(7), "default", "claude-haiku-4-5")

	require.Len(t, events.events, 1)
	assert.Equal(t, loopActionUserForced, events.events[0].Action)
	assert.Empty(t, pins.upserts, "a /force-model pin outranks auto-escalation")
}

func TestHandleLoopEscalation_ExistingEscalationPinIsSilent(t *testing.T) {
	pins := newStubPinStore()
	pins.getFound = true
	pins.getPin = sessionpin.Pin{Model: escalateModel, Reason: translate.ReasonLoopEscalation}
	events := &recordingLoopStore{}
	svc := newLoopEscalationSvc(pins, events)

	svc.handleLoopEscalation(context.Background(), loopTestSig, 12, 0.2, 30, uuid.New(), loopTestKey(8), "default", "claude-haiku-4-5")

	assert.Empty(t, events.events, "an already-rescued session must not double-log")
	assert.Empty(t, pins.upserts)
}

func TestInLoopEscalationHoldout_DeterministicAndProportional(t *testing.T) {
	key := loopTestKey(9)
	assert.False(t, inLoopEscalationHoldout(key, 0), "pct 0 disables the holdout")
	assert.True(t, inLoopEscalationHoldout(key, 100), "pct 100 holds out everything")
	assert.Equal(t,
		inLoopEscalationHoldout(key, 10),
		inLoopEscalationHoldout(key, 10),
		"same key must always land in the same bucket")

	// sha256-derived keys are uniform; at pct=10 over 2000 keys the holdout
	// share must land near 10% (binomial 3-sigma is well within [7%, 13%]).
	held := 0
	const n = 2000
	for i := 0; i < n; i++ {
		sum := sha256.Sum256([]byte{byte(i), byte(i >> 8)})
		var k [sessionpin.SessionKeyLen]byte
		copy(k[:], sum[:])
		if inLoopEscalationHoldout(k, 10) {
			held++
		}
	}
	assert.InDelta(t, 0.10, float64(held)/float64(n), 0.03, "holdout share must track the configured percentage")
}

func TestHandleLoopEscalation_PinFailureLeavesNoEventRow(t *testing.T) {
	// Bugbot (PR #357): if the event row lands but the pin upsert fails, the
	// once-per-session budget sees count > 0 on the next detection and bails,
	// permanently blocking the rescue for a session that never got one. A
	// failed pin must leave NO row so the next turn retries the whole rescue.
	pins := newStubPinStore()
	pins.upsertErr = errors.New("postgres down")
	events := &recordingLoopStore{}
	svc := newLoopEscalationSvc(pins, events)

	svc.handleLoopEscalation(context.Background(), loopTestSig, 12, 0.2, 30, uuid.New(), loopTestKey(10), "default", "claude-haiku-4-5")

	assert.Empty(t, events.events, "a failed rescue must not consume the session's escalation budget")
	assert.Empty(t, pins.upserts)
}

func TestHandleLoopEscalation_NilInstallationSkipsHoldout(t *testing.T) {
	// Bugbot (PR #357): the holdout requires a recordable event row, and the
	// insert is skipped for unauthenticated requests — a nil installation in
	// the holdout bucket would withhold the rescue with no measurement record.
	pins := newStubPinStore()
	events := &recordingLoopStore{}
	svc := newLoopEscalationSvc(pins, events).WithLoopEscalationConfig(true, 100)

	svc.handleLoopEscalation(context.Background(), loopTestSig, 12, 0.2, 30, uuid.Nil, loopTestKey(11), "default", "claude-haiku-4-5")

	assert.Empty(t, events.events, "nil installation cannot record an event row")
	assert.Empty(t, pins.upserts, "nil installation cannot pin either — but it must not be counted as holdout")
}

// TestHandleLoopEscalation_AnthropicUnchanged is the Layer-3 regression guard:
// the existing handleLoopEscalation entrypoint must keep pinning to
// ProviderAnthropic + escalateModel. Written before the handleLoopEscalationTo
// extract so a bad refactor surfaces here immediately.
func TestHandleLoopEscalation_AnthropicUnchanged(t *testing.T) {
	pins := newStubPinStore()
	events := &recordingLoopStore{}
	svc := newLoopEscalationSvc(pins, events)

	svc.handleLoopEscalation(context.Background(), loopTestSig, 12, 0.2, 30, uuid.New(), loopTestKey(12), "default", "claude-haiku-4-5")

	require.Len(t, pins.upserts, 1)
	assert.Equal(t, providers.ProviderAnthropic, pins.upserts[0].Provider,
		"handleLoopEscalation must keep pinning Anthropic — Gemini escalation uses handleLoopEscalationTo")
	assert.Equal(t, escalateModel, pins.upserts[0].Model)
	assert.Equal(t, translate.ReasonLoopEscalation, pins.upserts[0].Reason)

	require.Len(t, events.events, 1)
	assert.Equal(t, escalateModel, events.events[0].EscalationTarget)
	assert.Equal(t, loopActionEscalated, events.events[0].Action)
}

func TestWriteSyntheticGeminiResponse_NonStreaming(t *testing.T) {
	const text = "loop break message"
	const inputTokens = 42
	env, err := translate.ParseGemini([]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"stream":false}`))
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	require.NoError(t, writeSyntheticGeminiResponse(rec, env, text, inputTokens))

	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	body := rec.Body.Bytes()
	assert.Equal(t, text, gjson.GetBytes(body, "candidates.0.content.parts.0.text").String())
	assert.Equal(t, "model", gjson.GetBytes(body, "candidates.0.content.role").String())
	assert.Equal(t, "STOP", gjson.GetBytes(body, "candidates.0.finishReason").String())
	outTokens := len(text) / 4
	assert.Equal(t, int64(inputTokens), gjson.GetBytes(body, "usageMetadata.promptTokenCount").Int())
	assert.Equal(t, int64(outTokens), gjson.GetBytes(body, "usageMetadata.candidatesTokenCount").Int())
	assert.Equal(t, int64(inputTokens+outTokens), gjson.GetBytes(body, "usageMetadata.totalTokenCount").Int())
}

func TestWriteSyntheticGeminiResponse_Streaming(t *testing.T) {
	const text = "stream loop break"
	const inputTokens = 17
	env, err := translate.ParseGemini([]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"stream":true}`))
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	require.NoError(t, writeSyntheticGeminiResponse(rec, env, text, inputTokens))

	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	frames := geminiSSEDataFrames(t, rec.Body.String())
	require.Len(t, frames, 2, "streaming synthetic Gemini response must be exactly two SSE data frames")

	assert.Equal(t, text, gjson.Get(frames[0], "candidates.0.content.parts.0.text").String())
	assert.Equal(t, "model", gjson.Get(frames[0], "candidates.0.content.role").String())
	assert.Equal(t, "", gjson.Get(frames[0], "candidates.0.finishReason").String(),
		"first frame is content-only; finishReason belongs on the second frame")

	assert.Equal(t, "STOP", gjson.Get(frames[1], "candidates.0.finishReason").String())
	parts := gjson.Get(frames[1], "candidates.0.content.parts")
	require.True(t, parts.Exists() && parts.IsArray())
	assert.Equal(t, 0, len(parts.Array()), "second frame must carry empty parts")
	outTokens := len(text) / 4
	assert.Equal(t, int64(inputTokens), gjson.Get(frames[1], "usageMetadata.promptTokenCount").Int())
	assert.Equal(t, int64(outTokens), gjson.Get(frames[1], "usageMetadata.candidatesTokenCount").Int())
	assert.Equal(t, int64(inputTokens+outTokens), gjson.Get(frames[1], "usageMetadata.totalTokenCount").Int())
}

func TestHandleToolCallLoopBreak_Gemini(t *testing.T) {
	// Today handleToolCallLoopBreak falls through to the Anthropic writer for
	// any non-OpenAI format. After Layer 3 it must emit a Gemini-native body.
	body, err := json.Marshal(map[string]any{
		"contents": []any{
			map[string]any{"role": "user", "parts": []any{
				map[string]any{"text": "do stuff"},
			}},
		},
		"stream": false,
	})
	require.NoError(t, err)
	env, err := translate.ParseGemini(body)
	require.NoError(t, err)

	pins := newStubPinStore()
	svc := newLoopEscalationSvc(pins, nil)
	rec := httptest.NewRecorder()
	err = svc.handleToolCallLoopBreak(
		context.Background(), rec, env, loopTestSig, 5,
		uuid.New(), loopTestKey(13), "default",
		"gemini-2.5-flash", providers.ProviderGoogle, 99,
	)
	require.NoError(t, err)

	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"),
		"Gemini loop break must not use the Anthropic/OpenAI content type")
	resp := rec.Body.Bytes()
	require.True(t, gjson.GetBytes(resp, "candidates").Exists(),
		"Gemini loop break must write candidates[], not an Anthropic message envelope; got %s", rec.Body.String())
	assert.Equal(t, "STOP", gjson.GetBytes(resp, "candidates.0.finishReason").String())
	assert.Equal(t, "model", gjson.GetBytes(resp, "candidates.0.content.role").String())
	text := gjson.GetBytes(resp, "candidates.0.content.parts.0.text").String()
	assert.Contains(t, text, "Read", "break message should name the looping tool")
	assert.Contains(t, text, "5", "break message should include the repeat count")
	assert.False(t, gjson.GetBytes(resp, "type").Exists(),
		"must not be an Anthropic synthetic message (type=message)")
	assert.False(t, gjson.GetBytes(resp, "choices").Exists(),
		"must not be an OpenAI synthetic chat.completion")
}

func TestHandleLoopEscalationTo_PinsGoogleModel(t *testing.T) {
	pins := newStubPinStore()
	events := &recordingLoopStore{}
	svc := newLoopEscalationSvc(pins, events)

	svc.handleLoopEscalationTo(
		context.Background(), loopTestSig, 12, 0.2, 30,
		uuid.New(), loopTestKey(14), "default", "gemini-2.5-flash",
		providers.ProviderGoogle, geminiEscalateModel,
	)

	require.Len(t, pins.upserts, 1, "Gemini escalation must write a pin")
	assert.Equal(t, providers.ProviderGoogle, pins.upserts[0].Provider)
	assert.Equal(t, geminiEscalateModel, pins.upserts[0].Model)
	assert.Equal(t, "gemini-3.1-pro-preview", pins.upserts[0].Model)
	assert.Equal(t, translate.ReasonLoopEscalation, pins.upserts[0].Reason)

	require.Len(t, events.events, 1)
	assert.Equal(t, loopActionEscalated, events.events[0].Action)
	assert.Equal(t, geminiEscalateModel, events.events[0].EscalationTarget)
	assert.Equal(t, "gemini-2.5-flash", events.events[0].LoopingModel)
}

// geminiSSEDataFrames splits an SSE body into the JSON payloads of each
// `data:` frame (blank-line delimited), skipping comment/event-only frames.
func geminiSSEDataFrames(t *testing.T, body string) []string {
	t.Helper()
	var frames []string
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data: ") {
				frames = append(frames, strings.TrimPrefix(line, "data: "))
				break
			}
		}
	}
	return frames
}
