package proxy_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSummarizer is a deterministic handover.Summarizer for tests.
// summary is returned verbatim on Summarize; if errOnCall is non-nil it
// is returned instead. calls counts invocations so tests can assert the
// adapter was reached.
type fakeSummarizer struct {
	summary   string
	errOnCall error
	calls     atomic.Int32
}

func (f *fakeSummarizer) Summarize(ctx context.Context, env *translate.RequestEnvelope) (string, error) {
	f.calls.Add(1)
	if f.errOnCall != nil {
		return "", f.errOnCall
	}
	return f.summary, nil
}

// usageProvider is a fakeProvider that writes an Anthropic non-streaming
// response with the configured token-usage payload so the OTel
// UsageExtractor surfaces non-zero usage to the cache-stats writeback.
type usageProvider struct {
	in       int
	out      int
	cacheIn  int
	cacheOut int
}

func (p *usageProvider) Proxy(ctx context.Context, _ router.Decision, _ providers.PreparedRequest, w http.ResponseWriter, _ *http.Request) error {
	body := `{"id":"m","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":` +
		itoa(p.in) + `,"output_tokens":` + itoa(p.out) +
		`,"cache_creation_input_tokens":` + itoa(p.cacheOut) +
		`,"cache_read_input_tokens":` + itoa(p.cacheIn) + `}}`
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
	return nil
}

func (p *usageProvider) Passthrough(ctx context.Context, _ providers.PreparedRequest, _ http.ResponseWriter, _ *http.Request) error {
	return nil
}

func itoa(n int) string {
	// Tiny helper that avoids importing strconv into the test body.
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// largeBody yields ~10k estimated input tokens via a long user prompt;
// the planner's EV math scales with feats.Tokens so we need a sizable
// prompt to push expected savings comfortably over the threshold.
func largeBody(t *testing.T) []byte {
	t.Helper()
	prompt := strings.Repeat("aaaa ", 8000) // ~10k tokens
	return []byte(`{
		"model":"claude-opus-4-7",
		"system":"sys",
		"messages":[{"role":"user","content":"` + prompt + `"}]
	}`)
}

// TestTurnLoop_ToolResultWithPinSkipsScorerAndPlanner pins down the
// short-circuit: a trailing tool_result turn must reuse the pin without
// consulting the scorer (re-routing on tool_result embeddings flips
// decisions to noisy candidates) or the planner.
func TestTurnLoop_ToolResultWithPinSkipsScorerAndPlanner(t *testing.T) {
	const toolResultBody = `{
		"model":"claude-opus-4-7",
		"system":"sys",
		"messages":[
			{"role":"user","content":"plan"},
			{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"R","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}
		]
	}`
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:    providers.ProviderAnthropic,
		Model:       "claude-haiku-4-5",
		Reason:      "cluster:v0.2",
		PinnedUntil: time.Now().Add(time.Hour),
	}
	// fakeRouter.err makes any Route() call fail; the test passes only
	// if the orchestrator never touches the scorer.
	fr := &fakeRouter{err: errors.New("scorer must not be called on tool_result turn")}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(toolResultBody), rec, httpReq))

	assert.Equal(t, 0, fr.routeCalls, "tool_result must not invoke the scorer")
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get("x-router-model"))
}

// TestTurnLoop_PlannerStaysWhenScorerAgrees verifies the same-model
// trivial-stay branch: scorer recommends the pin's model, planner
// returns reason=same_model, pin wins.
func TestTurnLoop_PlannerStaysWhenScorerAgrees(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:        providers.ProviderAnthropic,
		Model:           "claude-haiku-4-5",
		Reason:          "cluster:v0.2",
		PinnedUntil:     time.Now().Add(time.Hour),
		LastTurnEndedAt: time.Now().Add(-1 * time.Minute),
	}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "cluster:v0.2"}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httpReq))

	assert.Equal(t, 1, fr.routeCalls, "planner re-eval still runs the scorer")
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get("x-router-model"))
}

// TestTurnLoop_PlannerSwitchesOnPositiveEV constructs a scenario where
// pinning the (expensive) opus model against a (cheap) haiku scorer
// recommendation makes the EV math strongly positive, so the planner
// switches and the handover summarizer is invoked.
func TestTurnLoop_PlannerSwitchesOnPositiveEV(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:        providers.ProviderAnthropic,
		Model:           "claude-opus-4-7",
		Reason:          "cluster:v0.2",
		PinnedUntil:     time.Now().Add(time.Hour),
		LastInputTokens: 5000,
		LastTurnEndedAt: time.Now().Add(-30 * time.Second),
	}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "cluster:v0.2"}}
	sz := &fakeSummarizer{summary: "Prior conversation summary."}
	svc := newPinSvc(fr, store).WithSummarizer(sz)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, largeBody(t), rec, httpReq))

	assert.Equal(t, 1, fr.routeCalls)
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get("x-router-model"), "planner must switch to fresh model")
	assert.Equal(t, int32(1), sz.calls.Load(), "summarizer must be invoked on switch")

	waitForUpsert(t, store)
	require.NotEmpty(t, store.upserts)
	last := store.upserts[len(store.upserts)-1]
	assert.Equal(t, "claude-haiku-4-5", last.Model, "switch must persist the new model on the pin")
}

// TestTurnLoop_SummarizerErrorFallsBackToTrim asserts the graceful-
// degradation path: a summarizer error must not abort the request; the
// orchestrator falls back to trim-last-N and proceeds with the switch.
func TestTurnLoop_SummarizerErrorFallsBackToTrim(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:        providers.ProviderAnthropic,
		Model:           "claude-opus-4-7",
		Reason:          "cluster:v0.2",
		PinnedUntil:     time.Now().Add(time.Hour),
		LastInputTokens: 5000,
		LastTurnEndedAt: time.Now().Add(-30 * time.Second),
	}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "cluster:v0.2"}}
	sz := &fakeSummarizer{errOnCall: errors.New("upstream haiku 500")}
	svc := newPinSvc(fr, store).WithSummarizer(sz)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, largeBody(t), rec, httpReq))

	assert.Equal(t, int32(1), sz.calls.Load(), "summarizer must be tried before fallback")
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get("x-router-model"), "switch must still happen on summarizer error")
}

// TestTurnLoop_PlannerDisabledPreservesFirstDecisionWins exercises the
// ROUTER_PLANNER_ENABLED kill switch: an existing pin wins outright and
// the scorer is not consulted, mirroring the legacy stickiness.
func TestTurnLoop_PlannerDisabledPreservesFirstDecisionWins(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:    providers.ProviderAnthropic,
		Model:       "claude-haiku-4-5",
		Reason:      "cluster:v0.2",
		PinnedUntil: time.Now().Add(time.Hour),
	}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-opus-4-7", Reason: "fresh"}}
	svc := newPinSvc(fr, store).WithPlannerEnabled(false)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httpReq))

	assert.Equal(t, 0, fr.routeCalls, "planner-disabled with pin must skip the scorer")
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get("x-router-model"))
}

// TestTurnLoop_UsageWritebackPersistsCacheStats wires a usage-emitting
// fake provider through ProxyMessages and asserts the orchestrator
// writes the upstream usage back to the pin row.
func TestTurnLoop_UsageWritebackPersistsCacheStats(t *testing.T) {
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "fresh"}}
	provider := &usageProvider{in: 1200, out: 80, cacheIn: 900, cacheOut: 200}
	// Telemetry repo flips usageRequired() on, which is what wires the
	// extractor up so its tokens flow into recordTurnUsage. nil here
	// would short-circuit usage extraction in the proxy.
	svc := proxy.NewService(
		fr,
		map[string]providers.Client{providers.ProviderAnthropic: provider},
		nil,
		false,
		nil,
		store,
		false,
		providers.ProviderAnthropic, "claude-haiku-4-5",
		recordingTelemetry{},
	)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httpReq))

	select {
	case <-store.usageCh:
	case <-time.After(2 * time.Second):
		t.Fatal("expected UpdateUsage within 2s; none observed")
	}

	require.Len(t, store.usages, 1)
	got := store.usages[0]
	assert.Equal(t, 1200, got.InputTokens)
	assert.Equal(t, 80, got.OutputTokens)
	assert.Equal(t, 900, got.CachedReadTokens)
	assert.Equal(t, 200, got.CachedWriteTokens)
}

// recordingTelemetry is the minimum no-op telemetry repository that
// flips proxy.Service.usageRequired() on so the OTel extractor wires up.
// We do not assert on telemetry rows here.
type recordingTelemetry struct{}

func (recordingTelemetry) InsertRequestTelemetry(ctx context.Context, p proxy.InsertTelemetryParams) error {
	return nil
}
func (recordingTelemetry) GetTelemetrySummary(ctx context.Context, installationID string, from, to time.Time) (proxy.TelemetrySummary, error) {
	return proxy.TelemetrySummary{}, nil
}
func (recordingTelemetry) GetTelemetryTimeseries(ctx context.Context, installationID string, from, to time.Time, granularity string) ([]proxy.TelemetryBucket, error) {
	return nil, nil
}
func (recordingTelemetry) GetTelemetrySummaryAll(ctx context.Context, from, to time.Time) (proxy.TelemetrySummary, error) {
	return proxy.TelemetrySummary{}, nil
}
func (recordingTelemetry) GetTelemetryTimeseriesAll(ctx context.Context, from, to time.Time, granularity string) ([]proxy.TelemetryBucket, error) {
	return nil, nil
}
func (recordingTelemetry) GetTelemetryRows(ctx context.Context, installationID string, from, to time.Time, limit int32) ([]proxy.TelemetryRow, error) {
	return nil, nil
}
func (recordingTelemetry) GetTelemetryRowsAll(ctx context.Context, from, to time.Time, limit int32) ([]proxy.TelemetryRow, error) {
	return nil, nil
}
