package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"weave-os/router/internal/auth"
	"weave-os/router/internal/billing"
	"weave-os/router/internal/flags"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/subscriptions"
)

type scriptedSubscriptionLeaser struct {
	leases      []subscriptions.Lease
	next        int
	providers   []subscriptions.Provider
	cooldownIDs []string
	disabledIDs []string
}

func (s *scriptedSubscriptionLeaser) Lease(_ context.Context, _ string, provider subscriptions.Provider, _ string) (subscriptions.Lease, bool, error) {
	s.providers = append(s.providers, provider)
	if s.next >= len(s.leases) {
		return subscriptions.Lease{}, true, subscriptions.ErrNoAvailableAccount
	}
	lease := s.leases[s.next]
	s.next++
	return lease, true, nil
}

func (s *scriptedSubscriptionLeaser) Cooldown(_ context.Context, _ string, _ subscriptions.Provider, accountID string, _ time.Time) error {
	s.cooldownIDs = append(s.cooldownIDs, accountID)
	return nil
}

func (s *scriptedSubscriptionLeaser) Disable(_ context.Context, _ string, _ subscriptions.Provider, accountID string) error {
	s.disabledIDs = append(s.disabledIDs, accountID)
	return nil
}

func managedSubscriptionTestContext() context.Context {
	return managedSubscriptionContext(auth.SubscriptionProviderClaude)
}

func managedSubscriptionContext(provider auth.SubscriptionProvider) context.Context {
	ctx := context.WithValue(context.Background(), APIKeyIDContextKey{}, "key-1")
	ctx = context.WithValue(ctx, ManagedSubscriptionProvidersContextKey{}, map[auth.SubscriptionProvider]struct{}{
		provider: {},
	})
	return WithManagedSubscriptionUsage(ctx)
}

func TestDispatchWithFallbackUsesOnlyMatchingManagedProviderFamily(t *testing.T) {
	leaser := &scriptedSubscriptionLeaser{leases: []subscriptions.Lease{{AccountID: "opaque-codex", AccessToken: "token-codex"}}}
	client := &fakeClient{name: providers.ProviderOpenAI, outcomes: []fakeOutcome{{writeBytes: []byte("served")}}}
	svc := newServiceWithProviders(t, map[string]providers.Client{providers.ProviderOpenAI: client}).WithManagedSubscriptions(leaser)
	recorder := httptest.NewRecorder()
	buffer := newPreludeBuffer(recorder)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	_, err := svc.dispatchWithFallback(managedSubscriptionContext(auth.SubscriptionProviderCodex), failoverInputs{
		w: recorder, buf: buffer,
		initialDecision: router.Decision{Model: "gpt-5.6-sol", Provider: providers.ProviderOpenAI},
		bindings:        []catalog.ProviderBinding{{Provider: providers.ProviderOpenAI}},
		attempt: func(ctx context.Context, decision router.Decision, client providers.Client) error {
			require.Equal(t, "token-codex", string(CredentialsFromContext(ctx).APIKey))
			buffer.Seal()
			return client.Proxy(ctx, decision, providers.PreparedRequest{}, buffer, request)
		},
	})

	require.NoError(t, err)
	require.Equal(t, []subscriptions.Provider{subscriptions.ProviderCodex}, leaser.providers)
	require.Equal(t, "served", recorder.Body.String())
}

func TestDispatchWithFallbackDoesNotCrossManagedProviderFamilies(t *testing.T) {
	leaser := &scriptedSubscriptionLeaser{leases: []subscriptions.Lease{{AccountID: "opaque-claude", AccessToken: "token-claude"}}}
	svc := newServiceWithProviders(t, nil).WithManagedSubscriptions(leaser)

	ctx, _, managed, err := svc.leaseManagedSubscription(
		managedSubscriptionContext(auth.SubscriptionProviderClaude),
		providers.ProviderOpenAI,
		"gpt-5.6-sol",
	)

	require.NoError(t, err)
	require.False(t, managed)
	require.Nil(t, CredentialsFromContext(ctx))
	require.Empty(t, leaser.providers)
}

func TestManagedSubscriptionOverridesBYOKButNotInboundOAuth(t *testing.T) {
	leaser := &scriptedSubscriptionLeaser{leases: []subscriptions.Lease{{AccountID: "opaque-claude", AccessToken: "managed-token"}}}
	svc := newServiceWithProviders(t, nil).WithManagedSubscriptions(leaser)
	base := managedSubscriptionTestContext()

	byokCtx := context.WithValue(base, CredentialsContextKey{}, &Credentials{APIKey: []byte("byok-token"), Source: credSourceBYOK})
	managedCtx, managedLease, managed, err := svc.leaseManagedSubscription(byokCtx, providers.ProviderAnthropic, "claude-opus-4-8")
	require.NoError(t, err)
	require.True(t, managed)
	require.Equal(t, "managed-token", string(CredentialsFromContext(managedCtx).APIKey))
	managedLease.Release()

	oauth := &Credentials{APIKey: []byte("inbound-oauth"), Source: credSourceSubscription, OAuth: true}
	oauthCtx := context.WithValue(base, CredentialsContextKey{}, oauth)
	unchangedCtx, _, managed, err := svc.leaseManagedSubscription(oauthCtx, providers.ProviderAnthropic, "claude-opus-4-8")
	require.NoError(t, err)
	require.False(t, managed)
	require.Same(t, oauth, CredentialsFromContext(unchangedCtx))
}

func TestManagedSubscriptionEnrollmentFailureFailsClosedAtLease(t *testing.T) {
	leaser := &scriptedSubscriptionLeaser{}
	svc := newServiceWithProviders(t, nil).WithManagedSubscriptions(leaser)
	ctx := context.WithValue(context.Background(), ManagedSubscriptionEnrollmentUnavailableContextKey{}, true)

	_, _, managed, err := svc.leaseManagedSubscription(ctx, providers.ProviderAnthropic, "claude-opus-4-8")
	require.ErrorIs(t, err, ErrSubscriptionPoolUnavailable)
	require.True(t, managed)
	require.Empty(t, leaser.providers)
}

func TestManagedSubscriptionAllPlansExhaustedFallsThroughToNormalRouting(t *testing.T) {
	leaser := &scriptedSubscriptionLeaser{}
	svc := newServiceWithProviders(t, nil).
		WithManagedSubscriptions(leaser)
	ctx := managedSubscriptionTestContext()
	ctx = flags.WithOverrides(ctx, flags.Overrides{Bools: map[flags.Key]bool{flags.KeySubscriptionPlanAwareRouting: true}})
	ctx = context.WithValue(ctx, ManagedSubscriptionPlanStatesContextKey{}, map[subscriptions.Provider]SubscriptionPlanState{
		subscriptions.ProviderClaude: SubscriptionPlanStateExhausted,
	})

	out, _, managed, err := svc.leaseManagedSubscription(
		ctx,
		providers.ProviderAnthropic,
		"claude-opus-4-8",
	)

	require.NoError(t, err)
	require.False(t, managed)
	require.Same(t, ctx, out)
	require.Empty(t, leaser.providers)
}

func TestManagedSubscriptionAllPlansExhaustedPreservesSubscriptionOnly(t *testing.T) {
	leaser := &scriptedSubscriptionLeaser{}
	svc := newServiceWithProviders(t, nil).
		WithManagedSubscriptions(leaser)
	ctx := billing.WithSubscriptionOnly(managedSubscriptionTestContext())
	ctx = flags.WithOverrides(ctx, flags.Overrides{Bools: map[flags.Key]bool{flags.KeySubscriptionPlanAwareRouting: true}})
	ctx = context.WithValue(ctx, ManagedSubscriptionPlanStatesContextKey{}, map[subscriptions.Provider]SubscriptionPlanState{
		subscriptions.ProviderClaude: SubscriptionPlanStateExhausted,
	})

	out, _, managed, err := svc.leaseManagedSubscription(
		ctx,
		providers.ProviderAnthropic,
		"claude-opus-4-8",
	)

	require.ErrorIs(t, err, ErrSubscriptionPoolExhausted)
	require.True(t, managed)
	require.Same(t, ctx, out)
	require.Empty(t, leaser.providers)
}

func TestManagedSubscriptionAllPlansExhaustedRequiresOrgOptInForPaidFallback(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		leaser := &scriptedSubscriptionLeaser{}
		svc := newServiceWithProviders(t, nil).WithManagedSubscriptions(leaser)
		ctx := flags.WithOverrides(managedSubscriptionTestContext(), flags.Overrides{
			Bools: map[flags.Key]bool{flags.KeySubscriptionPlanAwareRouting: enabled},
		})
		ctx = context.WithValue(ctx, ManagedSubscriptionPlanStatesContextKey{}, map[subscriptions.Provider]SubscriptionPlanState{
			subscriptions.ProviderClaude: SubscriptionPlanStateExhausted,
		})
		_, _, managed, err := svc.leaseManagedSubscription(ctx, providers.ProviderAnthropic, "claude-opus-4-8")
		if enabled {
			require.NoError(t, err)
			require.False(t, managed)
		} else {
			require.ErrorIs(t, err, ErrSubscriptionPoolExhausted)
			require.True(t, managed)
		}
	}
}

func TestInferenceFailsClosedWhenSubscriptionEnrollmentIsUnknown(t *testing.T) {
	svc := &Service{}
	ctx := context.WithValue(context.Background(), ManagedSubscriptionEnrollmentUnavailableContextKey{}, true)
	body := []byte(`{"model":"claude-opus-4-8","messages":[]}`)

	require.ErrorIs(t, svc.ProxyMessages(ctx, body, httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", nil)), ErrSubscriptionPoolUnavailable)
	require.ErrorIs(t, svc.ProxyOpenAIChatCompletion(ctx, body, httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)), ErrSubscriptionPoolUnavailable)
	require.ErrorIs(t, svc.ProxyGeminiGenerateContent(ctx, body, httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1beta/models/test:generateContent", nil)), ErrSubscriptionPoolUnavailable)
}

func TestDispatchWithFallbackRotatesManagedAccountBeforeCommit(t *testing.T) {
	leaser := &scriptedSubscriptionLeaser{leases: []subscriptions.Lease{
		{AccountID: "opaque-a", AccessToken: "token-a"},
		{AccountID: "opaque-b", AccessToken: "token-b"},
	}}
	client := &fakeClient{name: providers.ProviderAnthropic, outcomes: []fakeOutcome{
		{err: &providers.UpstreamErrorResponse{Status: http.StatusTooManyRequests, Headers: http.Header{"Retry-After": []string{"30"}}}},
		{writeBytes: []byte("served")},
	}}
	svc := newServiceWithProviders(t, map[string]providers.Client{providers.ProviderAnthropic: client}).WithManagedSubscriptions(leaser)
	recorder := httptest.NewRecorder()
	buffer := newPreludeBuffer(recorder)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	var tokens []string

	ctx := managedSubscriptionTestContext()
	_, err := svc.dispatchWithFallback(ctx, failoverInputs{
		w: recorder, buf: buffer,
		initialDecision: router.Decision{Model: "claude-opus-4-8", Provider: providers.ProviderAnthropic},
		bindings:        []catalog.ProviderBinding{{Provider: providers.ProviderAnthropic}},
		attempt: func(ctx context.Context, decision router.Decision, client providers.Client) error {
			tokens = append(tokens, string(CredentialsFromContext(ctx).APIKey))
			buffer.Seal()
			return client.Proxy(ctx, decision, providers.PreparedRequest{}, buffer, request)
		},
	})

	require.NoError(t, err)
	require.Equal(t, []string{"token-a", "token-b"}, tokens)
	require.Equal(t, []string{"opaque-a"}, leaser.cooldownIDs)
	require.Equal(t, "served", recorder.Body.String())
	require.True(t, servedOnSubscription(ctx))
}

func TestDispatchWithFallbackDoesNotMarkFailedManagedAttemptServed(t *testing.T) {
	leaser := &scriptedSubscriptionLeaser{leases: []subscriptions.Lease{{AccountID: "opaque-a", AccessToken: "token-a"}}}
	client := &fakeClient{name: providers.ProviderAnthropic, outcomes: []fakeOutcome{{err: errors.New("upstream failed")}}}
	svc := newServiceWithProviders(t, map[string]providers.Client{providers.ProviderAnthropic: client}).WithManagedSubscriptions(leaser)
	ctx := managedSubscriptionTestContext()

	_, err := svc.dispatchWithFallback(ctx, failoverInputs{
		w:               httptest.NewRecorder(),
		initialDecision: router.Decision{Model: "claude-opus-4-8", Provider: providers.ProviderAnthropic},
		bindings:        []catalog.ProviderBinding{{Provider: providers.ProviderAnthropic}},
		attempt: func(ctx context.Context, decision router.Decision, client providers.Client) error {
			return client.Proxy(ctx, decision, providers.PreparedRequest{}, httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
		},
	})

	require.Error(t, err)
	require.False(t, servedOnSubscription(ctx))
}

func TestDispatchWithFallbackBoundsManagedAccountRotationByTime(t *testing.T) {
	leaser := &scriptedSubscriptionLeaser{leases: []subscriptions.Lease{
		{AccountID: "opaque-a", AccessToken: "token-a"},
		{AccountID: "opaque-b", AccessToken: "token-b"},
	}}
	client := &fakeClient{name: providers.ProviderAnthropic, outcomes: []fakeOutcome{
		{err: &providers.UpstreamErrorResponse{Status: http.StatusTooManyRequests}},
		{writeBytes: []byte("must not run")},
	}}
	svc := newServiceWithProviders(t, map[string]providers.Client{providers.ProviderAnthropic: client}).WithManagedSubscriptions(leaser)
	startedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clockCalls := 0
	svc.now = func() time.Time {
		clockCalls++
		if clockCalls == 1 {
			return startedAt
		}
		return startedAt.Add(11 * time.Second)
	}

	_, err := svc.dispatchWithFallback(managedSubscriptionTestContext(), failoverInputs{
		w:               httptest.NewRecorder(),
		initialDecision: router.Decision{Model: "claude-opus-4-8", Provider: providers.ProviderAnthropic},
		bindings:        []catalog.ProviderBinding{{Provider: providers.ProviderAnthropic}},
		attempt: func(ctx context.Context, decision router.Decision, client providers.Client) error {
			return client.Proxy(ctx, decision, providers.PreparedRequest{}, httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
		},
	})

	require.Error(t, err)
	require.Equal(t, 1, client.calls)
	require.Equal(t, 1, leaser.next)
}

func TestDispatchWithFallbackDisablesRejectedManagedAccountBeforeRotation(t *testing.T) {
	leaser := &scriptedSubscriptionLeaser{leases: []subscriptions.Lease{
		{AccountID: "opaque-a", AccessToken: "token-a"},
		{AccountID: "opaque-b", AccessToken: "token-b"},
	}}
	client := &fakeClient{name: providers.ProviderAnthropic, outcomes: []fakeOutcome{
		{err: &providers.UpstreamErrorResponse{Status: http.StatusUnauthorized}},
		{writeBytes: []byte("served")},
	}}
	svc := newServiceWithProviders(t, map[string]providers.Client{providers.ProviderAnthropic: client}).WithManagedSubscriptions(leaser)
	recorder := httptest.NewRecorder()
	buffer := newPreludeBuffer(recorder)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	_, err := svc.dispatchWithFallback(managedSubscriptionTestContext(), failoverInputs{
		w: recorder, buf: buffer,
		initialDecision: router.Decision{Model: "claude-opus-4-8", Provider: providers.ProviderAnthropic},
		bindings:        []catalog.ProviderBinding{{Provider: providers.ProviderAnthropic}},
		attempt: func(ctx context.Context, decision router.Decision, client providers.Client) error {
			buffer.Seal()
			return client.Proxy(ctx, decision, providers.PreparedRequest{}, buffer, request)
		},
	})

	require.NoError(t, err)
	require.Equal(t, []string{"opaque-a"}, leaser.disabledIDs)
	require.Equal(t, "served", recorder.Body.String())
}

func TestDispatchWithFallbackDoesNotReplayCommittedManagedStream(t *testing.T) {
	leaser := &scriptedSubscriptionLeaser{leases: []subscriptions.Lease{
		{AccountID: "opaque-a", AccessToken: "token-a"},
		{AccountID: "opaque-b", AccessToken: "token-b"},
	}}
	client := &fakeClient{name: providers.ProviderAnthropic, outcomes: []fakeOutcome{{
		writeBytes: []byte("partial"), err: &providers.UpstreamErrorResponse{Status: http.StatusTooManyRequests},
	}}}
	svc := newServiceWithProviders(t, map[string]providers.Client{providers.ProviderAnthropic: client}).WithManagedSubscriptions(leaser)
	recorder := httptest.NewRecorder()
	buffer := newPreludeBuffer(recorder)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	_, err := svc.dispatchWithFallback(managedSubscriptionTestContext(), failoverInputs{
		w: recorder, buf: buffer,
		initialDecision: router.Decision{Model: "claude-opus-4-8", Provider: providers.ProviderAnthropic},
		bindings:        []catalog.ProviderBinding{{Provider: providers.ProviderAnthropic}},
		attempt: func(ctx context.Context, decision router.Decision, client providers.Client) error {
			buffer.Seal()
			return client.Proxy(ctx, decision, providers.PreparedRequest{}, buffer, request)
		},
	})

	require.Error(t, err)
	require.Equal(t, 1, leaser.next)
	require.Equal(t, "partial", recorder.Body.String())
}

func TestDispatchWithFallbackNeverUsesPaidBindingAfterManagedExhaustion(t *testing.T) {
	leaser := &scriptedSubscriptionLeaser{leases: []subscriptions.Lease{{AccountID: "opaque-a", AccessToken: "token-a"}}}
	primary := &fakeClient{name: providers.ProviderAnthropic, outcomes: []fakeOutcome{{err: &providers.UpstreamErrorResponse{Status: http.StatusTooManyRequests}}}}
	paidFallback := &fakeClient{name: "paid"}
	svc := newServiceWithProviders(t, map[string]providers.Client{
		providers.ProviderAnthropic: primary,
		"paid":                      paidFallback,
	}).WithManagedSubscriptions(leaser)
	recorder := httptest.NewRecorder()
	buffer := newPreludeBuffer(recorder)

	_, err := svc.dispatchWithFallback(managedSubscriptionTestContext(), failoverInputs{
		w: recorder, buf: buffer,
		initialDecision: router.Decision{Model: "claude-opus-4-8", Provider: providers.ProviderAnthropic},
		bindings: []catalog.ProviderBinding{
			{Provider: providers.ProviderAnthropic},
			{Provider: "paid"},
		},
		attempt: func(ctx context.Context, decision router.Decision, client providers.Client) error {
			buffer.Seal()
			return client.Proxy(ctx, decision, providers.PreparedRequest{}, buffer, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
		},
	})

	require.True(t, errors.Is(err, ErrSubscriptionPoolExhausted))
	require.Equal(t, 0, paidFallback.calls)
}
