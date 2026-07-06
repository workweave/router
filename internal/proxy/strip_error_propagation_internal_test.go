package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stripFailureRouter never gets called: the strip failure must abort the
// turn before routing runs.
type stripFailureRouter struct {
	routeCalls int
}

func (r *stripFailureRouter) Route(context.Context, router.Request) (router.Decision, error) {
	r.routeCalls++
	return router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5"}, nil
}

// stripFailureProvider never gets called either, for the same reason.
type stripFailureProvider struct {
	proxyCalls int
}

func (p *stripFailureProvider) Proxy(context.Context, router.Decision, providers.PreparedRequest, http.ResponseWriter, *http.Request) error {
	p.proxyCalls++
	return nil
}

func (p *stripFailureProvider) Passthrough(context.Context, providers.PreparedRequest, http.ResponseWriter, *http.Request) error {
	return nil
}

// TestProxyOpenAIChatCompletion_StripRoutingMarkerFailure_ReturnsError guards
// [23]: ProxyOpenAIChatCompletion used to log a strip failure and proceed
// with the unstripped body instead of aborting, unlike ProxyMessages. Force
// the seam to fail and assert the OpenAI path now returns the error (and
// never dispatches upstream) just like the Anthropic path.
func TestProxyOpenAIChatCompletion_StripRoutingMarkerFailure_ReturnsError(t *testing.T) {
	wantErr := errors.New("boom: strip failed")
	prevStrip := stripRoutingMarkerFromMessages
	stripRoutingMarkerFromMessages = func(body []byte) ([]byte, error) { return nil, wantErr }
	defer func() { stripRoutingMarkerFromMessages = prevStrip }()

	fr := &stripFailureRouter{}
	fp := &stripFailureProvider{}
	svc := NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: fp}, nil, false, nil, nil, false,
		providers.ProviderAnthropic, "claude-haiku-4-5", nil)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))

	err := svc.ProxyOpenAIChatCompletion(context.Background(), []byte(body), rec, httpReq)

	require.ErrorIs(t, err, wantErr, "strip failure must propagate instead of being swallowed")
	assert.Zero(t, fr.routeCalls, "must abort before routing runs on strip failure")
	assert.Zero(t, fp.proxyCalls, "must abort before dispatching upstream on strip failure")
}

// TestProxyMessages_StripRoutingMarkerFailure_ReturnsError is the existing
// Anthropic-path behavior [23] fixes ProxyOpenAIChatCompletion to match.
func TestProxyMessages_StripRoutingMarkerFailure_ReturnsError(t *testing.T) {
	wantErr := errors.New("boom: strip failed")
	prevStrip := stripRoutingMarkerFromMessages
	stripRoutingMarkerFromMessages = func(body []byte) ([]byte, error) { return nil, wantErr }
	defer func() { stripRoutingMarkerFromMessages = prevStrip }()

	fr := &stripFailureRouter{}
	fp := &stripFailureProvider{}
	svc := NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: fp}, nil, false, nil, nil, false,
		providers.ProviderAnthropic, "claude-haiku-4-5", nil)

	body := `{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))

	err := svc.ProxyMessages(context.Background(), []byte(body), rec, httpReq)

	require.ErrorIs(t, err, wantErr, "strip failure must propagate")
	assert.Zero(t, fr.routeCalls, "must abort before routing runs on strip failure")
	assert.Zero(t, fp.proxyCalls, "must abort before dispatching upstream on strip failure")
}
