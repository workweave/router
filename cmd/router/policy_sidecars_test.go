package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/policy"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildConfiguredPolicySidecarsOnboardsFutureStrategy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/capabilities":
			_ = json.NewEncoder(w).Encode(policy.Capabilities{
				SchemaVersion:  policy.SchemaVersionV1,
				SupportsShadow: true,
			})
		case "/route":
			var payload struct {
				ExecutionMode string `json:"execution_mode"`
				Candidates    []struct {
					RosterID string `json:"roster_id"`
					Provider string `json:"provider"`
				} `json:"candidates"`
			}
			require.NoError(t, json.NewDecoder(request.Body).Decode(&payload))
			require.NotEmpty(t, payload.Candidates)
			assert.Equal(t, policy.ExecutionModeServing, payload.ExecutionMode)
			assert.NotEqual(t, providers.ProviderOpenRouter, payload.Candidates[0].Provider)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version":     policy.SchemaVersionV1,
				"selected_roster_id": payload.Candidates[0].RosterID,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	registrations, err := buildConfiguredPolicySidecars(
		context.Background(),
		`{"future-policy":"`+server.URL+`"}`,
		time.Second,
		map[string]struct{}{"gpt-5.5": {}},
		map[string]struct{}{providers.ProviderOpenAI: {}},
		server.Client(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	require.NoError(t, err)
	require.Len(t, registrations, 1)
	assert.Equal(t, router.Strategy("future-policy"), registrations[0].Strategy)
	assert.True(t, registrations[0].Capabilities.SupportsShadow)
	decision, err := registrations[0].Router.Route(context.Background(), router.Request{})
	require.NoError(t, err)
	assert.Equal(t, "gpt-5.5", decision.Model)
	assert.Equal(t, providers.ProviderOpenAI, decision.Provider)
}

func TestBuildConfiguredPolicySidecarsRejectsReservedAndInvalidConfiguration(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, raw := range []string{
		`{"hmm":"https://sidecar.internal"}`,
		`{"future":"not-a-url"}`,
		`{"future policy":"https://sidecar.internal"}`,
		`{"Future":"https://one.internal","future":"https://two.internal"}`,
	} {
		_, err := buildConfiguredPolicySidecars(
			context.Background(), raw, time.Second, nil, nil, nil, logger,
		)
		require.Error(t, err, raw)
	}
}
