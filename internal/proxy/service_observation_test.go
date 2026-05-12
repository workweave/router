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
// notify channel is the rendezvous for the async goroutine.
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

// TestProxyMessages_RecordsClusterObservation asserts cluster-routed
// decisions surface every routing-brain field into the telemetry row.
// W-1339 / W-1335 depend on the writer populating them.
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
	assert.Equal(t, installID, row.InstallationID)
	assert.Equal(t, "claude-haiku-4-5", row.DecisionModel)
	assert.Equal(t, []int32{3, 7}, row.ClusterIDs)
	assert.Equal(t, []string{"claude-opus-4-7", "claude-haiku-4-5"}, row.CandidateModels)
	require.NotNil(t, row.ChosenScore)
	assert.InDelta(t, 0.85, *row.ChosenScore, 1e-6)
	assert.Equal(t, "v-test", row.ClusterRouterVersion)
	// AlphaBreakdown / Cache* are W-1335 / W-1309 forward-compat slots; nil here.
	assert.Nil(t, row.AlphaBreakdown)
	assert.Nil(t, row.CacheCreationTokens)
	assert.Nil(t, row.CacheReadTokens)
}

// TestProxyMessages_ChosenScoreZeroIsPersisted pins the *float64 semantics:
// nil = not a cluster decision, &0 = legitimate zero. Regression guard
// against a `!= 0` simplification that silently drops zero scores.
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
	assert.Nil(t, row.ClusterIDs)
	assert.Nil(t, row.CandidateModels)
	assert.Nil(t, row.ChosenScore)
	assert.Empty(t, row.ClusterRouterVersion)
}
