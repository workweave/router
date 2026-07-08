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
	"workweave/router/internal/router/planner"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// fakeSummarizer is a deterministic handover.Summarizer: returns summary,
// or errOnCall if set. calls counts invocations.
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

// usageProvider writes an Anthropic response with the configured token
// usage so the OTel UsageExtractor surfaces it to the cache-stats writeback.
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

// largeBody yields ~10k input tokens so the planner's EV math clears threshold.
func largeBody(t *testing.T) []byte {
	t.Helper()
	prompt := strings.Repeat("aaaa ", 8000) // ~10k tokens
	return []byte(`{
		"model":"claude-opus-4-7",
		"system":"sys",
		"messages":[{"role":"user","content":"` + prompt + `"}]
	}`)
}

// largeMultiTurnBody yields a 6-message conversation so a trim-to-last-3
// fallback would be observable (6→3), proving the handover failure path
// preserves full history instead.
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

const toolResultPinnedBody = `{
	"model":"claude-opus-4-7",
	"system":"sys",
	"messages":[
		{"role":"user","content":"plan"},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"R","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}
	]
}`

// TestTurnLoop_ToolResultScoringDisabledSkipsScorer verifies kill-switch-off:
// a pinned tool_result reuses the pin verbatim without scorer or planner.
func TestTurnLoop_ToolResultScoringDisabledSkipsScorer(t *testing.T) {
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
	fr := &fakeRouter{err: errors.New("scorer must not be called when tool-result scoring is disabled")}
	svc := newPinSvc(fr, store).WithScoreToolResultTurns(false)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(toolResultPinnedBody), rec, httpReq))

	assert.Equal(t, 0, fr.routeCalls, "disabled tool-result scoring must not invoke the scorer")
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get(proxy.HeaderRouterModel))
}

// TestTurnLoop_ToolResultScoringEnabledRunsScorerAndStays verifies the default path:
// scorer runs (routeCalls==1) but planner STAYs, so the served model is unchanged.
func TestTurnLoop_ToolResultScoringEnabledRunsScorerAndStays(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:    providers.ProviderAnthropic,
		Model:       "claude-haiku-4-5",
		Reason:      "cluster:v0.2",
		PinnedUntil: time.Now().Add(time.Hour),
	}
	// Scorer agrees with the pin, so the planner STAYs and the served model
	// is the pin's — but the scorer MUST have been consulted (routeCalls==1).
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "cluster:v0.2"}}
	svc := newPinSvc(fr, store) // default: scoreToolResultTurns == true

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(toolResultPinnedBody), rec, httpReq))

	assert.Equal(t, 1, fr.routeCalls, "tool_result must run the scorer under MainLoop parity")
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get(proxy.HeaderRouterModel), "planner agreement STAYs on the pinned model")
}

// TestTurnLoop_ToolResultScoringEnabledSwitchesSafely verifies a positive-EV switch
// on a tool_result turn: handover strips the orphaned tool_result from the forwarded body.
func TestTurnLoop_ToolResultScoringEnabledSwitchesSafely(t *testing.T) {
	chunk := strings.Repeat("aaaa ", 4000) // ~5k tokens each, positive EV
	toolResultLargeBody := []byte(`{
		"model":"claude-opus-4-7",
		"system":"sys",
		"messages":[
			{"role":"user","content":"` + chunk + `"},
			{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"R","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"` + chunk + `"}]}
		]
	}`)
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
	svc, up := newPinSvcCapturing(fr, store)
	svc = svc.WithSummarizer(sz)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, toolResultLargeBody, rec, httpReq))

	assert.Equal(t, 1, fr.routeCalls, "tool_result must run the scorer under MainLoop parity")
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get(proxy.HeaderRouterModel), "positive-EV switch must move off the pinned model")
	assert.Equal(t, int32(1), sz.calls.Load(), "summarizer must be invoked on a tool_result switch")

	// The forwarded body must not contain the orphaned tool_result: handover
	// rewrote history to [summary, latestUser-minus-tool_results].
	require.NotEmpty(t, up.proxyBodies, "upstream must have been called")
	assert.NotContains(t, string(up.proxyBodies[0]), "tool_result",
		"handover must strip the orphaned tool_result on a mid-tool-use switch")
}

func TestTurnLoop_HMMToolResultCommunicationFollowsFreshDecision(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:    providers.ProviderAnthropic,
		Model:       "claude-haiku-4-5",
		Reason:      "hmm_policy:tool_execution(label=SPAWN_EXPLORE)",
		PinnedUntil: time.Now().Add(time.Hour),
	}
	fr := &fakeRouter{decision: router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-sonnet-4-5",
		Reason:   "hmm_policy(label=Simple Followup)",
		Metadata: &router.RoutingMetadata{
			Strategy: string(router.StrategyHMM),
			RouteID:  "route-1",
		},
	}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(toolResultPinnedBody), rec, httpReq))

	assert.Equal(t, 1, fr.routeCalls, "tool_result must ask HMM for a fresh communication decision")
	assert.Equal(t, "claude-sonnet-4-5", rec.Header().Get(proxy.HeaderRouterModel),
		"a completed tool result must not stay pinned to the tool-execution model")
	store.mu.Lock()
	assertOnlyHMMHistoryUpserts(t, store)
	store.mu.Unlock()
}

func TestTurnLoop_HMMToolResultToolExecutionUsesFreshDecision(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:    providers.ProviderAnthropic,
		Model:       "claude-haiku-4-5",
		Reason:      "hmm_policy:tool_execution(label=SPAWN_EXPLORE)",
		PinnedUntil: time.Now().Add(time.Hour),
	}
	fr := &fakeRouter{decision: router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-sonnet-4-5",
		Reason:   "hmm_policy:tool_execution(label=SPAWN_EXPLORE)",
		Metadata: &router.RoutingMetadata{
			Strategy: string(router.StrategyHMM),
			RouteID:  "route-1",
		},
	}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(toolResultPinnedBody), rec, httpReq))

	assert.Equal(t, 1, fr.routeCalls, "tool_result still scores so HMM can decide whether execution continues")
	assert.Equal(t, "claude-sonnet-4-5", rec.Header().Get(proxy.HeaderRouterModel),
		"HMM tool execution must follow the fresh sidecar decision instead of an existing session pin")
	store.mu.Lock()
	assertOnlyHMMHistoryUpserts(t, store)
	store.mu.Unlock()
}

func TestTurnLoop_HMMToolExecutionStaysWhenWarmCacheEVBeatsCheapFresh(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:        providers.ProviderAnthropic,
		Model:           "claude-sonnet-5",
		Reason:          "hmm_policy:tool_execution(label=SPAWN_EXPLORE)",
		PinnedUntil:     time.Now().Add(time.Hour),
		LastInputTokens: 5000,
		LastTurnEndedAt: time.Now().Add(-30 * time.Second),
	}
	fr := &fakeRouter{decision: router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-haiku-4-5",
		Reason:   "hmm_policy:tool_execution(label=SPAWN_EXPLORE)",
		Metadata: &router.RoutingMetadata{
			Strategy: string(router.StrategyHMM),
			RouteID:  "route-1",
		},
	}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httpReq))

	assert.Equal(t, 1, fr.routeCalls, "ongoing HMM execution still scores to see if execution continues")
	assert.Equal(t, "claude-sonnet-5", rec.Header().Get(proxy.HeaderRouterModel),
		"HMM should stay when switching to a cheaper tool model would not beat warm-cache eviction cost")
	store.mu.Lock()
	assertOnlyHMMHistoryUpserts(t, store)
	store.mu.Unlock()
}

func TestTurnLoop_HMMHistoryMaxedOutExcludesServedModelBeforeRouting(t *testing.T) {
	store := newFakePinStore()
	store.hasHMMHistory = true
	store.hmmHistory = sessionpin.Pin{
		Provider:         providers.ProviderAnthropic,
		LastServedModel:  "claude-sonnet-5",
		LastOutputTokens: 8192,
		LastTurnEndedAt:  time.Now().Add(-30 * time.Second),
		PinnedUntil:      time.Now().Add(time.Hour),
	}
	fr := &fakeRouter{decision: router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-haiku-4-5",
		Reason:   "hmm_policy(classifier 'Simple Tool Call Request')",
		Metadata: &router.RoutingMetadata{
			Strategy: string(router.StrategyHMM),
			RouteID:  "route-1",
		},
	}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httpReq))

	require.NotNil(t, fr.capturedReq)
	assert.Contains(t, fr.capturedReq.ExcludedModels, "claude-sonnet-5",
		"HMM history saturation must exclude the prior served model before sidecar routing")
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get(proxy.HeaderRouterModel))
	store.mu.Lock()
	assertOnlyHMMHistoryUpserts(t, store)
	store.mu.Unlock()
}

func TestTurnLoop_HMMExpiredHistoryMaxedOutStillExcludesServedModel(t *testing.T) {
	store := newFakePinStore()
	store.hasHMMHistory = true
	// Expired history row (PinnedUntil in the past) that maxed out its output
	// cap: the maxed model must still be excluded, matching the active-pin path.
	store.hmmHistory = sessionpin.Pin{
		Provider:         providers.ProviderAnthropic,
		LastServedModel:  "claude-sonnet-5",
		LastOutputTokens: 8192,
		LastTurnEndedAt:  time.Now().Add(-time.Hour),
		PinnedUntil:      time.Now().Add(-time.Minute),
	}
	fr := &fakeRouter{decision: router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-haiku-4-5",
		Reason:   "hmm_policy(classifier 'Simple Tool Call Request')",
		Metadata: &router.RoutingMetadata{
			Strategy: string(router.StrategyHMM),
			RouteID:  "route-1",
		},
	}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httpReq))

	require.NotNil(t, fr.capturedReq)
	assert.Contains(t, fr.capturedReq.ExcludedModels, "claude-sonnet-5",
		"a maxed-out model must stay excluded even after the history row's TTL lapses")
	store.mu.Lock()
	assertOnlyHMMHistoryUpserts(t, store)
	store.mu.Unlock()
}

func TestTurnLoop_HMMConversationFollowsFreshDecision(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:        providers.ProviderAnthropic,
		Model:           "claude-haiku-4-5",
		Reason:          "hmm_policy(label=Simple Followup)",
		PinnedUntil:     time.Now().Add(time.Hour),
		LastInputTokens: 5000,
		LastTurnEndedAt: time.Now().Add(-30 * time.Second),
	}
	fr := &fakeRouter{decision: router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-sonnet-5",
		Reason:   "hmm_policy(label=Complex Design)",
		Metadata: &router.RoutingMetadata{
			Strategy:    string(router.StrategyHMM),
			RouteID:     "route-1",
			ChosenScore: 0.90,
		},
	}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httpReq))

	assert.Equal(t, 1, fr.routeCalls, "HMM conversation turns must score fresh")
	assert.Equal(t, "claude-sonnet-5", rec.Header().Get(proxy.HeaderRouterModel),
		"HMM normal conversation routing must follow the fresh sidecar decision instead of EV-staying on the old pin")
	store.mu.Lock()
	assertOnlyHMMHistoryUpserts(t, store)
	store.mu.Unlock()
}

func TestTurnLoop_HMMToolExecutionPhaseChangeUsesFreshDecision(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:        providers.ProviderAnthropic,
		Model:           "claude-sonnet-4-5",
		Reason:          "hmm_policy(label=Complex Followup)",
		PinnedUntil:     time.Now().Add(time.Hour),
		LastInputTokens: 5000,
		LastTurnEndedAt: time.Now().Add(-30 * time.Second),
	}
	fr := &fakeRouter{decision: router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-sonnet-5",
		Reason:   "hmm_policy:tool_execution(label=SPAWN_EXPLORE)",
		Metadata: &router.RoutingMetadata{
			Strategy: string(router.StrategyHMM),
			RouteID:  "route-1",
		},
	}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httpReq))

	assert.Equal(t, 1, fr.routeCalls, "main-loop HMM turn must ask for a fresh decision")
	assert.Equal(t, "claude-sonnet-5", rec.Header().Get(proxy.HeaderRouterModel),
		"HMM tool/explore phase changes should follow the fresh sidecar decision")

	store.mu.Lock()
	assertOnlyHMMHistoryUpserts(t, store)
	store.mu.Unlock()
}

func TestTurnLoop_HMMToolExecutionSameModelDoesNotRewritePin(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:        providers.ProviderAnthropic,
		Model:           "claude-sonnet-4-5",
		Reason:          "hmm_policy(label=Complex Followup)",
		PinnedUntil:     time.Now().Add(time.Hour),
		LastInputTokens: 5000,
		LastTurnEndedAt: time.Now().Add(-30 * time.Second),
	}
	fr := &fakeRouter{decision: router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-sonnet-4-5",
		Reason:   "hmm_policy:tool_execution(label=SPAWN_EXPLORE)",
		Metadata: &router.RoutingMetadata{
			Strategy: string(router.StrategyHMM),
			RouteID:  "route-1",
		},
	}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httpReq))

	assert.Equal(t, 1, fr.routeCalls, "main-loop HMM turn must ask for a fresh decision")
	assert.Equal(t, "claude-sonnet-4-5", rec.Header().Get(proxy.HeaderRouterModel))

	store.mu.Lock()
	assertOnlyHMMHistoryUpserts(t, store)
	store.mu.Unlock()
}

// TestTurnLoop_ToolResultPinOnExcludedProviderFallsThroughToScorer verifies that a pin
// on an excluded provider falls through to the scorer rather than being served sticky.
func TestTurnLoop_ToolResultPinOnExcludedProviderFallsThroughToScorer(t *testing.T) {
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
		Provider:    providers.ProviderOpenAI,
		Model:       "gpt-5.5",
		Reason:      "cluster:v0.2",
		PinnedUntil: time.Now().Add(time.Hour),
	}
	fr := &fakeRouter{decision: router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-haiku-4-5",
		Reason:   "cluster:v0.2",
	}}
	svc := proxy.NewService(
		fr,
		map[string]providers.Client{
			providers.ProviderAnthropic: &fakeProvider{},
			providers.ProviderOpenAI:    &fakeProvider{},
		},
		nil,
		false,
		nil,
		store,
		false,
		providers.ProviderAnthropic,
		"claude-haiku-4-5",
		nil,
	)

	ctx := context.WithValue(authedCtx(uuid.New().String()),
		proxy.InstallationExcludedProvidersContextKey{}, []string{providers.ProviderOpenAI})
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(toolResultBody), rec, httpReq))

	assert.Equal(t, 1, fr.routeCalls,
		"pin on an excluded provider must fall through to the scorer instead of being served sticky")
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get(proxy.HeaderRouterModel),
		"the turn must be served on the scorer's fresh decision, not the excluded pin")
}

// Same-model trivial-stay: scorer recommends the pin's model, pin wins.
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

// Pinning expensive opus against a cheap haiku recommendation makes EV
// strongly positive, so the planner switches and invokes the summarizer.
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

// A summarizer error must not abort the request or trim history — the
// orchestrator forwards full prior history. Trimming silently lobotomized
// switched-to models in prod.
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

// No-summarizer path (e.g. self-hoster without an Anthropic key): the switch
// turn must forward the full conversation unchanged rather than trimming it.
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

// When the request forwards creds for the summarizer's own provider, the
// orchestrator summarizes using the caller's creds (not the deployment
// account), still producing a clean envelope cross-provider models can accept.
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

// If the request is BYOK for a different provider than the summarizer's,
// summarizing would route prior conversation through the platform account —
// skip it and pass full history through unchanged.
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

// ROUTER_PLANNER_ENABLED kill switch: an existing pin wins outright without
// consulting the scorer, mirroring legacy stickiness.
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

// Asserts the orchestrator writes upstream usage back to the pin row.
func TestTurnLoop_UsageWritebackPersistsCacheStats(t *testing.T) {
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "fresh"}}
	provider := &usageProvider{in: 1200, out: 80, cacheIn: 900, cacheOut: 200}
	// Telemetry repo flips usageRequired() on; nil here would short-circuit
	// usage extraction in the proxy.
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

// trimSessionTurn builds an alternating user/assistant body with a constant
// first message, so every turn derives the same session key.
func trimSessionTurn(t *testing.T, msgCount int) []byte {
	t.Helper()
	require.True(t, msgCount%2 == 1, "odd msgCount keeps the trailing message a user turn")
	msgs := make([]string, 0, msgCount)
	for i := 0; i < msgCount; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, `{"role":"`+role+`","content":"TRIM-SESSION turn `+itoa(i)+`"}`)
	}
	msgs[0] = `{"role":"user","content":"TRIM-SESSION refactor the dispatch loop"}`
	return []byte(`{"model":"claude-opus-4-7","system":"sys","messages":[` + strings.Join(msgs, ",") + `]}`)
}

// warmOpusPin's prior turn billed 1.5k input tokens seconds ago (warm), so an
// opus→haiku switch is an EV-negative stay unless something prices the cache
// as cold.
func warmOpusPin() sessionpin.Pin {
	return sessionpin.Pin{
		Provider:        providers.ProviderAnthropic,
		Model:           "claude-opus-4-7",
		Reason:          "cluster:v0.2",
		PinnedUntil:     time.Now().Add(time.Hour),
		LastInputTokens: 1500,
		LastTurnEndedAt: time.Now().Add(-30 * time.Second),
	}
}

// A client history trim must make the planner price the warm pin's cache as
// dead — letting the cold-pin follow-fresh lever switch — without invoking
// the switch summarizer.
func TestTurnLoop_PrefixTrimPricesPinColdAndSkipsSummarizer(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = warmOpusPin()
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "cluster:v0.2"}}
	sz := &fakeSummarizer{summary: "Prior conversation summary."}
	svc := newPinSvc(fr, store).WithSummarizer(sz).WithPlanner(planner.EVConfig{
		ThresholdUSD:           0.001,
		ExpectedRemainingTurns: 3,
		ColdPinFollowFresh:     true,
	})
	ctx := authedCtx(uuid.New().String())

	// Turn 1 (9 messages) records the compaction baseline.
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, trimSessionTurn(t, 9), rec1, req1))
	require.Equal(t, "claude-opus-4-7", rec1.Header().Get(proxy.HeaderRouterModel),
		"warm pin must stay on turn 1 (cold lever must not fire on a warm cache)")

	// Turn 2 drops to 3 messages: a full-compaction trim.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, trimSessionTurn(t, 3), rec2, req2))
	assert.Equal(t, "claude-haiku-4-5", rec2.Header().Get(proxy.HeaderRouterModel),
		"trim turn must price the pin cold and switch to the fresh pick")
	assert.Equal(t, int32(0), sz.calls.Load(),
		"switch on a trim turn must skip the summarizer — the client's compaction summary already bounds the body")
}

// With the kill switch off, the trim is detected but the planner keeps
// pricing the pin warm and stays, even with the cold-pin lever armed.
func TestTurnLoop_PrefixTrimKillSwitchPreservesWarmStay(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = warmOpusPin()
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "cluster:v0.2"}}
	sz := &fakeSummarizer{summary: "Prior conversation summary."}
	svc := newPinSvc(fr, store).WithSummarizer(sz).WithPrefixTrimFreeSwitch(false).WithPlanner(planner.EVConfig{
		ThresholdUSD:           0.001,
		ExpectedRemainingTurns: 3,
		ColdPinFollowFresh:     true,
	})
	ctx := authedCtx(uuid.New().String())

	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, trimSessionTurn(t, 9), rec1, req1))
	require.Equal(t, "claude-opus-4-7", rec1.Header().Get(proxy.HeaderRouterModel))

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, trimSessionTurn(t, 3), rec2, req2))
	assert.Equal(t, "claude-opus-4-7", rec2.Header().Get(proxy.HeaderRouterModel),
		"kill switch off must preserve the warm-priced EV stay on the trim turn")
	assert.Equal(t, int32(0), sz.calls.Load())
}

// A trim turn must skip the expired-pin re-anchor (guard (h)): the prior
// model's cache is dead regardless of pin expiry, so the fresh pick wins.
func TestTurnLoop_PrefixTrimSkipsExpiredPinReAnchor(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	expired := warmOpusPin()
	expired.PinnedUntil = time.Now().Add(-time.Minute)
	store.pin = expired
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "cluster:v0.2"}}
	svc := newPinSvc(fr, store).WithAvailableModels(map[string]struct{}{
		"claude-opus-4-7":  {},
		"claude-haiku-4-5": {},
	})
	ctx := authedCtx(uuid.New().String())

	// Turn 1 (9 messages, no trim): the expired pin re-anchors, proving the
	// re-anchor path is live before the trim turn bypasses it.
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, trimSessionTurn(t, 9), rec1, req1))
	require.Equal(t, "claude-opus-4-7", rec1.Header().Get(proxy.HeaderRouterModel),
		"expired pin must re-anchor on a normal turn (guard baseline)")

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, trimSessionTurn(t, 3), rec2, req2))
	assert.Equal(t, "claude-haiku-4-5", rec2.Header().Get(proxy.HeaderRouterModel),
		"trim turn must skip the expired-pin re-anchor and follow the fresh pick")
}

// A sub-agent's small opening turn derives its own session key and compaction
// bucket, so it must not read as a trim of the main loop's baseline.
func TestTurnLoop_SubAgentDoesNotInheritMainLoopTrimBaseline(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = warmOpusPin()
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "cluster:v0.2"}}
	sz := &fakeSummarizer{summary: "Prior conversation summary."}
	svc := newPinSvc(fr, store).WithSummarizer(sz)
	ctx := authedCtx(uuid.New().String())

	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, trimSessionTurn(t, 9), rec1, req1))
	require.Equal(t, "claude-opus-4-7", rec1.Header().Get(proxy.HeaderRouterModel))

	subAgentBody := []byte(`{"model":"claude-opus-4-7","system":"sys","messages":[
		{"role":"user","content":"SUB-AGENT find every .go file under internal/"},
		{"role":"assistant","content":"searching"},
		{"role":"user","content":"narrow to the proxy package"}
	]}`)
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, subAgentBody, rec2, req2))
	assert.Equal(t, "claude-opus-4-7", rec2.Header().Get(proxy.HeaderRouterModel),
		"a sub-agent's small opening turn must not read as a trim of the main loop's baseline")
	assert.Equal(t, int32(0), sz.calls.Load())
}

// Runaway-tool-call escape hatch: when the prior turn saturated the output
// cap, the pinned model is excluded and treated as missing before the scorer
// runs. Without this, Claude Code's auto-continue locks the session into the
// broken model for minutes.
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

// With band swap, the served model can differ from the pin's anchor. The
// maxed-out guard must exclude LastServedModel, not the anchor — otherwise
// the broken paired model stays eligible.
func TestTurnLoop_MaxedOutExcludesLastServedModelNotAnchor(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:         providers.ProviderAnthropic,
		Model:            "claude-haiku-4-5",     // anchor, healthy
		LastServedModel:  "moonshotai/kimi-k2.6", // swapped-to paired model that saturated the cap
		Reason:           "cluster:v0.52",
		PinnedUntil:      time.Now().Add(time.Hour),
		LastOutputTokens: 8192, // saturated previous turn
		LastTurnEndedAt:  time.Now().Add(-10 * time.Second),
	}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-5", Reason: "fresh"}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httpReq))

	require.NotNil(t, fr.capturedReq, "scorer must run after a maxed-out pin is dropped")
	assert.Contains(t, fr.capturedReq.ExcludedModels, "moonshotai/kimi-k2.6",
		"the model that actually maxed out (LastServedModel) must be excluded")
	assert.NotContains(t, fr.capturedReq.ExcludedModels, "claude-haiku-4-5",
		"the healthy anchor must not be excluded when the paired model maxed out")
}

// Output well below the cap is healthy: the pin must not be excluded.
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

// recordingTelemetry is a no-op repo that flips usageRequired() on; we
// don't assert on telemetry rows here.
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
