package proxy_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	feedbacktoken "workweave/router/internal/feedback"
	"workweave/router/internal/observability/otel"
	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
	"workweave/router/internal/router/sessionpin"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
)

// spanCollector runs an httptest server that decodes OTLP/HTTP trace export
// requests and records every span it receives, keyed by name.
type spanCollector struct {
	srv    *httptest.Server
	mu     sync.Mutex
	byName map[string][]*tracev1.Span
}

func newSpanCollector(t *testing.T) *spanCollector {
	t.Helper()
	c := &spanCollector{byName: make(map[string][]*tracev1.Span)}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var req coltracepb.ExportTraceServiceRequest
		require.NoError(t, proto.Unmarshal(body, &req))
		c.mu.Lock()
		for _, rs := range req.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					c.byName[sp.Name] = append(c.byName[sp.Name], sp)
				}
			}
		}
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// attrString returns the string value of the named attribute on the span, or
// "" (and false) if it isn't present.
func attrString(sp *tracev1.Span, key string) (string, bool) {
	for _, kv := range sp.Attributes {
		if kv.Key != key {
			continue
		}
		if sv, ok := kv.Value.Value.(*commonv1.AnyValue_StringValue); ok {
			return sv.StringValue, true
		}
	}
	return "", false
}

func newTestEmitter(t *testing.T, endpoint string) *otel.Emitter {
	t.Helper()
	em, err := otel.NewEmitter(otel.EmitterConfig{
		Endpoint:      endpoint,
		Workers:       1,
		QueueSize:     100,
		BatchSize:     1,
		FlushInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = em.Shutdown(context.Background()) })
	return em
}

// TestFeedbackSpans_UseDistinctNamesAndSchemas guards against the two
// independent feedback pipelines colliding on the OTLP span name
// "router.feedback" (finding [28]): the signed-link pipeline
// (Service.SubmitFeedback) owns that name as a documented contract the Weave
// backend's buildFeedbackRow reads, while the /router-feedback slash-command
// pipeline must emit under its own distinct name with its own schema.
func TestFeedbackSpans_UseDistinctNamesAndSchemas(t *testing.T) {
	collector := newSpanCollector(t)
	emitter := newTestEmitter(t, collector.srv.URL)

	// --- Pipeline A: signed feedback-link submission ---
	repo := &fakeFeedbackRepoForSpanTest{}
	signer := feedbacktoken.NewSigner("secret", time.Hour)
	linkSvc := proxy.NewService(nil, nil, emitter, false, nil, nil, false, "", "", nil).
		WithFeedback(repo, signer, "https://router.example.com")

	err := linkSvc.SubmitFeedback(context.Background(), proxy.SubmitFeedbackParams{
		InstallationID: "inst-1",
		ExternalID:     "org-1",
		RequestID:      "req-1",
		RouterUserID:   "user-1",
		Rating:         "down",
	})
	require.NoError(t, err)

	// --- Pipeline B: /router-feedback slash command ---
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", LastServedModel: "claude-haiku-4-5"}
	feedbackStore := &fakeFeedbackStore{}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-6", Reason: "cluster"}}
	cmdSvc := proxy.NewService(
		fr,
		map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}},
		emitter,
		false,
		nil,
		store,
		false,
		providers.ProviderAnthropic,
		"claude-haiku-4-5",
		nil,
	).WithRouterFeedbackStore(feedbackStore)

	const body = `{
		"model":"claude-sonnet-4-6",
		"max_tokens":1024,
		"messages":[
			{"role":"user","content":"/router-feedback got stuck on Haiku for too long"}
		]
	}`
	ctx := context.WithValue(context.Background(), proxy.APIKeyIDContextKey{}, "key-1")
	ctx = context.WithValue(ctx, proxy.InstallationIDContextKey{}, uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, cmdSvc.ProxyMessages(ctx, []byte(body), rec, httpReq))

	// Force both buffers to flush and the emitter to export before asserting.
	require.NoError(t, emitter.Shutdown(context.Background()))

	collector.mu.Lock()
	defer collector.mu.Unlock()

	linkSpans := collector.byName["router.feedback"]
	cmdSpans := collector.byName["router.feedback.command"]
	require.Len(t, linkSpans, 1, "signed-link pipeline must emit exactly one router.feedback span")
	require.Len(t, cmdSpans, 1, "/router-feedback command must emit exactly one router.feedback.command span")

	// The documented-contract span (router.feedback) always carries a rating.
	rating, ok := attrString(linkSpans[0], "feedback.rating")
	assert.True(t, ok, "router.feedback span must carry feedback.rating (Weave contract)")
	assert.Equal(t, "down", rating)

	// The two pipelines must not collide under the same span name.
	assert.NotEqual(t, linkSpans[0].Name, cmdSpans[0].Name)

	// The command pipeline's span has its own, unrelated schema (e.g. carries
	// client.app, which the link pipeline's contract never emits).
	_, cmdHasClientApp := attrString(cmdSpans[0], "client.app")
	assert.True(t, cmdHasClientApp, "router.feedback.command span should carry client.app")
	_, linkHasClientApp := attrString(linkSpans[0], "client.app")
	assert.False(t, linkHasClientApp, "router.feedback span schema must stay untouched by the command pipeline's fields")
}

type fakeFeedbackRepoForSpanTest struct{}

func (f *fakeFeedbackRepoForSpanTest) Upsert(_ context.Context, _ proxy.UpsertFeedbackParams) error {
	return nil
}

func (f *fakeFeedbackRepoForSpanTest) GetContext(_ context.Context, _, _ string) (proxy.FeedbackContext, error) {
	return proxy.FeedbackContext{}, nil
}
