package proxy

import (
	"context"
	"net/http"
	"testing"

	"workweave/router/internal/auth"
	"workweave/router/internal/providers"
	"workweave/router/internal/proxy/usage"
	"workweave/router/internal/subscriptions"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePool is an in-memory SubscriptionPoolSource for testing rotation without
// a DB or OAuth endpoints.
type fakePool struct {
	byProvider map[string][]*subscriptions.Credential
	markedUsed []string
	err        error
}

func (f *fakePool) SelectCredential(_ context.Context, _, _, provider string, skip func(string) bool) (*subscriptions.Credential, error) {
	if f.err != nil {
		return nil, f.err
	}
	for _, cred := range f.byProvider[provider] {
		if skip != nil && skip(cred.ID) {
			continue
		}
		return cred, nil
	}
	return nil, nil
}

func (f *fakePool) PoolExists(_ context.Context, _, _, provider string) bool {
	return len(f.byProvider[provider]) > 0
}

// HasUsableCredential mirrors SelectCredential's skip walk but without the
// refresh side effects, matching the real service's side-effect-free probe.
func (f *fakePool) HasUsableCredential(_ context.Context, _, _, provider string, skip func(string) bool) bool {
	if f.err != nil {
		return false
	}
	for _, cred := range f.byProvider[provider] {
		if skip != nil && skip(cred.ID) {
			continue
		}
		return true
	}
	return false
}

func (f *fakePool) MarkUsed(id string) { f.markedUsed = append(f.markedUsed, id) }

// poolCtx builds a request context carrying an installation id and user email,
// the two identifiers pooled selection keys on.
func poolCtx() context.Context {
	ctx := context.WithValue(context.Background(), InstallationIDContextKey{}, uuid.New().String())
	return context.WithValue(ctx, ClientIdentityContextKey{}, ClientIdentity{Email: "dev@example.com"})
}

func anthropicPoolCred(id string) *subscriptions.Credential {
	return &subscriptions.Credential{
		ID:          id,
		Provider:    providers.ProviderAnthropic,
		AccessToken: []byte("sk-ant-oat01-pooled-" + id),
	}
}

func TestResolveAndInjectCredentials_PooledUsedWhenInboundExhausted(t *testing.T) {
	// Inbound Claude subscription is suppressed-as-exhausted; a healthy pooled
	// account exists. Resolution must inject the pooled credential (OAuth, billed
	// as subscription-served), not fall straight through to a Weave key.
	pool := &fakePool{byProvider: map[string][]*subscriptions.Credential{
		providers.ProviderAnthropic: {anthropicPoolCred("cred-1")},
	}}
	s := &Service{subscriptionPool: pool}
	ctx := withSuppressedClaudeSubscription(poolCtx())
	headers := http.Header{"Authorization": []string{"Bearer sk-ant-oat01-spent"}}

	out := s.resolveAndInjectCredentials(ctx, providers.ProviderAnthropic, headers)
	creds := CredentialsFromContext(out)
	require.NotNil(t, creds)
	assert.True(t, creds.OAuth)
	assert.Equal(t, credSourcePooledSubscription, creds.Source)
	assert.Equal(t, "cred-1", creds.PoolCredentialID)
	assert.True(t, servedOnSubscription(out), "a pooled subscription turn bills at the subscription rate")
	assert.Equal(t, []string{"cred-1"}, pool.markedUsed)
}

func TestResolveAndInjectCredentials_InboundSubWinsOverPool(t *testing.T) {
	// A healthy inbound subscription outranks the pool — the pool is overflow, not
	// a replacement.
	pool := &fakePool{byProvider: map[string][]*subscriptions.Credential{
		providers.ProviderAnthropic: {anthropicPoolCred("cred-1")},
	}}
	s := &Service{subscriptionPool: pool}
	ctx := context.WithValue(poolCtx(), AnthropicSubscriptionContextKey{}, "sk-ant-oat01-inbound")

	out := s.resolveAndInjectCredentials(ctx, providers.ProviderAnthropic, http.Header{})
	creds := CredentialsFromContext(out)
	require.NotNil(t, creds)
	assert.Equal(t, credSourceSubscription, creds.Source, "inbound sub must win")
	assert.Empty(t, pool.markedUsed, "pool must not be touched when the inbound sub serves")
}

func TestResolveAndInjectCredentials_ExhaustedPoolEntrySkipped(t *testing.T) {
	// The first pooled account is observed-exhausted; selection must advance to
	// the next one.
	obs := observerWithSnapshot("unused", usage.Snapshot{})
	s := &Service{subscriptionPool: &fakePool{byProvider: map[string][]*subscriptions.Credential{
		providers.ProviderAnthropic: {anthropicPoolCred("cred-1"), anthropicPoolCred("cred-2")},
	}}, usageObserver: obs}
	// Mark cred-1 exhausted under its pool-namespaced key.
	obs.Record(s.poolObserverKey("cred-1"), exhaustedSnapshot())

	ctx := withSuppressedClaudeSubscription(poolCtx())
	out := s.resolveAndInjectCredentials(ctx, providers.ProviderAnthropic, http.Header{})
	creds := CredentialsFromContext(out)
	require.NotNil(t, creds)
	assert.Equal(t, "cred-2", creds.PoolCredentialID, "exhausted cred-1 must be skipped")
}

func TestResolveAndInjectCredentials_AllPoolExhaustedFallsToBYOK(t *testing.T) {
	obs := observerWithSnapshot("unused", usage.Snapshot{})
	s := &Service{subscriptionPool: &fakePool{byProvider: map[string][]*subscriptions.Credential{
		providers.ProviderAnthropic: {anthropicPoolCred("cred-1")},
	}}, usageObserver: obs}
	obs.Record(s.poolObserverKey("cred-1"), exhaustedSnapshot())

	ctx := context.WithValue(withSuppressedClaudeSubscription(poolCtx()), ExternalAPIKeysContextKey{},
		[]*auth.ExternalAPIKey{{Provider: providers.ProviderAnthropic, Plaintext: []byte("sk-ant-api-byok")}})
	out := s.resolveAndInjectCredentials(ctx, providers.ProviderAnthropic, http.Header{})
	creds := CredentialsFromContext(out)
	require.NotNil(t, creds)
	assert.False(t, creds.OAuth)
	assert.Equal(t, []byte("sk-ant-api-byok"), creds.APIKey, "all pool exhausted → BYOK")
}

func TestResolveAndInjectCredentials_PoolBypassedWithoutIdentity(t *testing.T) {
	pool := &fakePool{byProvider: map[string][]*subscriptions.Credential{
		providers.ProviderAnthropic: {anthropicPoolCred("cred-1")},
	}}
	s := &Service{subscriptionPool: pool}
	// No installation id / email on ctx → pool must not be consulted.
	ctx := withSuppressedClaudeSubscription(context.Background())

	out := s.resolveAndInjectCredentials(ctx, providers.ProviderAnthropic, http.Header{})
	assert.Nil(t, CredentialsFromContext(out))
	assert.Empty(t, pool.markedUsed)
}

func TestResolveAndInjectCredentials_SubDisabledSuppressesPool(t *testing.T) {
	pool := &fakePool{byProvider: map[string][]*subscriptions.Credential{
		providers.ProviderAnthropic: {anthropicPoolCred("cred-1")},
	}}
	s := &Service{subscriptionPool: pool}
	ctx := context.WithValue(poolCtx(), InstallationSubscriptionRoutingDisabledContextKey{}, true)

	out := s.resolveAndInjectCredentials(ctx, providers.ProviderAnthropic, http.Header{})
	assert.Nil(t, CredentialsFromContext(out), "org toggle off suppresses the pool too")
	assert.Empty(t, pool.markedUsed)
}

func TestResolveAndInjectCredentials_PoolErrorFallsThrough(t *testing.T) {
	// A pool DB error must never fail the turn: resolution falls through to BYOK.
	s := &Service{subscriptionPool: &fakePool{err: assertErr}}
	ctx := context.WithValue(withSuppressedClaudeSubscription(poolCtx()), ExternalAPIKeysContextKey{},
		[]*auth.ExternalAPIKey{{Provider: providers.ProviderAnthropic, Plaintext: []byte("sk-ant-api-byok")}})

	out := s.resolveAndInjectCredentials(ctx, providers.ProviderAnthropic, http.Header{})
	creds := CredentialsFromContext(out)
	require.NotNil(t, creds)
	assert.Equal(t, []byte("sk-ant-api-byok"), creds.APIKey)
}

func TestPoolHasUsableCandidate_HealthyRowIsAFallback(t *testing.T) {
	// A pool with a non-exhausted account is a usable fallback.
	s := &Service{subscriptionPool: &fakePool{byProvider: map[string][]*subscriptions.Credential{
		providers.ProviderAnthropic: {anthropicPoolCred("cred-1")},
	}}}
	assert.True(t, s.poolHasUsableCandidate(poolCtx(), providers.ProviderAnthropic))
	assert.True(t, s.anthropicFallbackKeyAvailable(poolCtx()))
}

func TestAnthropicFallbackKeyAvailable_AllPoolExhaustedIsNotAFallback(t *testing.T) {
	// A pool whose only account is observed-exhausted is NOT a usable fallback:
	// suppressing the inbound subscription would leave the turn with no working
	// credential and 400. anthropicFallbackKeyAvailable must report false so the
	// caller keeps the spent inbound subscription and preserves the upstream 429.
	obs := observerWithSnapshot("unused", usage.Snapshot{})
	s := &Service{subscriptionPool: &fakePool{byProvider: map[string][]*subscriptions.Credential{
		providers.ProviderAnthropic: {anthropicPoolCred("cred-1")},
	}}, usageObserver: obs}
	obs.Record(s.poolObserverKey("cred-1"), exhaustedSnapshot())

	assert.False(t, s.poolHasUsableCandidate(poolCtx(), providers.ProviderAnthropic),
		"an all-exhausted pool has no usable candidate")
	assert.False(t, s.anthropicFallbackKeyAvailable(poolCtx()),
		"an all-exhausted pool with no BYOK/deployment key is not a fallback")
}

func TestPooledCredentialFor_CodexSetsAccountID(t *testing.T) {
	s := &Service{subscriptionPool: &fakePool{byProvider: map[string][]*subscriptions.Credential{
		providers.ProviderOpenAI: {{
			ID:               "cred-gpt",
			Provider:         providers.ProviderOpenAI,
			AccessToken:      []byte("chatgpt-jwt"),
			ChatGPTAccountID: "acct-42",
		}},
	}}}
	creds := s.pooledCredentialFor(poolCtx(), providers.ProviderOpenAI)
	require.NotNil(t, creds)
	assert.Equal(t, credSourcePooledCodexSubscription, creds.Source)
	assert.Equal(t, []byte("acct-42"), creds.AccountID)
}

func TestPoolObserverKey_SurvivesTokenRotation(t *testing.T) {
	// A pooled credential is keyed by its row UUID, not its token — so an
	// exhaustion snapshot recorded before a refresh is still found afterward,
	// even though the access token changed.
	obs := observerWithSnapshot("unused", usage.Snapshot{})
	s := &Service{usageObserver: obs}
	obs.Record(s.poolObserverKey("cred-1"), exhaustedSnapshot())

	snap, ok := obs.Snapshot(s.poolObserverKey("cred-1"))
	require.True(t, ok, "the pool-keyed snapshot must be readable after the token rotates")
	assert.True(t, snap.Exhausted())
}

func TestRecordPoolExhaustionIfPooled(t *testing.T) {
	obs := observerWithSnapshot("unused", usage.Snapshot{})
	s := &Service{usageObserver: obs}
	ctx := context.WithValue(context.Background(), CredentialsContextKey{}, &Credentials{
		OAuth: true, PoolCredentialID: "cred-9",
	})
	s.recordPoolExhaustionIfPooled(ctx)

	snap, ok := obs.Snapshot(s.poolObserverKey("cred-9"))
	require.True(t, ok)
	assert.True(t, snap.Exhausted(), "a failed pooled turn marks its account exhausted so failover advances")
}

var assertErr = &poolTestError{}

type poolTestError struct{}

func (*poolTestError) Error() string { return "pool boom" }
