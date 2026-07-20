package policyclient

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/policy"
)

func TestLiveGenericPolicySidecarContract(t *testing.T) {
	sidecarURL := os.Getenv("ROUTER_POLICY_LIVE_TEST_URL")
	if sidecarURL == "" {
		t.Skip("ROUTER_POLICY_LIVE_TEST_URL is unset")
	}

	client := New(sidecarURL, nil, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	capabilities, err := client.Capabilities(ctx)
	require.NoError(t, err)
	assert.Equal(t, policy.SchemaVersionV1, capabilities.SchemaVersion)
	assert.True(t, capabilities.AuthoritativePerTurnSelection)
	require.NoError(t, client.CheckHealth(ctx))

	result, err := client.Decide(ctx, policy.Query{
		Strategy:        router.Strategy("universal-turn"),
		ExecutionMode:   policy.ExecutionModeServing,
		RouteID:         "go-live-contract-route",
		ClientApp:       "claude-code",
		TrainingAllowed: false,
		DebugEnabled:    true,
		ConversationMessages: []router.ConversationMessage{
			{Role: "system", Text: "Work in repository."},
			{Role: "user", Text: "Fix the failing test."},
			{Role: "assistant", ToolResults: []router.ConversationToolResult{{
				ToolUseID:     "toolu_live",
				Text:          "raw tool result must remain masked",
				ResultPresent: true,
				CharCount:     34,
				ByteCount:     34,
				ExitCategory:  "success",
			}}},
		},
		TurnContext: &router.PolicyTurnContext{
			VisibleTurnIndex:    1,
			SessionTurnCount:    1,
			TurnType:            "tool_result",
			CacheState:          router.PolicyCacheStateUnknown,
			SessionEverSwitched: false,
			HistoryTruncated:    false,
		},
		AvailableTools:       []string{"ReadFile"},
		EstimatedInputTokens: 1200,
		HasTools:             true,
		Candidates: []policy.Candidate{
			{
				RosterID:                  "claude-fable-5",
				CatalogID:                 "claude-fable-5",
				Provider:                  providers.ProviderAnthropic,
				UpstreamID:                "claude-fable-5",
				InputUSDPer1M:             3,
				OutputUSDPer1M:            15,
				EstimatedCostUSD:          0.012,
				CacheReadMultiplier:       0.1,
				MarginalCostFactor:        1,
				EffectiveInputUSDPer1M:    3,
				EffectiveOutputUSDPer1M:   15,
				EffectiveEstimatedCostUSD: 0.012,
			},
			{
				RosterID:                  "gpt-5.5",
				CatalogID:                 "gpt-5.5",
				Provider:                  providers.ProviderOpenAI,
				UpstreamID:                "gpt-5.5",
				InputUSDPer1M:             2.5,
				OutputUSDPer1M:            10,
				EstimatedCostUSD:          0.009,
				CacheReadMultiplier:       0.1,
				MarginalCostFactor:        1,
				EffectiveInputUSDPer1M:    2.5,
				EffectiveOutputUSDPer1M:   10,
				EffectiveEstimatedCostUSD: 0.009,
			},
		},
	})

	require.NoError(t, err)
	assert.Equal(t, policy.SchemaVersionV1, result.SchemaVersion)
	assert.Equal(t, "go-live-contract-route", result.RouteID)
	assert.Contains(t, []string{"claude-fable-5", "gpt-5.5"}, result.Model)
	assert.Len(t, result.CandidateScores, 2)
	assert.Equal(t, 1.0, result.Propensity)
	assert.NotEmpty(t, result.PolicyArtifactID)
	assert.NotEmpty(t, result.PolicyArtifactSHA256)
	assert.NotEmpty(t, result.RosterVersion)
}
