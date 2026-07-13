package hmm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"
)

type fakeDecider struct {
	query Query
	res   Result
	err   error
	calls int
}

func (f *fakeDecider) Decide(_ context.Context, q Query) (Result, error) {
	f.calls++
	f.query = q
	return f.res, f.err
}

func TestRouterMapsSidecarRosterModelBackToCatalogDecision(t *testing.T) {
	decider := &fakeDecider{res: Result{
		RouteID:              "route-1",
		Model:                "moonshotai/kimi-k2.7-code",
		Provider:             providers.ProviderFireworks,
		Score:                0.8,
		CandidateScores:      map[string]float32{"moonshotai/kimi-k2.7-code": 0.8},
		Reason:               "policy",
		Propensity:           0.9,
		DisplayMarker:        "display marker",
		PolicyLabel:          "short_turn",
		PolicyGroup:          "standard",
		PolicyRouteKey:       "standard|open",
		PolicyArtifactID:     "hmm-prod",
		PolicyArtifactSHA256: "sha256:abc",
		RosterVersion:        "roster-v2",
		SchemaVersion:        "policy_router_v1",
		DebugRef:             "debug-1",
	}}
	deployed := map[string]struct{}{"moonshotai/kimi-k2.7": {}}
	available := map[string]struct{}{providers.ProviderFireworks: {}}
	r := New(decider, deployed, available)

	decision, err := r.Route(context.Background(), router.Request{
		OrganizationID: "org-1",
		InstallationID: "installation-1",
		ClientApp:      "codex",
		RolloutID:      "rollout-1",
		PromptText:     "hello",
		ConversationMessages: []router.ConversationMessage{{
			Role: "user",
			Text: "latest hello",
		}},
		EstimatedInputTokens: 10,
		DebugEnabled:         true,
		FeedbackKey:          "feedback-session",
		FeedbackRole:         "default",
		ClientSessionID:      "client-session-abc",
	})

	require.NoError(t, err)
	assert.Equal(t, "moonshotai/kimi-k2.7", decision.Model)
	assert.NotNil(t, decision.Metadata)
	assert.Equal(t, "display marker", decision.Metadata.DisplayMarker)
	assert.Equal(t, "route-1", decision.Metadata.RouteID)
	assert.Equal(t, "hmm", decision.Metadata.Strategy)
	assert.Equal(t, float32(0.9), decision.Metadata.Propensity)
	assert.Equal(t, "standard|open", decision.Metadata.PolicyRouteKey)
	assert.Equal(t, "hmm-prod", decision.Metadata.PolicyArtifactID)
	assert.Equal(t, "sha256:abc", decision.Metadata.PolicyArtifactSHA256)
	assert.Equal(t, "roster-v2", decision.Metadata.RosterVersion)
	assert.Equal(t, "policy_router_v1", decision.Metadata.SidecarSchemaVersion)
	assert.Equal(t, "debug-1", decision.Metadata.DebugRef)
	assert.Equal(t, map[string]float32{"moonshotai/kimi-k2.7": 0.8}, decision.Metadata.CandidateScores)
	assert.Equal(t, "hello", decider.query.PromptText)
	assert.Equal(t, router.StrategyHMM, decider.query.Strategy)
	assert.Equal(t, "org-1", decider.query.OrganizationID)
	assert.Equal(t, "installation-1", decider.query.InstallationID)
	assert.Equal(t, "codex", decider.query.ClientApp)
	assert.Equal(t, "rollout-1", decider.query.RolloutID)
	assert.Equal(t, "feedback-session", decider.query.FeedbackKey)
	assert.Equal(t, "default", decider.query.FeedbackRole)
	assert.Equal(t, "client-session-abc", decider.query.ClientSessionID)
	assert.Equal(t, []router.ConversationMessage{{Role: "user", Text: "latest hello"}}, decider.query.ConversationMessages)
	require.Len(t, decider.query.Candidates, 1)
	candidate := decider.query.Candidates[0]
	assert.Equal(t, "moonshotai/kimi-k2.7-code", candidate.RosterID)
	assert.Equal(t, "moonshotai/kimi-k2.7", candidate.CatalogID)
	assert.Equal(t, providers.ProviderFireworks, candidate.Provider)
	assert.Equal(t, 0.95, candidate.InputUSDPer1M)
	assert.Equal(t, 4.0, candidate.OutputUSDPer1M)
	assert.InDelta(t, 0.0000095, candidate.EstimatedCostUSD, 1e-12)
	assert.Equal(t, 262144, candidate.Capabilities.ContextWindow)
	assert.Equal(t, "high", candidate.Capabilities.Tier)
	assert.True(t, candidate.Capabilities.SupportsTools)
	assert.False(t, candidate.Capabilities.SupportsImages)
}

func TestRouterKeepsGeneratedRouteIDWhenSidecarOmitsIt(t *testing.T) {
	decider := &fakeDecider{res: Result{
		Model: "moonshotai/kimi-k2.7-code",
	}}
	r := New(decider, map[string]struct{}{"moonshotai/kimi-k2.7": {}}, map[string]struct{}{providers.ProviderFireworks: {}})

	decision, err := r.Route(context.Background(), router.Request{PromptText: "hello"})

	require.NoError(t, err)
	require.NotNil(t, decision.Metadata)
	assert.NotEmpty(t, decider.query.RouteID)
	assert.Equal(t, decider.query.RouteID, decision.Metadata.RouteID)
}

func TestRouterFailsClosedOnUnknownReturnedModel(t *testing.T) {
	decider := &fakeDecider{res: Result{Model: "unknown/model"}}
	r := New(decider, map[string]struct{}{"moonshotai/kimi-k2.7": {}}, map[string]struct{}{providers.ProviderFireworks: {}})

	_, err := r.Route(context.Background(), router.Request{PromptText: "hello"})

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrHMMUnavailable)
}

func TestRouterFailsClosedOnReturnedProviderMismatch(t *testing.T) {
	decider := &fakeDecider{res: Result{Model: "moonshotai/kimi-k2.7-code", Provider: providers.ProviderOpenRouter}}
	r := New(decider, map[string]struct{}{"moonshotai/kimi-k2.7": {}}, map[string]struct{}{providers.ProviderFireworks: {}})

	_, err := r.Route(context.Background(), router.Request{PromptText: "hello"})

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrHMMUnavailable)
}

func TestRouterDoesNotOfferOpenRouterFallbackCandidates(t *testing.T) {
	decider := &fakeDecider{res: Result{Model: "minimax/minimax-m3"}}
	r := New(
		decider,
		map[string]struct{}{"minimax/minimax-m3": {}},
		map[string]struct{}{providers.ProviderOpenRouter: {}},
	)

	candidates := r.resolver.Resolve(router.Request{}).Candidates

	assert.Empty(t, candidates)

	_, err := r.Route(context.Background(), router.Request{PromptText: "hello"})

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrHMMUnavailable)
	assert.Zero(t, decider.calls)
}

func TestCurrentHMMRosterCatalogArmsResolveToCurrentProviders(t *testing.T) {
	deployed := map[string]struct{}{
		"deepseek/deepseek-v4-flash": {},
		"qwen/qwen3-coder-next":      {},
		"gpt-5.4-nano":               {},
		"minimax/minimax-m3":         {},
		"moonshotai/kimi-k2.7":       {},
		"deepseek/deepseek-v4-pro":   {},
		"gemini-3.5-flash":           {},
		"claude-sonnet-5":            {},
		"claude-opus-4-8":            {},
		"gpt-5.5":                    {},
		"z-ai/glm-5.2":               {},
		"gemini-3.1-pro-preview":     {},
		"claude-fable-5":             {},
	}
	available := map[string]struct{}{
		providers.ProviderAnthropic:  {},
		providers.ProviderOpenAI:     {},
		providers.ProviderGoogle:     {},
		providers.ProviderOpenRouter: {},
		providers.ProviderFireworks:  {},
		providers.ProviderBedrock:    {},
		providers.ProviderMakora:     {},
		providers.ProviderTogether:   {},
	}
	r := New(&fakeDecider{}, deployed, available)

	candidates := r.resolver.Resolve(router.Request{}).Candidates

	gotRosterIDs := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		assert.NotEqual(t, providers.ProviderOpenRouter, candidate.Provider, candidate.RosterID)
		gotRosterIDs = append(gotRosterIDs, candidate.RosterID)
	}
	assert.ElementsMatch(t, []string{
		"deepseek/deepseek-v4-flash",
		"qwen/qwen3-coder-next",
		"openai/gpt-5.4-nano",
		"minimax/minimax-m3",
		"moonshotai/kimi-k2.7-code",
		"deepseek/deepseek-v4-pro",
		"google/gemini-3.5-flash",
		"anthropic/claude-sonnet-5",
		"anthropic/claude-opus-4.8",
		"openai/gpt-5.5",
		"z-ai/glm-5.2",
		"google/gemini-3.1-pro-preview",
		"anthropic/claude-fable-5",
	}, gotRosterIDs)
}

func TestRosterIDForSkipsAmbiguousBareProviderIDs(t *testing.T) {
	got := rosterIDFor(catalog.Model{
		ID: "bare-provider-model",
		Providers: []catalog.ProviderBinding{{
			Provider: providers.ProviderFireworks,
		}},
	})

	assert.Empty(t, got)
}
