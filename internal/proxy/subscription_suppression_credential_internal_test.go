package proxy

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/providers"
)

const routerKeyedInstallationID = "11111111-1111-1111-1111-111111111111"

// routerKeyedCtx mimics an authenticated managed request: the auth middleware
// stashed an installation ID, so installationIDFromContext != nil and
// resolveAndInjectCredentials treats it as router-keyed.
func routerKeyedCtx() context.Context {
	return context.WithValue(context.Background(), InstallationIDContextKey{}, routerKeyedInstallationID)
}

// The exhaustion / 429 failover suppresses the Claude subscription and re-resolves
// credentials so the retry serves on the deployment ANTHROPIC_API_KEY. On a
// router-keyed request with no BYOK, none of the resolution branches match, and
// the ctx still carries the subscription credential injected on the primary
// attempt. resolveAndInjectCredentials MUST clear it — otherwise the retry
// re-sends the spent sk-ant-oat and 429s forever (the provider client only falls
// back to its deployment key when ctx carries no credential). This is the bug
// that made #519 / #555 no-ops for every managed customer.
func TestResolveAndInjectCredentials_SuppressedSubClearedOnRouterKeyedPath(t *testing.T) {
	// Primary attempt injected the caller's subscription onto ctx.
	spentSub := &Credentials{APIKey: []byte("sk-ant-oat01-spent"), Source: credSourceSubscription, OAuth: true}
	ctx := context.WithValue(routerKeyedCtx(), CredentialsContextKey{}, spentSub)

	// Failover suppresses the sub, then re-resolves for the retry. The spent
	// sk-ant-oat also rides in the inbound Authorization on the router-key path.
	ctx = withSuppressedClaudeSubscription(ctx)
	headers := http.Header{}
	headers.Set("Authorization", "Bearer sk-ant-oat01-spent")

	out := resolveAndInjectCredentials(ctx, providers.ProviderAnthropic, headers)

	assert.Nil(t, CredentialsFromContext(out),
		"a suppressed subscription must be cleared so the client uses its deployment key, not the spent token")
}

// Regression guard: without suppression, the normal path still forwards the
// caller's subscription so their Claude turns bill their own plan.
func TestResolveAndInjectCredentials_UnsuppressedSubStillForwarded(t *testing.T) {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer sk-ant-oat01-live")

	out := resolveAndInjectCredentials(routerKeyedCtx(), providers.ProviderAnthropic, headers)

	got := CredentialsFromContext(out)
	require.NotNil(t, got, "a live subscription must resolve on the router-key path")
	assert.Equal(t, credSourceSubscription, got.Source)
	assert.True(t, got.OAuth)
}

// The clear must be scoped to Anthropic: a suppressed-Claude request that also
// carries a Codex subscription for an OpenAI turn must still resolve the Codex
// credential (its OpenAI turns bill the caller's ChatGPT plan, unaffected).
func TestResolveAndInjectCredentials_SuppressionDoesNotClearCodexOpenAITurn(t *testing.T) {
	ctx := withSuppressedClaudeSubscription(routerKeyedCtx())
	headers := http.Header{}
	headers.Set("Authorization", "Bearer eyJhbGciOi.codex.jwt")
	headers.Set("ChatGPT-Account-ID", "acct-1")

	out := resolveAndInjectCredentials(ctx, providers.ProviderOpenAI, headers)

	got := CredentialsFromContext(out)
	require.NotNil(t, got, "a Codex subscription must still resolve for its OpenAI turn")
	assert.Equal(t, credSourceCodexSubscription, got.Source)
}
