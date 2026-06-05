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
	"workweave/router/internal/router/handover"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
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

func (f *fakeSummarizer) Summarize(ctx context.Context, env *translate.RequestEnvelope) (string, handover.Usage, error) {
	f.calls.Add(1)
	if f.errOnCall != nil {
		return "", handover.Usage{}, f.errOnCall
	}
	return f.summary, handover.Usage{}, nil
}

func (f *fakeSummarizer) Provider() string { return providers.ProviderAnthropic }

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

// largeMultiTurnBody yields a ~10k-token conversation of six non-system
// messages so a trim-to-last-3 fallback would be observable: the forwarded
// body would shrink from 6 messages to 3. Used to prove the handover failure
// path preserves the full history rather than trimming it.
func largeMultiTurnBody(t *testing.T) []byte {
	t.Helper()
	chunk := strings.Repeat("aaaa ", 1600) // ~2k tokens each
	msgs := []string{
		`{"role":"user","content":"FIRST-USER-MARKER ` + chunk + `"}`,
		`{"role":"assistant","content":"` + chunk + `"}`,
		`{"role":"user","content":"` + chunk + `"}`,
		`{"role":"assistant","content":"` + chunk + `"}`,
		`{"role":"user","content":"` + chunk + `"}`,
		`{"role":"user","content":"latest question"}`,
	}
	return []byte(`{"model":"claude-opus-4-7","system":"sys","messages":[` + strings.Join(msgs, ",") + `]}`)
}

// forwardedMessageCount returns the number of messages in the body the
// orchestrator forwarded to the upstream provider on its first Proxy call.
func forwardedMessageCount(t *testing.T, p *fakeProvider) int {
	t.Helper()
	require.NotEmpty(t, p.proxyBodies, "upstream must have been called")
	return int(gjson.GetBytes(p.proxyBodies[0], "messages.#").Int())
}

// newPinSvcCapturing mirrors newPinSvc but returns the upstream fakeProvider
// so a test can inspect the body forwarded after handover.
func newPinSvcCapturing(fr *fakeRouter, store *fakePinStore) (*proxy.Service, *fakeProvider) {
	p := &fakeProvider{}
	svc := proxy.NewService(
		fr,
		map[string]providers.Client{providers.ProviderAnthropic: p},
		nil,
		false,
		nil,
		store,
		false,
		providers.ProviderAnthropic,
		"claude-haiku-4-5",
		nil,
	)
	return svc, p
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
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get(proxy.HeaderRouterModel))
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
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get(proxy.HeaderRouterModel))
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
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get(proxy.HeaderRouterModel), "planner must switch to fresh model")
	assert.Equal(t, int32(1), sz.calls.Load(), "summarizer must be invoked on switch")

	waitForUpsert(t, store)
	require.NotEmpty(t, store.upserts)
	last := store.upserts[len(store.upserts)-1]
	assert.Equal(t, "claude-haiku-4-5", last.Model, "switch must persist the new model on the pin")
}

// TestTurnLoop_SummarizerErrorPreservesFullHistory asserts the failure
// path: a summarizer error must not abort the request, and must NOT trim the
// conversation — the orchestrator forwards the full prior history to the new
// model so it keeps the context it needs. Trimming to the last few turns
// silently lobotomized switched-to models in prod.
func TestTurnLoop_SummarizerErrorPreservesFullHistory(t *testing.T) {
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
	svc, provider := newPinSvcCapturing(fr, store)
	svc.WithSummarizer(sz)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, largeMultiTurnBody(t), rec, httpReq))

	assert.Equal(t, int32(1), sz.calls.Load(), "summarizer must be tried before the fallback")
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get(proxy.HeaderRouterModel), "switch must still happen on summarizer error")
	assert.Equal(t, 6, forwardedMessageCount(t, provider), "all 6 messages must be forwarded — the failure path must not trim history")
	assert.Contains(t, string(provider.proxyBodies[0]), "FIRST-USER-MARKER", "the earliest user turn must survive (trim-to-last-N would drop it)")
}

// TestTurnLoop_HandoverPreservesFullHistoryWhenSummarizerNotWired guards the
// no-summarizer path: when no summarizer is wired (e.g. a self-hoster without
// an Anthropic key for handover), the switch turn must forward the full
// conversation unchanged rather than trimming it.
func TestTurnLoop_HandoverPreservesFullHistoryWhenSummarizerNotWired(t *testing.T) {
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
	// No WithSummarizer call — Service.summarizer stays nil.
	svc, provider := newPinSvcCapturing(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, largeMultiTurnBody(t), rec, httpReq))

	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get(proxy.HeaderRouterModel), "switch must proceed even without a summarizer")
	assert.Equal(t, 6, forwardedMessageCount(t, provider), "all 6 messages must be forwarded when no summarizer is wired — no trimming")
}

// TestTurnLoop_HandoverUsesClientCredsForSummarizerProvider verifies the
// caller-keyed summarization path: when the request forwards credentials
// for the summarizer's own provider, the orchestrator runs summarization
// using the caller's credentials rather than skipping it. This preserves
// the tenant boundary (no traffic through the deployment account) while
// still producing a clean handover envelope that cross-provider models
// (e.g. Gemini 3.x, which requires thoughtSignature on every functionCall)
// can accept.
func TestTurnLoop_HandoverUsesClientCredsForSummarizerProvider(t *testing.T) {
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
	httpReq.Header.Set("x-api-key", "sk-ant-customer-byok-key")
	require.NoError(t, svc.ProxyMessages(ctx, largeBody(t), rec, httpReq))

	assert.Equal(t, int32(1), sz.calls.Load(), "summarizer must run with the caller's own Anthropic key (no tenant boundary crossed)")
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get(proxy.HeaderRouterModel))
}

// TestTurnLoop_HandoverSkippedWhenClientCredsCrossProvider keeps the
// tenant-boundary guard: if the request is BYOK/client-keyed for a
// DIFFERENT provider than the summarizer's, the deployment summarizer
// would route prior conversation through the platform account. Skip
// summarization in that case and pass the full history through unchanged.
func TestTurnLoop_HandoverSkippedWhenClientCredsCrossProvider(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:        providers.ProviderOpenAI,
		Model:           "gpt-5",
		Reason:          "cluster:v0.2",
		PinnedUntil:     time.Now().Add(time.Hour),
		LastInputTokens: 5000,
		LastTurnEndedAt: time.Now().Add(-30 * time.Second),
	}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "cluster:v0.2"}}
	sz := &fakeSummarizer{summary: "Should not be invoked."}
	svc := newPinSvc(fr, store).WithSummarizer(sz)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	// Client-supplied OpenAI key — does NOT match the Anthropic summarizer.
	httpReq.Header.Set("Authorization", "Bearer sk-customer-openai-key")
	require.NoError(t, svc.ProxyMessages(ctx, largeBody(t), rec, httpReq))

	assert.Equal(t, int32(0), sz.calls.Load(), "summarizer must NOT run when client creds are for a different provider than the summarizer")
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get(proxy.HeaderRouterModel), "switch must still happen with full history passed through")
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
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get(proxy.HeaderRouterModel))
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

// TestTurnLoop_MaxedOutPinExcludedFromCandidates locks in the
// runaway-tool-call escape hatch: when the previous turn saturated the
// output cap (a hallmark of OSS-model parse-failure runaways), the pinned
// model is excluded from the candidate set and the pin is treated as
// missing so sticky branches cannot re-anchor it before the scorer runs.
// Without this, Claude Code's "Output token limit hit. Resume directly…"
// auto-continue locks the session into the broken model for minutes.
func TestTurnLoop_MaxedOutPinExcludedFromCandidates(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:         providers.ProviderOpenRouter,
		Model:            "moonshotai/kimi-k2.6",
		Reason:           "cluster:v0.52",
		PinnedUntil:      time.Now().Add(time.Hour),
		LastOutputTokens: 8192, // saturated previous turn
		LastTurnEndedAt:  time.Now().Add(-10 * time.Second),
	}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "fresh"}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httpReq))

	require.NotNil(t, fr.capturedReq, "scorer must run after a maxed-out pin is dropped")
	assert.Contains(t, fr.capturedReq.ExcludedModels, "moonshotai/kimi-k2.6",
		"pinned model must be excluded from candidates after a maxed-out turn")
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get(proxy.HeaderRouterModel),
		"router must serve the fresh decision, not the broken pin")
}

// TestTurnLoop_UnderMaxedOutThresholdKeepsPin guards against false positives:
// a turn that produced output well below the cap is healthy, so the pin
// must not be excluded and the planner's normal STAY logic applies.
func TestTurnLoop_UnderMaxedOutThresholdKeepsPin(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:         providers.ProviderAnthropic,
		Model:            "claude-haiku-4-5",
		Reason:           "cluster:v0.52",
		PinnedUntil:      time.Now().Add(time.Hour),
		LastOutputTokens: 1024, // healthy, well below threshold
		LastTurnEndedAt:  time.Now().Add(-10 * time.Second),
	}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "fresh"}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httpReq))

	require.NotNil(t, fr.capturedReq)
	assert.NotContains(t, fr.capturedReq.ExcludedModels, "claude-haiku-4-5",
		"healthy pin must not be excluded from candidates")
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get(proxy.HeaderRouterModel))
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
