package policyclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/policy"
)

func TestClientPostsVersionedRouteAndParsesPolicyMetadata(t *testing.T) {
	var got routeRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		require.Equal(t, "/route", request.URL.Path)
		require.NoError(t, json.NewDecoder(request.Body).Decode(&got))
		_ = json.NewEncoder(w).Encode(routeResponse{
			SchemaVersion:        policy.SchemaVersionV1,
			RouteID:              "route-1",
			SelectedRosterID:     "moonshotai/kimi-k2.7-code",
			SelectedProvider:     providers.ProviderFireworks,
			Score:                0.91,
			ScoreLabel:           "classifier_confidence",
			Cluster:              "medium",
			ComplexityLabel:      "Simple Followup",
			RoutingBucket:        "medium|open",
			ClassifierConfidence: floatPtr(0.91),
			ClassifierMargin:     floatPtr(0.22),
			Propensity:           1,
			PolicyArtifactID:     "hmm-prod",
			PolicyArtifactSHA256: "sha256:abc",
			RosterVersion:        "roster-v2",
			DebugRef:             "debug-1",
		})
	}))
	defer server.Close()

	qualityBias := 0.7
	preferenceRank := 0
	client := New(server.URL, server.Client(), 0)
	result, err := client.Decide(context.Background(), policy.Query{
		Strategy:        router.StrategyHMM,
		RouteID:         "route-1",
		OrganizationID:  "org-1",
		InstallationID:  "installation-1",
		ClientApp:       "codex",
		RequestedModel:  "Weave",
		PromptText:      "hello",
		RoutingIntent:   "high",
		PreferredModels: []string{"moonshotai/kimi-k2.7"},
		RoutingKnobs:    &router.Overrides{QualityBias: &qualityBias},
		DebugEnabled:    true,
		ConversationMessages: []router.ConversationMessage{
			{Role: "user", Text: "please explore the repo"},
			{Role: "assistant", Text: "done"},
			{Role: "tool", Text: "raw tool result is omitted"},
			{Role: "user", Text: "latest hello", ToolCalls: []router.ConversationToolCall{{Name: "Read", InputKeys: []string{"file_path"}}}},
		},
		AvailableTools: []string{"Read", "Grep", "Read", ""},
		Candidates: []policy.Candidate{{
			RosterID:       "moonshotai/kimi-k2.7-code",
			CatalogID:      "moonshotai/kimi-k2.7",
			Provider:       providers.ProviderFireworks,
			PreferenceRank: &preferenceRank,
		}},
	})

	require.NoError(t, err)
	assert.Equal(t, policy.SchemaVersionV1, got.SchemaVersion)
	assert.Equal(t, string(router.StrategyHMM), got.Strategy)
	assert.Equal(t, "org-1", got.OrganizationID)
	assert.Equal(t, "installation-1", got.InstallationID)
	assert.Equal(t, "codex", got.ClientApp)
	assert.Equal(t, "codex", got.Harness)
	assert.Equal(t, "high", got.RoutingIntent)
	assert.Equal(t, []string{"moonshotai/kimi-k2.7"}, got.PreferredModels)
	require.NotNil(t, got.QualityBias)
	assert.Equal(t, 0.7, *got.QualityBias)
	assert.False(t, got.TrainingAllowed)
	assert.True(t, got.DebugEnabled)
	assert.Equal(t, "latest hello", got.LatestUserText)
	assert.Equal(t, 1, got.TurnIndex)
	assert.Equal(t, []string{"Read", "Grep"}, got.AvailableTools)
	require.Len(t, got.Candidates, 1)
	assert.Equal(t, "moonshotai/kimi-k2.7", got.Candidates[0].CatalogID)
	assert.Equal(t, "medium|open", result.PolicyRouteKey)
	assert.Equal(t, providers.ProviderFireworks, result.Provider)
	assert.Equal(t, "hmm-prod", result.PolicyArtifactID)
	assert.Equal(t, "sha256:abc", result.PolicyArtifactSHA256)
	assert.Equal(t, "roster-v2", result.RosterVersion)
	assert.Equal(t, "debug-1", result.DebugRef)
}

func TestClientAcceptsLegacyRouteResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"model": "anthropic/claude-opus-4-8"})
	}))
	defer server.Close()

	result, err := New(server.URL, server.Client(), 0).Decide(context.Background(), policy.Query{
		Candidates: []policy.Candidate{{RosterID: "anthropic/claude-opus-4-8", CatalogID: "claude-opus-4-8", Provider: providers.ProviderAnthropic}},
	})

	require.NoError(t, err)
	assert.Equal(t, "anthropic/claude-opus-4-8", result.Model)
	assert.Empty(t, result.SchemaVersion)
}

func TestClientRejectsUnknownRouteSchema(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"schema_version": "policy_router_v99", "selected_roster_id": "model"})
	}))
	defer server.Close()

	_, err := New(server.URL, server.Client(), 0).Decide(context.Background(), policy.Query{
		Candidates: []policy.Candidate{{RosterID: "model"}},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported policy route schema")
}

func TestClientCapabilities(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		require.Equal(t, "/capabilities", request.URL.Path)
		_ = json.NewEncoder(w).Encode(policy.Capabilities{SchemaVersion: policy.SchemaVersionV1, ReportsFeedback: true})
	}))
	defer server.Close()

	capabilities, err := New(server.URL, server.Client(), 0).Capabilities(context.Background())

	require.NoError(t, err)
	assert.True(t, capabilities.ReportsFeedback)
}

func TestClientReportsOutcomeAndFeedback(t *testing.T) {
	paths := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	}))
	defer server.Close()
	client := New(server.URL, server.Client(), 0)

	require.NoError(t, client.ReportOutcome(context.Background(), map[string]interface{}{"route_id": "route-1"}))
	require.NoError(t, client.ReportFeedback(context.Background(), map[string]interface{}{"route_id": "route-1"}))
	assert.Equal(t, []string{"/outcome", "/feedback"}, paths)
}

func TestRouteMessagesPreservesLatestUserWhenPayloadIsCapped(t *testing.T) {
	messages := routeMessages([]router.ConversationMessage{
		{Role: "user", Text: strings.Repeat("a", maxRouteMessageTotalChars+100)},
		{Role: "assistant", Text: "older response"},
		{Role: "tool", Text: "raw tool output should be skipped"},
		{Role: "user", Text: "latest request"},
	})

	assert.Equal(t, "latest request", latestUserText(messages))
	assert.Equal(t, 1, turnIndex(messages))
	for _, message := range messages {
		assert.NotEqual(t, "tool", message.Role)
	}
}

func floatPtr(value float64) *float64 { return &value }
