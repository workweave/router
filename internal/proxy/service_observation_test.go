package proxy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/proxy/usage"
	"workweave/router/internal/router"
	"workweave/router/internal/router/policy"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureTelemetry records every InsertTelemetryParams the proxy fires.
// notify channel is the rendezvous for the async goroutine.
type captureTelemetry struct {
	mu           sync.Mutex
	rows         []proxy.InsertTelemetryParams
	shadowRows   []proxy.PolicyShadowDecision
	notify       chan struct{}
	shadowNotify chan struct{}
}

func newCaptureTelemetry() *captureTelemetry {
	return &captureTelemetry{
		notify:       make(chan struct{}, 4),
		shadowNotify: make(chan struct{}, 4),
	}
}

func (c *captureTelemetry) InsertRequestTelemetry(_ context.Context, p proxy.InsertTelemetryParams) error {
	c.mu.Lock()
	c.rows = append(c.rows, p)
	c.mu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
	return nil
}

func (c *captureTelemetry) InsertPolicyShadowDecision(_ context.Context, event proxy.PolicyShadowDecision) error {
	c.mu.Lock()
	c.shadowRows = append(c.shadowRows, event)
	c.mu.Unlock()
	select {
	case c.shadowNotify <- struct{}{}:
	default:
	}
	return nil
}

func (c *captureTelemetry) GetTelemetrySummary(context.Context, string, time.Time, time.Time) (proxy.TelemetrySummary, error) {
	return proxy.TelemetrySummary{}, nil
}

func (c *captureTelemetry) GetTelemetryTimeseries(context.Context, string, time.Time, time.Time, string) ([]proxy.TelemetryBucket, error) {
	return nil, nil
}

func (c *captureTelemetry) GetTelemetrySummaryAll(context.Context, time.Time, time.Time) (proxy.TelemetrySummary, error) {
	return proxy.TelemetrySummary{}, nil
}

func (c *captureTelemetry) GetTelemetryTimeseriesAll(context.Context, time.Time, time.Time, string) ([]proxy.TelemetryBucket, error) {
	return nil, nil
}

func (c *captureTelemetry) GetTelemetryRows(context.Context, string, time.Time, time.Time, int32) ([]proxy.TelemetryRow, error) {
	return nil, nil
}

func (c *captureTelemetry) GetTelemetryRowsAll(context.Context, time.Time, time.Time, int32) ([]proxy.TelemetryRow, error) {
	return nil, nil
}

func (c *captureTelemetry) firstRow(t *testing.T) proxy.InsertTelemetryParams {
	t.Helper()
	select {
	case <-c.notify:
	case <-time.After(2 * time.Second):
		t.Fatal("expected an async telemetry insert within 2s; none observed")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	require.NotEmpty(t, c.rows)
	return c.rows[0]
}

func (c *captureTelemetry) firstShadowRow(t *testing.T) proxy.PolicyShadowDecision {
	t.Helper()
	select {
	case <-c.shadowNotify:
	case <-time.After(4 * time.Second):
		t.Fatal("expected an async policy shadow insert within 4s; none observed")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	require.NotEmpty(t, c.shadowRows)
	return c.shadowRows[0]
}

type shadowRequestRouter struct {
	decision router.Decision
	requests chan router.Request
}

func (r *shadowRequestRouter) Route(_ context.Context, request router.Request) (router.Decision, error) {
	r.requests <- request
	return r.decision, nil
}

func TestPolicyShadowComparisonSkipsDryRunAndCollectsServingRoute(t *testing.T) {
	const installID = "66666666-6666-6666-6666-666666666666"
	serving := router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-haiku-4-5",
	}
	scorerDecision := router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-opus-4-7",
	}
	shadowRouter := &shadowRequestRouter{
		decision: router.Decision{
			Provider: providers.ProviderOpenAI,
			Model:    "gpt-5.5",
			Metadata: &router.RoutingMetadata{
				RouteID:              "shadow-route-1",
				PolicyRouteKey:       "high",
				PolicyArtifactID:     "future-prod",
				PolicyArtifactSHA256: "sha256:future",
			},
		},
		requests: make(chan router.Request, 1),
	}
	telem := newCaptureTelemetry()
	shadowStrategy := router.Strategy("future-policy")
	svc := proxy.NewService(
		&fakeRouter{decision: scorerDecision},
		map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}},
		nil, false, nil, nil, false,
		providers.ProviderAnthropic, "claude-haiku-4-5", telem,
	).WithPolicyStrategy(policy.StrategySpec{Strategy: shadowStrategy, Router: shadowRouter})

	ctx := context.WithValue(context.Background(), proxy.InstallationIDContextKey{}, installID)
	ctx = context.WithValue(ctx, proxy.ExternalIDContextKey{}, "org-1")
	ctx = context.WithValue(ctx, proxy.PolicyRolloutIDContextKey{}, "rollout-1")
	ctx = context.WithValue(ctx, proxy.PolicyTrainingAllowedContextKey{}, true)
	ctx = context.WithValue(ctx, proxy.PolicyDebugEnabledContextKey{}, true)
	ctx = context.WithValue(ctx, proxy.PolicyShadowStrategyContextKey{}, shadowStrategy)

	decision, err := svc.Route(ctx, router.Request{})

	require.NoError(t, err)
	assert.Equal(t, scorerDecision, decision)
	select {
	case <-shadowRouter.requests:
		t.Fatal("dry-run route must not call the shadow policy")
	default:
	}

	recorder := httptest.NewRecorder()
	body := []byte(`{"model":"claude-opus-4-7","tools":[],"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}}},"messages":[{"role":"user","content":"hello"}]}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, body, recorder, request))

	shadowRequest := <-shadowRouter.requests
	assert.True(t, shadowRequest.ShadowMode)
	assert.False(t, shadowRequest.TrainingAllowed)
	assert.False(t, shadowRequest.DebugEnabled)
	row := telem.firstShadowRow(t)
	assert.Equal(t, installID, row.InstallationID)
	assert.Equal(t, "org-1", row.OrganizationID)
	assert.Equal(t, "rollout-1", row.RolloutID)
	assert.True(t, row.TrainingAllowed)
	assert.Equal(t, "cluster", row.ServingStrategy)
	assert.Equal(t, serving.Model, row.ServingModel)
	assert.Equal(t, "future-policy", row.ShadowStrategy)
	assert.Equal(t, "gpt-5.5", row.ShadowModel)
	assert.Equal(t, "shadow-route-1", row.ShadowRouteID)
	assert.Equal(t, "future-prod", row.ShadowPolicyArtifactID)
	assert.False(t, row.ModelsAgree)
}

func TestPolicyShadowComparisonCollectsUsageBypassRoute(t *testing.T) {
	const installID = "77777777-7777-7777-7777-777777777777"
	shadowStrategy := router.Strategy("future-policy")
	shadowRouter := &shadowRequestRouter{
		decision: router.Decision{
			Provider: providers.ProviderOpenAI,
			Model:    "gpt-5.5",
		},
		requests: make(chan router.Request, 1),
	}
	telem := newCaptureTelemetry()
	observer := usage.NewObserver([]byte("salt"), 10*time.Minute, time.Now)
	observer.Record(observer.Key([]byte(bypassSubToken)), usage.Snapshot{
		Primary: usage.Window{UsedPercent: 0.20, WindowMinutes: 300},
	})
	svc := proxy.NewService(
		&fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: bypassScorerPickMdl}},
		map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}},
		nil, false, nil, nil, false,
		providers.ProviderAnthropic, bypassScorerPickMdl, telem,
	).WithSubscriptionAwareRouting(observer, 0.05, 2.0).
		WithPolicyStrategy(policy.StrategySpec{Strategy: shadowStrategy, Router: shadowRouter})

	ctx := bypassCtx(0.80)
	ctx = context.WithValue(ctx, proxy.InstallationIDContextKey{}, installID)
	ctx = context.WithValue(ctx, proxy.ExternalIDContextKey{}, "org-bypass")
	ctx = context.WithValue(ctx, proxy.PolicyTrainingAllowedContextKey{}, true)
	ctx = context.WithValue(ctx, proxy.PolicyShadowStrategyContextKey{}, shadowStrategy)
	recorder, request, body := bypassRequest(t)

	require.NoError(t, svc.ProxyMessages(ctx, body, recorder, request))

	row := telem.firstShadowRow(t)
	assert.Equal(t, installID, row.InstallationID)
	assert.Equal(t, bypassRequestedMdl, row.ServingModel)
	assert.Equal(t, "gpt-5.5", row.ShadowModel)
	assert.False(t, row.ModelsAgree)
}

// TestProxyMessages_RecordsClusterObservation asserts cluster-routed decisions
// surface every routing-brain field into the telemetry row (W-1339/W-1335).
func TestProxyMessages_RecordsClusterObservation(t *testing.T) {
	const installID = "11111111-1111-1111-1111-111111111111"
	decision := router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-haiku-4-5",
		Reason:   "cluster:v-test top_p=[3,7] model=claude-haiku-4-5 provider=anthropic",
		Metadata: &router.RoutingMetadata{
			ClusterIDs:           []int{3, 7},
			CandidateModels:      []string{"claude-opus-4-7", "claude-haiku-4-5"},
			ChosenScore:          0.85,
			ClusterRouterVersion: "v-test",
			CandidateScores:      map[string]float32{"claude-opus-4-7": 0.85, "claude-haiku-4-5": 0.42},
			Propensity:           1.0,
		},
	}
	telem := newCaptureTelemetry()
	svc := proxy.NewService(
		&fakeRouter{decision: decision},
		map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}},
		nil,
		false,
		nil,
		nil,
		false,
		providers.ProviderAnthropic, "claude-haiku-4-5",
		telem,
	)

	ctx := context.WithValue(context.Background(), proxy.InstallationIDContextKey{}, installID)
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hello"}]}`)
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, body, rec, httpReq))

	row := telem.firstRow(t)
	assert.Equal(t, installID, row.InstallationID)
	assert.Equal(t, "claude-haiku-4-5", row.DecisionModel)
	assert.Equal(t, []int32{3, 7}, row.ClusterIDs)
	assert.Equal(t, []string{"claude-opus-4-7", "claude-haiku-4-5"}, row.CandidateModels)
	require.NotNil(t, row.ChosenScore)
	assert.InDelta(t, 0.85, *row.ChosenScore, 1e-6)
	assert.Equal(t, "v-test", row.ClusterRouterVersion)
	// Propensity=&1.0 for the deterministic argmax scorer; candidate_scores is
	// the pre-argmax vector — off-policy logging substrate.
	require.NotNil(t, row.Propensity)
	assert.InDelta(t, 1.0, *row.Propensity, 1e-6)
	require.NotNil(t, row.CandidateScores)
	var gotScores map[string]float32
	require.NoError(t, json.Unmarshal(row.CandidateScores, &gotScores))
	assert.InDelta(t, 0.85, gotScores["claude-opus-4-7"], 1e-6)
	assert.InDelta(t, 0.42, gotScores["claude-haiku-4-5"], 1e-6)
	// AlphaBreakdown is a W-1335 forward-compat slot; Cache* are nil since the
	// fake provider returns no body.
	assert.Nil(t, row.AlphaBreakdown)
	assert.Nil(t, row.CacheCreationTokens)
	assert.Nil(t, row.CacheReadTokens)
}

func TestProxyMessages_RecordsPolicyObservation(t *testing.T) {
	const installID = "55555555-5555-5555-5555-555555555555"
	decision := router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-haiku-4-5",
		Reason:   "policy:hmm",
		Metadata: &router.RoutingMetadata{
			Strategy:             string(router.StrategyHMM),
			RouteID:              "route-1",
			PolicyRouteKey:       "medium|open",
			PolicyArtifactID:     "hmm-prod",
			PolicyArtifactSHA256: "sha256:abc",
			RosterVersion:        "roster-v2",
			SidecarSchemaVersion: "policy_router_v1",
			DebugRef:             "debug-1",
		},
	}
	telem := newCaptureTelemetry()
	svc := proxy.NewService(
		&fakeRouter{decision: decision},
		map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}},
		nil, false, nil, nil, false,
		providers.ProviderAnthropic, "claude-haiku-4-5",
		telem,
	).WithContentCapture(proxy.CaptureHashed, 0, nil)

	ctx := context.WithValue(context.Background(), proxy.InstallationIDContextKey{}, installID)
	ctx = context.WithValue(ctx, proxy.PolicyTrainingAllowedContextKey{}, true)
	ctx = context.WithValue(ctx, proxy.PolicyDebugEnabledContextKey{}, true)
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hello"}]}`)
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, body, rec, httpReq))

	row := telem.firstRow(t)
	assert.Equal(t, "hmm", row.Strategy)
	assert.Equal(t, "route-1", row.RouteID)
	assert.Equal(t, "medium|open", row.PolicyRouteKey)
	assert.Equal(t, "hmm-prod", row.PolicyArtifactID)
	assert.Equal(t, "sha256:abc", row.PolicyArtifactSHA256)
	assert.Equal(t, "roster-v2", row.RosterVersion)
	assert.Equal(t, "policy_router_v1", row.SidecarSchemaVersion)
	assert.True(t, row.TrainingAllowed)
	assert.Equal(t, "hashed", row.CaptureMode)
	assert.Equal(t, "debug-1", row.DebugRef)
}

// TestProxyMessages_PersistsCacheTokens: cache_creation/read_input_tokens
// reported upstream must land on the telemetry row (W-1309 regression guard).
func TestProxyMessages_PersistsCacheTokens(t *testing.T) {
	const installID = "44444444-4444-4444-4444-444444444444"
	decision := router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-haiku-4-5",
		Reason:   "pin",
	}
	telem := newCaptureTelemetry()
	provider := &fakeProvider{
		proxyResponse: func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-haiku-4-5","stop_reason":"end_turn","usage":{"input_tokens":120,"output_tokens":7,"cache_creation_input_tokens":512,"cache_read_input_tokens":2048}}`))
		},
	}
	svc := proxy.NewService(
		&fakeRouter{decision: decision},
		map[string]providers.Client{providers.ProviderAnthropic: provider},
		nil, false, nil, nil, false,
		providers.ProviderAnthropic, "claude-haiku-4-5",
		telem,
	)

	ctx := context.WithValue(context.Background(), proxy.InstallationIDContextKey{}, installID)
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hello"}]}`)
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, body, rec, httpReq))

	row := telem.firstRow(t)
	require.NotNil(t, row.CacheCreationTokens, "cache_creation_input_tokens must persist as *int32, not be dropped")
	assert.Equal(t, int32(512), *row.CacheCreationTokens)
	require.NotNil(t, row.CacheReadTokens, "cache_read_input_tokens must persist as *int32, not be dropped")
	assert.Equal(t, int32(2048), *row.CacheReadTokens)
}

// TestProxyMessages_ChosenScoreZeroIsPersisted: nil = not a cluster decision,
// &0 = legitimate zero. Guards against a `!= 0` simplification dropping zero scores.
func TestProxyMessages_ChosenScoreZeroIsPersisted(t *testing.T) {
	const installID = "33333333-3333-3333-3333-333333333333"
	decision := router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-haiku-4-5",
		Reason:   "cluster:v-test top_p=[0] model=claude-haiku-4-5 provider=anthropic",
		Metadata: &router.RoutingMetadata{
			ClusterIDs:           []int{0},
			CandidateModels:      []string{"claude-haiku-4-5"},
			ChosenScore:          0, // must persist as &0, not nil
			ClusterRouterVersion: "v-test",
		},
	}
	telem := newCaptureTelemetry()
	svc := proxy.NewService(
		&fakeRouter{decision: decision},
		map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}},
		nil, false, nil, nil, false,
		providers.ProviderAnthropic, "claude-haiku-4-5",
		telem,
	)

	ctx := context.WithValue(context.Background(), proxy.InstallationIDContextKey{}, installID)
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hello"}]}`)
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, body, rec, httpReq))

	row := telem.firstRow(t)
	require.NotNil(t, row.ChosenScore, "zero ChosenScore must persist as &0, not be dropped")
	assert.Equal(t, 0.0, *row.ChosenScore)
}

// TestProxyMessages_NoMetadataOmitsClusterFields: pinned/heuristic decisions
// leave routing-brain columns NULL rather than synthesizing fake values.
func TestProxyMessages_NoMetadataOmitsClusterFields(t *testing.T) {
	const installID = "22222222-2222-2222-2222-222222222222"
	require.NotEqual(t, uuid.Nil.String(), installID)

	decision := router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-haiku-4-5",
		Reason:   "pin",
		// Metadata intentionally nil; matches pinDecision shape.
	}
	telem := newCaptureTelemetry()
	svc := proxy.NewService(
		&fakeRouter{decision: decision},
		map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}},
		nil,
		false,
		nil,
		nil,
		false,
		providers.ProviderAnthropic, "claude-haiku-4-5",
		telem,
	)

	ctx := context.WithValue(context.Background(), proxy.InstallationIDContextKey{}, installID)
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hello"}]}`)
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, body, rec, httpReq))

	row := telem.firstRow(t)
	assert.Equal(t, "claude-haiku-4-5", row.DecisionModel)
	assert.Nil(t, row.ClusterIDs)
	assert.Nil(t, row.CandidateModels)
	assert.Nil(t, row.ChosenScore)
	assert.Empty(t, row.ClusterRouterVersion)
	// Non-cluster decisions carry no score vector or propensity.
	assert.Nil(t, row.CandidateScores)
	assert.Nil(t, row.Propensity)
}

// TestProxyMessages_PersistsTurnType asserts turntype classification lands on
// the telemetry row, so analytics can separate automated turns from user traffic.
func TestProxyMessages_PersistsTurnType(t *testing.T) {
	const installID = "55555555-5555-5555-5555-555555555555"
	decision := router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-haiku-4-5",
		Reason:   "pin",
	}

	cases := []struct {
		name     string
		body     string
		turnType string
	}{
		{
			name:     "main loop",
			body:     `{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hello"}]}`,
			turnType: "main_loop",
		},
		{
			name:     "probe",
			body:     `{"model":"claude-opus-4-7","max_tokens":1,"messages":[{"role":"user","content":"quota"}]}`,
			turnType: "probe",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			telem := newCaptureTelemetry()
			svc := proxy.NewService(
				&fakeRouter{decision: decision},
				map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}},
				nil, false, nil, nil, false,
				providers.ProviderAnthropic, "claude-haiku-4-5",
				telem,
			)

			ctx := context.WithValue(context.Background(), proxy.InstallationIDContextKey{}, installID)
			rec := httptest.NewRecorder()
			httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
			require.NoError(t, svc.ProxyMessages(ctx, []byte(tc.body), rec, httpReq))

			row := telem.firstRow(t)
			assert.Equal(t, tc.turnType, row.TurnType)
		})
	}
}

// TestProxyMessages_PersistsRolloutID asserts the persisted installation
// rollout takes precedence over a client harness id in telemetry.
func TestProxyMessages_PersistsRolloutID(t *testing.T) {
	const installID = "66666666-6666-6666-6666-666666666666"
	const rolloutID = "policy-rollout-1"
	decision := router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-haiku-4-5",
		Reason:   "pin",
	}
	telem := newCaptureTelemetry()
	svc := proxy.NewService(
		&fakeRouter{decision: decision},
		map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}},
		nil, false, nil, nil, false,
		providers.ProviderAnthropic, "claude-haiku-4-5",
		telem,
	)

	ctx := context.WithValue(context.Background(), proxy.InstallationIDContextKey{}, installID)
	ctx = context.WithValue(ctx, proxy.ClientIdentityContextKey{}, proxy.ClientIdentity{RolloutID: "client-rollout"})
	ctx = context.WithValue(ctx, proxy.PolicyRolloutIDContextKey{}, rolloutID)
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hello"}]}`)
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, body, rec, httpReq))

	row := telem.firstRow(t)
	assert.Equal(t, rolloutID, row.RolloutID)
}

func TestProxyMessages_PersistedPolicyRolloutIDOverridesClientIdentity(t *testing.T) {
	const installID = "66666666-6666-6666-6666-666666666666"
	const rolloutID = "policy-rollout-42"
	decision := router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-haiku-4-5",
		Reason:   "pin",
	}
	telem := newCaptureTelemetry()
	svc := proxy.NewService(
		&fakeRouter{decision: decision},
		map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}},
		nil, false, nil, nil, false,
		providers.ProviderAnthropic, "claude-haiku-4-5",
		telem,
	)

	ctx := context.WithValue(context.Background(), proxy.InstallationIDContextKey{}, installID)
	ctx = context.WithValue(ctx, proxy.ClientIdentityContextKey{}, proxy.ClientIdentity{RolloutID: "header-rollout"})
	ctx = context.WithValue(ctx, proxy.PolicyRolloutIDContextKey{}, rolloutID)
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hello"}]}`)
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, body, rec, httpReq))

	row := telem.firstRow(t)
	assert.Equal(t, rolloutID, row.RolloutID)
}

// TestProxyMessages_PersistsSessionKeyAndRole: session_key/role are the join
// key to router.spiral_shadow_events, so they must match spiral's encoding
// byte-for-byte. role is keyed off the *requested* model (opus-4-7 ->
// "default_high"), not the cheaper model the router actually picked.
func TestProxyMessages_PersistsSessionKeyAndRole(t *testing.T) {
	const installID = "77777777-7777-7777-7777-777777777777"
	decision := router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-haiku-4-5",
		Reason:   "pin",
	}
	telem := newCaptureTelemetry()
	svc := proxy.NewService(
		&fakeRouter{decision: decision},
		map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}},
		nil, false, nil, nil, false,
		providers.ProviderAnthropic, "claude-haiku-4-5",
		telem,
	)

	ctx := context.WithValue(context.Background(), proxy.InstallationIDContextKey{}, installID)
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hello"}]}`)
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, body, rec, httpReq))

	row := telem.firstRow(t)
	require.Len(t, row.SessionKey, 16, "session_key must be the 16-byte digest, not empty")
	assert.NotEqual(t, make([]byte, 16), row.SessionKey, "session_key must be a real (non-zero) digest")
	assert.Equal(t, "default_high", row.Role, "role must be the requested model's pin role (opus-4-7 = TierHigh)")
}

// TestNormalizeRolloutID covers the bound: oversized ids are rejected (not
// truncated) so a stored id always joins back to a real rollout.
func TestNormalizeRolloutID(t *testing.T) {
	assert.Equal(t, "run/cond/0/iid", proxy.NormalizeRolloutID("  run/cond/0/iid "))
	assert.Equal(t, "", proxy.NormalizeRolloutID(strings.Repeat("x", 257)))
	assert.Equal(t, strings.Repeat("x", 256), proxy.NormalizeRolloutID(strings.Repeat("x", 256)))
}
