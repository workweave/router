package hmm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/router"
)

func TestHTTPDeciderPostsRouteAndParsesDisplayMetadata(t *testing.T) {
	var got routeRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/route", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		_ = json.NewEncoder(w).Encode(routeResponse{
			RouteID:       "route-1",
			Model:         "moonshotai/kimi-k2.7-code",
			Score:         0.91,
			ScoreKind:     "policy_confidence",
			Reason:        "policy",
			PolicyGroup:   "standard",
			PolicyLabel:   "short_turn",
			Propensity:    1.0,
			DisplayMarker: "display marker",
		})
	}))
	defer server.Close()

	decider := NewHTTPDecider(server.URL, server.Client(), 0)
	result, err := decider.Decide(context.Background(), Query{
		RouteID:    "route-1",
		PromptText: "hello",
		ConversationMessages: []router.ConversationMessage{
			{Role: "user", Text: "please explore the repo"},
			{Role: "assistant", Text: "done"},
			{Role: "tool", Text: "large raw tool result should not be sent"},
			{Role: "user", ToolResults: []router.ConversationToolResult{{
				ToolUseID: "toolu_123",
				IsError:   true,
			}}},
			{
				Role: "user",
				Text: "latest hello",
				ToolCalls: []router.ConversationToolCall{{
					Name:      "Read",
					InputKeys: []string{"file_path"},
				}},
			},
		},
		EstimatedInputTokens: 123,
		HasTools:             true,
		Candidates: []Candidate{{
			RosterID: "moonshotai/kimi-k2.7-code",
			Provider: "openrouter",
		}},
	})

	require.NoError(t, err)
	assert.Equal(t, "hello", got.PromptText)
	assert.Equal(t, "latest hello", got.LatestUserText)
	assert.Equal(t, 1, got.TurnIndex)
	require.Len(t, got.ConversationMessages, 4)
	assert.Equal(t, "user", got.ConversationMessages[0].Role)
	assert.Equal(t, "please explore the repo", got.ConversationMessages[0].Text)
	assert.Equal(t, "assistant", got.ConversationMessages[1].Role)
	assert.Equal(t, "user", got.ConversationMessages[2].Role)
	assert.Empty(t, got.ConversationMessages[2].Text)
	require.Len(t, got.ConversationMessages[2].ToolResults, 1)
	assert.Equal(t, "toolu_123", got.ConversationMessages[2].ToolResults[0].ToolUseID)
	assert.True(t, got.ConversationMessages[2].ToolResults[0].IsError)
	assert.Equal(t, "latest hello", got.ConversationMessages[3].Text)
	require.Len(t, got.ConversationMessages[3].ToolCalls, 1)
	assert.Equal(t, "Read", got.ConversationMessages[3].ToolCalls[0].Name)
	assert.Equal(t, []string{"file_path"}, got.ConversationMessages[3].ToolCalls[0].InputKeys)
	assert.True(t, got.HasTools)
	assert.Equal(t, []string{"moonshotai/kimi-k2.7-code"}, got.CandidateModels)
	assert.Equal(t, "moonshotai/kimi-k2.7-code", result.Model)
	assert.Equal(t, "display marker", result.DisplayMarker)
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
	for _, msg := range messages {
		assert.NotEqual(t, "tool", msg.Role)
	}
}

func TestRouteMessagesTreatsDeveloperTextAsPromptText(t *testing.T) {
	messages := routeMessages([]router.ConversationMessage{
		{Role: "user", Text: "earlier user request"},
		{Role: "assistant", Text: "earlier answer"},
		{Role: "developer", Text: "latest developer prompt"},
		{Role: "assistant", ToolCalls: []router.ConversationToolCall{
			{Name: "", InputKeys: []string{"ignored"}},
			{Name: "Read", InputKeys: []string{"file_path"}},
		}},
	})

	assert.Equal(t, "latest developer prompt", latestUserText(messages))
	assert.Equal(t, 1, turnIndex(messages))
	require.Len(t, messages, 4)
	require.Len(t, messages[3].ToolCalls, 1)
	assert.Equal(t, "Read", messages[3].ToolCalls[0].Name)
}

func TestHTTPDeciderReportsOutcome(t *testing.T) {
	var got map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/outcome", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	}))
	defer server.Close()

	decider := NewHTTPDecider(server.URL, server.Client(), 0)
	err := decider.ReportOutcome(context.Background(), map[string]interface{}{
		"route_id":     "route-1",
		"served_model": "moonshotai/kimi-k2.7-code",
	})

	require.NoError(t, err)
	assert.Equal(t, "route-1", got["route_id"])
}
