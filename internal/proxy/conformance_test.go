package proxy_test

// Translation-conformance suite. Each case drives a full ProxyMessages turn
// against a mock provider endpoint (newMockUpstream) and asserts BOTH directions
// of translation: the request the router sent upstream (wantUpstream, explicit
// field checks) and the Anthropic response it handed back (golden, full-output
// compare). The mock and golden infra live in conformance_mock_test.go and
// conformance_golden_test.go. Cases are grouped by upstream wire format in the
// conformance_<format>_test.go files.
//
// Adding a case when a new provider quirk surfaces: author an upstream fixture
// under testdata/conformance/<format>/, add a conformanceCase asserting the
// outbound field that should change, run `go test -update`, eyeball the golden.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"

	"github.com/stretchr/testify/require"
)

// routingMarkerHeader opts a request out of the per-turn routing badge so goldens
// don't carry the volatile marker block (mirrors proxy.routingMarkerHeader).
const routingMarkerHeader = "X-Weave-Routing-Marker"

type conformanceCase struct {
	// name is the subtest name and the testdata golden path, e.g. "openai_chat/basic_text".
	name string
	// provider is both the router.Decision.Provider and the providerMap key.
	provider string
	// model is the router.Decision.Model — must be a real catalog id so
	// router.Lookup resolves the right capabilities (the Responses-API dispatch
	// gate keys on CapReasoning).
	model string
	// newClient builds the real provider client pointed at the mock base URL.
	newClient func(baseURL string) providers.Client
	// inbound is the Anthropic Messages request body the client sends.
	inbound string
	// stream is the inbound stream flag; the mock replays the fixture in the
	// matching mode (SSE when true, one JSON body when false).
	stream bool
	// upstreamFixture is the testdata path (under testdata/conformance) of the
	// canned provider response the mock replays.
	upstreamFixture string
	// upstreamStatus overrides the mock response status (0 => 200).
	upstreamStatus int
	// wantUpstream asserts the request the router actually sent the provider.
	wantUpstream func(t *testing.T, path string, body []byte, hdr http.Header)
}

func runConformanceCase(t *testing.T, c conformanceCase) {
	t.Helper()
	fixture := readFixture(t, c.upstreamFixture)
	// The mock serves SSE iff the fixture is an SSE file. This is decoupled from
	// the inbound stream flag on purpose: the OpenAI Responses path always streams
	// upstream (stream:true) even for a non-streaming client, so a .sse fixture
	// pairs with a stream:false inbound there.
	mockStreaming := strings.HasSuffix(c.upstreamFixture, ".sse")
	mock, srv := newMockUpstream(t, mockStreaming, c.upstreamStatus, fixture)

	svc := proxy.NewService(
		&fakeRouter{decision: router.Decision{Provider: c.provider, Model: c.model}},
		map[string]providers.Client{c.provider: c.newClient(srv.URL)},
		nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil,
	).WithDeploymentKeyedProviders(map[string]struct{}{c.provider: {}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	req.Header.Set(routingMarkerHeader, "off")
	require.NoError(t, svc.ProxyMessages(context.Background(), []byte(c.inbound), rec, req),
		"ProxyMessages should succeed for a healthy upstream")

	if c.wantUpstream != nil {
		path, body, hdr := mock.captured(t)
		c.wantUpstream(t, path, body, hdr)
	}
	golden(t, c.name, normalizeResponse(t, rec.Header().Get("Content-Type"), rec.Body.Bytes()))
}
