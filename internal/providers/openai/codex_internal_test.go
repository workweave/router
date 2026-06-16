package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func codexCtx(token, accountID string) context.Context {
	return context.WithValue(context.Background(), proxy.CredentialsContextKey{}, &proxy.Credentials{
		APIKey:    []byte(token),
		AccountID: []byte(accountID),
		Source:    "codex_subscription",
		OAuth:     true,
	})
}

// TestProxy_CodexSubscriptionDispatch verifies a Codex (ChatGPT) subscription
// credential reroutes the upstream call to the Codex backend's /responses
// endpoint with the required auth + account-id + beta + originator headers, and
// forwards the prepared Responses body byte-for-byte.
func TestProxy_CodexSubscriptionDispatch(t *testing.T) {
	var gotPath, gotAuth, gotAccount, gotBeta, gotOriginator string
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAccount = r.Header.Get("ChatGPT-Account-ID")
		gotBeta = r.Header.Get("OpenAI-Beta")
		gotOriginator = r.Header.Get("originator")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
	}))
	defer upstream.Close()

	c := NewClient("deployment-key", upstream.URL)
	c.codexBaseURL = upstream.URL // point the Codex backend at the test server

	body := []byte(`{"model":"gpt-5.5","input":"hi","stream":true}`)
	prep := providers.PreparedRequest{Body: body, Endpoint: providers.EndpointResponses, Headers: make(http.Header)}
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(""))

	ctx := codexCtx("eyJhbGciOiJ-codex-jwt", "acct-12345")
	err := c.Proxy(ctx, router.Decision{Model: "gpt-5.5", Provider: providers.ProviderOpenAI}, prep, rec, clientReq)
	require.NoError(t, err)

	assert.Equal(t, "/responses", gotPath, "a Codex subscription turn must hit the Codex backend's /responses endpoint, not api.openai.com")
	assert.Equal(t, "Bearer eyJhbGciOiJ-codex-jwt", gotAuth, "the ChatGPT OAuth JWT must be sent as a Bearer token")
	assert.Equal(t, "acct-12345", gotAccount, "the ChatGPT-Account-ID header is required by the Codex backend")
	assert.Equal(t, "responses=experimental", gotBeta)
	assert.Equal(t, "codex_cli_rs", gotOriginator)
	assert.Empty(t, rec.Header().Get("x-api-key"))
	assert.Equal(t, body, gotBody, "the prepared Responses body must reach the Codex backend unchanged")
}

// TestProxy_CodexCredOnChatEndpointDoesNotMisroute guards the Bugbot finding:
// the Codex backend only accepts the Responses schema, so a chat-completions
// prep that happens to resolve a Codex credential must NOT be posted to the
// Codex /responses endpoint. The switch is gated on EndpointResponses.
func TestProxy_CodexCredOnChatEndpointDoesNotMisroute(t *testing.T) {
	var gotPath, gotAccount string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAccount = r.Header.Get("ChatGPT-Account-ID")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	c := NewClient("deployment-key", upstream.URL)
	c.codexBaseURL = "https://chatgpt.example.invalid" // must NOT be used for a chat body

	// EndpointChatCompletions (zero value) — a chat-shaped body.
	prep := providers.PreparedRequest{Body: []byte(`{"model":"gpt-5.5","messages":[]}`), Headers: make(http.Header)}
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))

	ctx := codexCtx("eyJhbGciOiJ-codex-jwt", "acct-12345")
	err := c.Proxy(ctx, router.Decision{Model: "gpt-5.5", Provider: providers.ProviderOpenAI}, prep, rec, clientReq)
	require.NoError(t, err)

	assert.Equal(t, "/v1/chat/completions", gotPath,
		"a chat-completions body must never be posted to the Codex /responses endpoint, even with a Codex credential")
	assert.Empty(t, gotAccount, "the Codex account-id header must not be set on a non-Responses dispatch")
}

// TestProxy_NoCodexCredHitsOpenAI confirms the Codex switch is gated on the
// subscription credential: a normal (deployment-key) request still targets
// api.openai.com and sends no ChatGPT-Account-ID header.
func TestProxy_NoCodexCredHitsOpenAI(t *testing.T) {
	var gotPath, gotAccount string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAccount = r.Header.Get("ChatGPT-Account-ID")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	c := NewClient("deployment-key", upstream.URL)
	c.codexBaseURL = "https://chatgpt.example.invalid" // must NOT be used

	prep := providers.PreparedRequest{Body: []byte(`{"model":"gpt-5.5"}`), Headers: make(http.Header)}
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))

	err := c.Proxy(context.Background(), router.Decision{Model: "gpt-5.5", Provider: providers.ProviderOpenAI}, prep, rec, clientReq)
	require.NoError(t, err)

	assert.Equal(t, "/v1/chat/completions", gotPath)
	assert.Empty(t, gotAccount, "a non-subscription request must not send the Codex account-id header")
}
