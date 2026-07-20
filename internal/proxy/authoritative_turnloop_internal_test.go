package proxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/handover"
	"workweave/router/internal/router/planner"
	"workweave/router/internal/router/policy"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"
)

type authoritativeTestRouter struct {
	decision router.Decision
	requests []router.Request
}

func (r *authoritativeTestRouter) Route(_ context.Context, req router.Request) (router.Decision, error) {
	r.requests = append(r.requests, req)
	return r.decision, nil
}

type authoritativeHandoverSummarizer struct {
	calls int
}

func (s *authoritativeHandoverSummarizer) Summarize(
	_ context.Context,
	_ *translate.RequestEnvelope,
) (string, handover.Usage, error) {
	s.calls++
	return "must not run", handover.Usage{}, nil
}

func (s *authoritativeHandoverSummarizer) Provider() string {
	return providers.ProviderAnthropic
}

func TestAuthoritativePolicySelectsEveryEligibleTurn(t *testing.T) {
	strategy := router.Strategy("authoritative-test")
	tests := []struct {
		name             string
		body             []byte
		pinFound         bool
		pinExpires       time.Time
		historyTruncated bool
	}{
		{
			name:       "first turn routes once",
			body:       []byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"start"}]}`),
			pinExpires: time.Now().Add(time.Hour),
		},
		{
			name:       "main loop ignores planner stay",
			body:       []byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"fix the failing test"}]}`),
			pinFound:   true,
			pinExpires: time.Now().Add(time.Hour),
		},
		{
			name: "tool result ignores sticky kill switch",
			body: []byte(`{
				"model":"claude-opus-4-8",
				"tools":[{"name":"Read","description":"read","input_schema":{"type":"object"}}],
				"messages":[
					{"role":"user","content":"inspect the repository"},
					{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"path":"README.md"}}]},
					{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"contents"}]}
				]
			}`),
			pinFound:   true,
			pinExpires: time.Now().Add(time.Hour),
		},
		{
			name:       "expired pin does not reanchor",
			body:       []byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"continue"}]}`),
			pinFound:   true,
			pinExpires: time.Now().Add(-time.Minute),
		},
		{
			name:             "deterministic compaction is visible",
			body:             []byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"continue after trim"}]}`),
			pinFound:         true,
			pinExpires:       time.Now().Add(time.Hour),
			historyTruncated: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newStubPinStore()
			store.getFound = test.pinFound
			store.getPin = sessionpin.Pin{
				Provider:         providers.ProviderAnthropic,
				Model:            "claude-opus-4-7",
				Reason:           "cluster:v0.2",
				TurnCount:        7,
				PinnedUntil:      test.pinExpires,
				LastTurnEndedAt:  time.Now().Add(-time.Minute),
				LastServedModel:  "claude-opus-4-7",
				LastOutputTokens: 321,
				HasEverSwitched:  true,
			}
			policyRouter := &authoritativeTestRouter{decision: router.Decision{
				Provider: providers.ProviderAnthropic,
				Model:    "claude-opus-4-8",
				Reason:   "authoritative-test_policy",
			}}
			summarizer := &authoritativeHandoverSummarizer{}
			svc := NewService(
				nil,
				nil,
				nil,
				false,
				nil,
				store,
				false,
				providers.ProviderAnthropic,
				"claude-haiku-4-5",
				nil,
			).WithScoreToolResultTurns(false).
				WithPlanner(planner.EVConfig{
					ThresholdUSD:           1_000_000,
					ExpectedRemainingTurns: 3,
				}).
				WithSummarizer(summarizer).
				WithPolicyStrategy(policy.StrategySpec{
					Strategy: strategy,
					Router:   policyRouter,
					Capabilities: policy.Capabilities{
						SchemaVersion:                 policy.SchemaVersionV1,
						AuthoritativePerTurnSelection: true,
					},
				})
			env, err := translate.ParseAnthropic(test.body)
			require.NoError(t, err)
			features := env.RoutingFeatures(false)
			ctx := router.WithStrategy(context.Background(), strategy)
			req := router.Request{
				RequestedModel:       features.Model,
				EstimatedInputTokens: features.Tokens,
				HasTools:             features.HasTools,
				ConversationMessages: conversationMessagesForRouting(env),
				HistoryTruncated:     test.historyTruncated,
			}

			result, err := svc.runTurnLoop(
				ctx,
				env,
				features,
				"api-key",
				uuid.New(),
				"",
				http.Header{},
				req,
			)

			require.NoError(t, err)
			assert.True(t, result.AuthoritativePerTurn)
			assert.Equal(t, "claude-opus-4-8", result.Decision.Model)
			assert.Equal(t, "authoritative_per_turn", result.PinTier)
			assert.False(t, result.StickyHit)
			assert.Empty(t, result.PlannerDecision.Outcome)
			assert.False(t, result.Handover.Invoked)
			assert.Equal(t, 0, summarizer.calls)
			require.Len(t, policyRouter.requests, 1)
			turnContext := policyRouter.requests[0].PolicyTurnContext
			require.NotNil(t, turnContext)
			assert.Equal(t, test.historyTruncated, turnContext.HistoryTruncated)
			if test.pinFound {
				assert.Equal(t, 7, turnContext.SessionTurnCount)
				assert.Equal(t, "claude-opus-4-7", turnContext.PreviousServedModel)
				assert.Equal(t, providers.ProviderAnthropic, turnContext.PreviousProvider)
				expectedCacheState := router.PolicyCacheStateWarm
				if test.historyTruncated {
					expectedCacheState = router.PolicyCacheStateCold
				}
				assert.Equal(t, expectedCacheState, turnContext.CacheState)
				require.NotNil(t, turnContext.PriorOutputTokens)
				assert.Equal(t, 321, *turnContext.PriorOutputTokens)
				assert.True(t, turnContext.SessionEverSwitched)
			} else {
				assert.Zero(t, turnContext.SessionTurnCount)
				assert.Empty(t, turnContext.PreviousServedModel)
				assert.Empty(t, turnContext.PreviousProvider)
				assert.Equal(t, router.PolicyCacheStateUnknown, turnContext.CacheState)
				assert.Nil(t, turnContext.PriorOutputTokens)
				assert.False(t, turnContext.SessionEverSwitched)
			}
			require.Len(t, store.upserts, 1)
			assert.Equal(t, "claude-opus-4-8", store.upserts[0].Model)
		})
	}
}

func TestAuthoritativePolicyDisablesSemanticCache(t *testing.T) {
	strategy := router.Strategy("authoritative-test")
	svc := (&Service{}).WithPolicyStrategy(policy.StrategySpec{
		Strategy: strategy,
		Router:   &authoritativeTestRouter{},
		Capabilities: policy.Capabilities{
			AuthoritativePerTurnSelection: true,
		},
	})

	assert.False(t, svc.semanticCacheAllowed(router.WithStrategy(context.Background(), strategy)))
	assert.True(t, svc.semanticCacheAllowed(context.Background()))
}

func TestAuthoritativePolicyPreservesExplicitForceModel(t *testing.T) {
	strategy := router.Strategy("authoritative-force-test")
	store := newStubPinStore()
	store.getFound = true
	store.getPin = sessionpin.Pin{
		Provider:    providers.ProviderAnthropic,
		Model:       "claude-opus-4-7",
		Reason:      translate.ReasonUserForceModel,
		PinnedUntil: time.Now().Add(time.Hour),
	}
	policyRouter := &authoritativeTestRouter{decision: router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-opus-4-8",
	}}
	svc := NewService(
		nil,
		nil,
		nil,
		false,
		nil,
		store,
		false,
		providers.ProviderAnthropic,
		"claude-haiku-4-5",
		nil,
	).WithPolicyStrategy(policy.StrategySpec{
		Strategy: strategy,
		Router:   policyRouter,
		Capabilities: policy.Capabilities{
			AuthoritativePerTurnSelection: true,
		},
	})
	env, err := translate.ParseAnthropic(
		[]byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"continue"}]}`),
	)
	require.NoError(t, err)
	features := env.RoutingFeatures(false)

	result, err := svc.runTurnLoop(
		router.WithStrategy(context.Background(), strategy),
		env,
		features,
		"api-key",
		uuid.New(),
		"",
		http.Header{},
		router.Request{
			RequestedModel:       features.Model,
			ConversationMessages: conversationMessagesForRouting(env),
		},
	)

	require.NoError(t, err)
	assert.Equal(t, "claude-opus-4-7", result.Decision.Model)
	assert.True(t, result.StickyHit)
	assert.Empty(t, policyRouter.requests)
}
