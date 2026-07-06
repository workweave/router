package hmm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/router"
)

func TestHTTPRouterEndToEndWithSidecar(t *testing.T) {
	var gotRoute routeRequest
	var gotOutcome map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/route":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotRoute))
			require.NotEmpty(t, gotRoute.RouteID)
			require.Contains(t, gotRoute.CandidateModels, "moonshotai/kimi-k2.7-code")
			require.Equal(t, "fireworks", gotRoute.CandidateProviders["moonshotai/kimi-k2.7-code"])
			_ = json.NewEncoder(w).Encode(routeResponse{
				RouteID:       gotRoute.RouteID,
				Model:         "moonshotai/kimi-k2.7-code",
				Score:         0.72,
				ScoreKind:     "policy_confidence",
				Reason:        "policy delegated_work",
				PolicyGroup:   "delegated",
				PolicyLabel:   "delegated_work",
				Propensity:    0.5,
				DisplayMarker: "✦ **Weave Router** → Delegating work with moonshotai/kimi-k2.7-code\n↳ label: delegated_work",
			})
		case "/outcome":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotOutcome))
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	r := New(
		NewHTTPDecider(server.URL, server.Client(), 0),
		map[string]struct{}{"moonshotai/kimi-k2.7": {}},
		map[string]struct{}{"fireworks": {}},
	)
	decision, err := r.Route(context.Background(), router.Request{
		PromptText:           "please inspect the router source",
		EstimatedInputTokens: 123,
		HasTools:             true,
	})

	require.NoError(t, err)
	require.NotNil(t, decision.Metadata)
	assert.Equal(t, "moonshotai/kimi-k2.7", decision.Model)
	assert.Equal(t, "fireworks", decision.Provider)
	assert.Equal(t, "hmm", decision.Metadata.Strategy)
	assert.Equal(t, float32(0.5), decision.Metadata.Propensity)
	assert.Contains(t, decision.Metadata.DisplayMarker, "Delegating work")
	assert.Equal(t, "please inspect the router source", gotRoute.PromptText)
	assert.True(t, gotRoute.HasTools)
	assert.Equal(t, 123, gotRoute.EstimatedInputTokens)

	err = r.ReportOutcome(context.Background(), map[string]interface{}{
		"route_id":     decision.Metadata.RouteID,
		"served_model": decision.Model,
	})

	require.NoError(t, err)
	assert.Equal(t, decision.Metadata.RouteID, gotOutcome["route_id"])
	assert.Equal(t, "moonshotai/kimi-k2.7", gotOutcome["served_model"])
}
