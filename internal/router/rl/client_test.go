package rl_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/router/rl"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTPDeciderPostsContractAndParsesResult(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(raw, &gotBody))
		// Mirror router_policy_server.py's success envelope.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":       "anthropic/claude-opus-4-8",
			"score":       1.875,
			"score_label": "DPO score",
			"reason":      "rl_policy artifact=main_q_dpo.pkl",
			"state_label": "implementing a fix",
		})
	}))
	defer server.Close()

	d := rl.NewHTTPDecider(server.URL, nil, 0)
	res, err := d.Decide(context.Background(), rl.Query{
		PromptText: "fix the bug",
		TurnIndex:  3,
		Candidates: []rl.Candidate{
			{RosterID: "anthropic/claude-opus-4-8", Provider: providers.ProviderAnthropic},
			{RosterID: "deepseek/deepseek-v4-flash", Provider: providers.ProviderMakora},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, "/route", gotPath)
	assert.Equal(t, "fix the bug", gotBody["prompt_text"])
	assert.EqualValues(t, 3, gotBody["turn_index"])
	assert.ElementsMatch(t,
		[]any{"anthropic/claude-opus-4-8", "deepseek/deepseek-v4-flash"},
		gotBody["candidate_models"])
	providersMap, ok := gotBody["candidate_providers"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, providers.ProviderAnthropic, providersMap["anthropic/claude-opus-4-8"])

	assert.Equal(t, "anthropic/claude-opus-4-8", res.Model)
	assert.InDelta(t, 1.875, res.Score, 1e-9)
	assert.Equal(t, "DPO score", res.ScoreLabel)
	assert.Equal(t, "implementing a fix", res.StateLabel)
}

func TestHTTPDeciderAttachesStaticHeaders(t *testing.T) {
	var gotKey, gotSecret string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("Modal-Key")
		gotSecret = r.Header.Get("Modal-Secret")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "anthropic/claude-opus-4-8",
			"score": 1.0,
		})
	}))
	defer server.Close()

	d := rl.NewHTTPDeciderWithHeaders(server.URL, nil, 0, map[string]string{
		"Modal-Key":    "mk_test",
		"Modal-Secret": "ms_test",
	})
	_, err := d.Decide(context.Background(), rl.Query{
		PromptText: "x",
		Candidates: []rl.Candidate{
			{RosterID: "anthropic/claude-opus-4-8", Provider: providers.ProviderAnthropic},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "mk_test", gotKey)
	assert.Equal(t, "ms_test", gotSecret)
}

func TestHTTPDeciderSurfacesSidecarError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "no request candidate model is present in the policy artifact roster",
			"type":  "ValueError",
		})
	}))
	defer server.Close()

	d := rl.NewHTTPDecider(server.URL, nil, 0)
	_, err := d.Decide(context.Background(), rl.Query{PromptText: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "policy artifact roster")
}
