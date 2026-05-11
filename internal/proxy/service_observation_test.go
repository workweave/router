package proxy_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureTelemetry records every InsertTelemetryParams the proxy fires.
// fireTelemetry runs in a goroutine, so the channel is the rendezvous
// the tests block on. Cap=4 so a chatty test never blocks the proxy
// path; tests only inspect the first row.
type captureTelemetry struct {
	mu     sync.Mutex
	rows   []proxy.InsertTelemetryParams
	notify chan struct{}
}

func newCaptureTelemetry() *captureTelemetry {
	return &captureTelemetry{notify: make(chan struct{}, 4)}
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

// TestProxyMessages_RecordsClusterObservation asserts that cluster-routed
// decisions surface every routing-brain field into the telemetry row.
// W-1308 introduced these columns; W-1339 (ClickHouse pipe) and W-1335
// (3-axis α-blend) depend on the writer populating them. If any field
// stops landing, those downstream tickets get NULL columns and fail to
// produce useful analytics.
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
		},
	}
	telem := newCaptureTelemetry()
	svc := proxy.NewService(
		&fakeRouter{decision: decision},
		map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}},
		nil,   // emitter
		false, // embedLastUserMessage
		0,     // stickyDecisionTTL
		nil,   // semanticCache
		nil,   // pinStore
		false, // hardPinExplore
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
	assert.Equal(t, []int32{3, 7}, row.ClusterIDs,
		"cluster_ids must mirror the scorer's top-p set so analytics can group by cluster")
	assert.Equal(t, []string{"claude-opus-4-7", "claude-haiku-4-5"}, row.CandidateModels,
		"candidate_models must record the eligible argmax set for margin analysis")
	require.NotNil(t, row.ChosenScore, "chosen_score must be persisted for non-zero scores")
	assert.InDelta(t, 0.85, *row.ChosenScore, 1e-6)
	assert.Equal(t, "v-test", row.ClusterRouterVersion,
		"cluster_router_version is the join key for comparing artifact versions on the same traffic")
	// AlphaBreakdown / CacheCreationTokens / CacheReadTokens are forward-
	// compat slots populated by W-1335 / W-1309; this PR leaves them nil.
	assert.Nil(t, row.AlphaBreakdown)
	assert.Nil(t, row.CacheCreationTokens)
	assert.Nil(t, row.CacheReadTokens)
}

// TestProxyMessages_ChosenScoreZeroIsPersisted guards the semantics of
// the *float64 ChosenScore pointer: nil means "not a cluster decision"
// and a non-nil &0 means "argmax produced a literal zero". Earlier
// code collapsed these with a `!= 0` guard, silently dropping
// legitimate zero scores. Cursor Bugbot caught the bug; this test
// pins it so a future "simplification" can't reintroduce it.
func TestProxyMessages_ChosenScoreZeroIsPersisted(t *testing.T) {
	const installID = "33333333-3333-3333-3333-333333333333"
	decision := router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-haiku-4-5",
		Reason:   "cluster:v-test top_p=[0] model=claude-haiku-4-5 provider=anthropic",
		Metadata: &router.RoutingMetadata{
			ClusterIDs:           []int{0},
			CandidateModels:      []string{"claude-haiku-4-5"},
			ChosenScore:          0, // legitimate zero — must persist as &0, not nil
			ClusterRouterVersion: "v-test",
		},
	}
	telem := newCaptureTelemetry()
	svc := proxy.NewService(
		&fakeRouter{decision: decision},
		map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}},
		nil, false, 0, nil, nil, false,
		providers.ProviderAnthropic, "claude-haiku-4-5",
		telem,
	)

	ctx := context.WithValue(context.Background(), proxy.InstallationIDContextKey{}, installID)
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hello"}]}`)
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, body, rec, httpReq))

	row := telem.firstRow(t)
	require.NotNil(t, row.ChosenScore,
		"zero-valued ChosenScore on a cluster-routed decision must persist as &0, not be dropped to nil")
	assert.Equal(t, 0.0, *row.ChosenScore)
}

// TestProxyMessages_NoMetadataOmitsClusterFields covers the pinned-route /
// heuristic path: when the router returns a Decision without Metadata,
// the telemetry row must leave the routing-brain columns empty rather
// than synthesizing fake values. The DB row exists (cost / latency still
// matter), but cluster_ids et al. are NULL.
func TestProxyMessages_NoMetadataOmitsClusterFields(t *testing.T) {
	const installID = "22222222-2222-2222-2222-222222222222"
	require.NotEqual(t, uuid.Nil.String(), installID)

	decision := router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-haiku-4-5",
		Reason:   "pin",
		// Metadata intentionally nil — same shape pinDecision produces.
	}
	telem := newCaptureTelemetry()
	svc := proxy.NewService(
		&fakeRouter{decision: decision},
		map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}},
		nil,
		false,
		0,
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
	assert.Nil(t, row.ClusterIDs, "non-cluster decisions leave cluster_ids NULL")
	assert.Nil(t, row.CandidateModels)
	assert.Nil(t, row.ChosenScore)
	assert.Empty(t, row.ClusterRouterVersion)
}
