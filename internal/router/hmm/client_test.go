package hmm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		RouteID:              "route-1",
		PromptText:           "hello",
		EstimatedInputTokens: 123,
		HasTools:             true,
		Candidates: []Candidate{{
			RosterID: "moonshotai/kimi-k2.7-code",
			Provider: "openrouter",
		}},
	})

	require.NoError(t, err)
	assert.Equal(t, "hello", got.PromptText)
	assert.True(t, got.HasTools)
	assert.Equal(t, []string{"moonshotai/kimi-k2.7-code"}, got.CandidateModels)
	assert.Equal(t, "moonshotai/kimi-k2.7-code", result.Model)
	assert.Equal(t, "display marker", result.DisplayMarker)
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
