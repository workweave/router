package proxy_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workweave/router/internal/billing"
	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A Codex (ChatGPT) subscription bearer is a JWT-shaped token (not sk-/rk_)
// paired with a ChatGPT-Account-ID header; the pair resolves to an OAuth
// subscription credential the Codex backend serves for free.
const (
	codexSubToken     = "eyJhbGciOi.codex.jwt"
	codexSubAccountID = "acct-codex-123"
)

// codexSubRequest builds an OpenAI chat-completions request carrying a Codex
// subscription in the inbound Authorization header.
func codexSubRequest(t *testing.T, body string) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+codexSubToken)
	req.Header.Set("ChatGPT-Account-ID", codexSubAccountID)
	return httptest.NewRecorder(), req
}

// TestSubscriptionOnly_OpenAI_ServesOnCodexSub: below the overdraft floor, a
// Codex-covered turn routed to OpenAI must serve on the caller's own ChatGPT
// subscription (OAuth credential => $0 debit), dispatch exactly once (no paid
// failover), and surface the depleted-credits warning.
func TestSubscriptionOnly_OpenAI_ServesOnCodexSub(t *testing.T) {
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-4o", Reason: "test"}}
	p := &fakeProvider{proxyResponse: func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}}
	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderOpenAI: p}, nil, false, nil, nil, false, providers.ProviderOpenAI, "gpt-4o", nil)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"stream":true}`
	rec, req := codexSubRequest(t, body)

	ctx := billing.WithSubscriptionOnly(context.Background())
	require.NoError(t, svc.ProxyOpenAIChatCompletion(ctx, []byte(body), rec, req))

	require.Len(t, p.proxyBodies, 1, "the turn must serve on the subscription exactly once (no paid failover)")
	require.NotNil(t, p.proxyCreds[0], "the dispatch must carry the caller's subscription credential")
	assert.True(t, p.proxyCreds[0].OAuth, "the turn must be served on the caller's own Codex subscription so billing debits $0")
	assert.Contains(t, rec.Body.String(), "credits are depleted", "the customer must see the depleted-credits warning")
	assert.Contains(t, rec.Body.String(), "ChatGPT (Codex)", "the warning must name the Codex subscription")
	assert.Contains(t, rec.Body.String(), "router-credits", "the warning must surface the top-up CTA")
}

// TestSubscriptionOnly_OpenAI_PaidRoute_Refuses402: a Codex-covered request
// that routing resolves to a paid provider (not served on the subscription)
// must be refused with the credits-exhausted sentinel and never dispatched —
// the bug the Codex path previously had, where such a turn debited past the
// floor with no bound.
func TestSubscriptionOnly_OpenAI_PaidRoute_Refuses402(t *testing.T) {
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderOpenRouter, Model: "deepseek/deepseek-chat", Reason: "test"}}
	p := &fakeProvider{proxyResponse: func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion"}`)
	}}
	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderOpenRouter: p}, nil, false, nil, nil, false, providers.ProviderOpenAI, "gpt-4o", nil)

	// MainLoop-shaped (tools + large max_tokens) so the turn isn't classified as
	// a hard-pinned classifier turn; that would bypass the scorer and defeat the
	// paid-route scenario under test.
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"Refactor the auth middleware and add tests."}],"max_tokens":4096,"tools":[{"type":"function","function":{"name":"edit_file","parameters":{"type":"object"}}}]}`
	rec, req := codexSubRequest(t, body)

	ctx := billing.WithSubscriptionOnly(context.Background())
	err := svc.ProxyOpenAIChatCompletion(ctx, []byte(body), rec, req)
	require.Error(t, err)
	require.Positive(t, fr.routeCalls, "the scorer must be consulted so the decision is the paid route under test")
	assert.True(t, errors.Is(err, proxy.ErrCreditsExhaustedSubscriptionUnavailable),
		"a Codex-covered turn that routes to a paid model must be refused, not dispatched")
	assert.Empty(t, p.proxyBodies, "no paid dispatch may occur below the floor in subscription-only mode")
}

// TestSubscriptionOnly_OpenAI_SubFailurePreCommit_Refuses402: when the
// subscription attempt fails before any bytes reach the client (e.g. a 429),
// paid failover is disabled — the turn must dispatch exactly once and be
// refused with the controlled 402 rather than rerouted onto a paid model.
func TestSubscriptionOnly_OpenAI_SubFailurePreCommit_Refuses402(t *testing.T) {
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-4o", Reason: "test"}}
	p := &fakeProvider{proxyErr: &providers.UpstreamStatusError{Status: http.StatusTooManyRequests}}
	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderOpenAI: p}, nil, false, nil, nil, false, providers.ProviderOpenAI, "gpt-4o", nil)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`
	rec, req := codexSubRequest(t, body)

	ctx := billing.WithSubscriptionOnly(context.Background())
	err := svc.ProxyOpenAIChatCompletion(ctx, []byte(body), rec, req)
	require.Error(t, err)
	assert.True(t, errors.Is(err, proxy.ErrCreditsExhaustedSubscriptionUnavailable),
		"a pre-commit subscription failure must be refused with the controlled 402, not rerouted to a paid model")
	assert.Len(t, p.proxyBodies, 1, "only the subscription attempt may dispatch; no paid failover")
}
