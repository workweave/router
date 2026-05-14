package proxy_test

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
	"workweave/router/internal/router/cache"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// embeddingFixture returns a deterministic L2-normalized vector keyed by seed.
func embeddingFixture(seed float32) []float32 {
	v := []float32{seed, 1, 0, 0, 0, 0, 0, 0}
	var sum float32
	for _, x := range v {
		sum += x * x
	}
	if sum == 0 {
		return v
	}
	norm := float32(math.Sqrt(float64(sum)))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x / norm
	}
	return out
}

// anthropicBody returns a minimal valid Anthropic Messages body. Includes a
// stub tool so the request stays classified as MainLoop — without it the
// turntype detector would fingerprint it as Classifier (small max_tokens,
// no tools, short message list) and hard-pin past the semantic cache.
func anthropicBody(prompt string, stream bool) []byte {
	streamLit := "false"
	if stream {
		streamLit = "true"
	}
	return []byte(`{
		"model":"claude-opus-4-7",
		"max_tokens":256,
		"stream":` + streamLit + `,
		"tools":[{"name":"noop","description":"placeholder","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"` + prompt + `"}]
	}`)
}

// decisionWithEmbedding builds a routing decision with metadata needed for cache eligibility.
func decisionWithEmbedding(emb []float32, clusterIDs []int) router.Decision {
	return router.Decision{
		Provider: "anthropic",
		Model:    "claude-haiku-4-5",
		Reason:   "test",
		Metadata: &router.RoutingMetadata{
			Embedding:  emb,
			ClusterIDs: clusterIDs,
		},
	}
}

// proxyContextWithExternalID wires the per-tenant ID; without it the cache is bypassed.
func proxyContextWithExternalID(t *testing.T, externalID string) context.Context {
	t.Helper()
	ctx := context.Background()
	if externalID != "" {
		ctx = context.WithValue(ctx, proxy.ExternalIDContextKey{}, externalID)
	}
	return ctx
}

func TestService_Cache_HitShortCircuitsProvider(t *testing.T) {
	emb := embeddingFixture(1)
	provider := &fakeProvider{
		proxyResponse: func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"first","content":"hi"}`))
		},
	}
	fr := &fakeRouter{decision: decisionWithEmbedding(emb, []int{0, 1, 2, 3})}
	c := cache.New(cache.DefaultConfig())
	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: provider}, nil, false, c, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil)

	ctx := proxyContextWithExternalID(t, "tenant-1")
	body := anthropicBody("ping", false)

	rec1 := httptest.NewRecorder()
	httpReq1 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, body, rec1, httpReq1))
	require.Len(t, provider.proxyBodies, 1, "first call must hit the provider")

	rec2 := httptest.NewRecorder()
	httpReq2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, body, rec2, httpReq2))
	assert.Len(t, provider.proxyBodies, 1, "cache hit must not invoke provider a second time")

	assert.Equal(t, `{"id":"first","content":"hi"}`, rec2.Body.String())
	assert.Equal(t, "hit", rec2.Header().Get("x-router-cache"))
}

func TestService_Cache_StreamingBypasses(t *testing.T) {
	emb := embeddingFixture(2)
	provider := &fakeProvider{
		proxyResponse: func(w http.ResponseWriter) { _, _ = w.Write([]byte("event: stream-payload\n")) },
	}
	fr := &fakeRouter{decision: decisionWithEmbedding(emb, []int{0})}
	c := cache.New(cache.DefaultConfig())
	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: provider}, nil, false, c, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil)

	ctx := proxyContextWithExternalID(t, "tenant-1")
	body := anthropicBody("streaming please", true)

	rec1 := httptest.NewRecorder()
	httpReq1 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, body, rec1, httpReq1))

	rec2 := httptest.NewRecorder()
	httpReq2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, body, rec2, httpReq2))

	assert.Len(t, provider.proxyBodies, 2, "streaming requests must always hit the provider — no caching")
	assert.Empty(t, rec2.Header().Get("x-router-cache"), "streaming responses carry no x-router-cache marker")
}

func TestService_Cache_HeuristicDecisionBypasses(t *testing.T) {
	provider := &fakeProvider{
		proxyResponse: func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"id":"x"}`)) },
	}
	// Decision with no Metadata — what the heuristic router produces.
	fr := &fakeRouter{decision: router.Decision{Provider: "anthropic", Model: "claude-haiku-4-5", Reason: "heuristic"}}
	c := cache.New(cache.DefaultConfig())
	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: provider}, nil, false, c, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil)

	ctx := proxyContextWithExternalID(t, "tenant-1")
	body := anthropicBody("ask", false)

	rec1 := httptest.NewRecorder()
	httpReq1 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, body, rec1, httpReq1))

	rec2 := httptest.NewRecorder()
	httpReq2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, body, rec2, httpReq2))

	assert.Len(t, provider.proxyBodies, 2, "decisions without RoutingMetadata must not be cached")
}

func TestService_Cache_MissingExternalIDBypasses(t *testing.T) {
	emb := embeddingFixture(3)
	provider := &fakeProvider{
		proxyResponse: func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"id":"y"}`)) },
	}
	fr := &fakeRouter{decision: decisionWithEmbedding(emb, []int{0})}
	c := cache.New(cache.DefaultConfig())
	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: provider}, nil, false, c, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil)

	body := anthropicBody("ask", false)

	// No externalID → cache bypassed (per-tenant scope is the only isolation).
	ctx := context.Background()
	rec1 := httptest.NewRecorder()
	httpReq1 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, body, rec1, httpReq1))

	rec2 := httptest.NewRecorder()
	httpReq2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, body, rec2, httpReq2))

	assert.Len(t, provider.proxyBodies, 2, "without externalID the cache must not store or replay")
}

func TestService_Cache_DisabledByNilCache(t *testing.T) {
	emb := embeddingFixture(4)
	provider := &fakeProvider{
		proxyResponse: func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"id":"z"}`)) },
	}
	fr := &fakeRouter{decision: decisionWithEmbedding(emb, []int{0})}
	// nil cache equivalent to ROUTER_SEMANTIC_CACHE_ENABLED=false.
	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: provider}, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil)

	ctx := proxyContextWithExternalID(t, "tenant-1")
	body := anthropicBody("ask", false)

	rec1 := httptest.NewRecorder()
	httpReq1 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, body, rec1, httpReq1))

	rec2 := httptest.NewRecorder()
	httpReq2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, body, rec2, httpReq2))

	assert.Len(t, provider.proxyBodies, 2, "nil cache must be a transparent passthrough")
	assert.Empty(t, rec2.Header().Get("x-router-cache"))
}
