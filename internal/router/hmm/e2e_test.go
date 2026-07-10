package hmm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/policyclient"
	"workweave/router/internal/providers"
	"workweave/router/internal/router"
)

func TestHTTPRouterEndToEndWithSidecar(t *testing.T) {
	var gotRoute struct {
		RouteID            string            `json:"route_id"`
		PromptText         string            `json:"prompt_text"`
		OrganizationID     string            `json:"organization_id"`
		InstallationID     string            `json:"installation_id"`
		HasTools           bool              `json:"has_tools"`
		EstimatedTokens    int               `json:"estimated_input_tokens"`
		CandidateModels    []string          `json:"candidate_models"`
		CandidateProviders map[string]string `json:"candidate_providers"`
	}
	var gotOutcome map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/route":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotRoute))
			require.NotEmpty(t, gotRoute.RouteID)
			require.Contains(t, gotRoute.CandidateModels, "moonshotai/kimi-k2.7-code")
			require.Equal(t, "fireworks", gotRoute.CandidateProviders["moonshotai/kimi-k2.7-code"])
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"route_id": gotRoute.RouteID, "model": "moonshotai/kimi-k2.7-code", "score": 0.72,
				"score_kind": "policy_confidence", "reason": "policy delegated_work", "policy_group": "delegated",
				"policy_label": "delegated_work", "propensity": 0.5, "display_marker": "Weave Router: delegated_work",
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
		policyclient.New(server.URL, server.Client(), 0),
		map[string]struct{}{"moonshotai/kimi-k2.7": {}},
		map[string]struct{}{providers.ProviderFireworks: {}},
	)
	decision, err := r.Route(context.Background(), router.Request{
		PromptText:           "please inspect the router source",
		EstimatedInputTokens: 123,
		HasTools:             true,
		OrganizationID:       "org-test",
		InstallationID:       "installation-test",
	})

	require.NoError(t, err)
	require.NotNil(t, decision.Metadata)
	assert.Equal(t, "moonshotai/kimi-k2.7", decision.Model)
	assert.Equal(t, providers.ProviderFireworks, decision.Provider)
	assert.Equal(t, "hmm", decision.Metadata.Strategy)
	assert.Equal(t, float32(0.5), decision.Metadata.Propensity)
	assert.Contains(t, decision.Metadata.DisplayMarker, "delegated_work")
	assert.Equal(t, "please inspect the router source", gotRoute.PromptText)
	assert.True(t, gotRoute.HasTools)
	assert.Equal(t, 123, gotRoute.EstimatedTokens)
	assert.Equal(t, "org-test", gotRoute.OrganizationID)
	assert.Equal(t, "installation-test", gotRoute.InstallationID)

	err = r.ReportOutcome(context.Background(), map[string]interface{}{
		"route_id":     decision.Metadata.RouteID,
		"served_model": decision.Model,
	})

	require.NoError(t, err)
	assert.Equal(t, decision.Metadata.RouteID, gotOutcome["route_id"])
	assert.Equal(t, "moonshotai/kimi-k2.7", gotOutcome["served_model"])
}
