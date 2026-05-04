package heuristic_test

import (
	"context"
	"testing"

	"workweave/router/internal/router"
	"workweave/router/internal/router/heuristic"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRules_Route(t *testing.T) {
	rules := heuristic.NewRules(heuristic.Config{
		SmallModel:      "haiku",
		LargeModel:      "opus",
		ThresholdTokens: 1000,
	})

	tests := []struct {
		name       string
		tokens     int
		wantModel  string
		wantReason string
	}{
		{"below threshold routes to small", 500, "haiku", "heuristic:short_prompt"},
		{"at threshold routes to large", 1000, "opus", "heuristic:long_prompt"},
		{"above threshold routes to large", 5000, "opus", "heuristic:long_prompt"},
		{"empty input routes to small", 0, "haiku", "heuristic:short_prompt"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := rules.Route(context.Background(), router.Request{EstimatedInputTokens: tc.tokens})
			require.NoError(t, err)
			assert.Equal(t, "anthropic", d.Provider)
			assert.Equal(t, tc.wantModel, d.Model)
			assert.Equal(t, tc.wantReason, d.Reason)
		})
	}
}

func TestRules_Route_ProviderOverride(t *testing.T) {
	rules := heuristic.NewRules(heuristic.Config{
		Provider:        "openai",
		SmallModel:      "gpt-5-nano",
		LargeModel:      "gpt-5",
		ThresholdTokens: 100,
	})

	short, err := rules.Route(context.Background(), router.Request{EstimatedInputTokens: 50})
	require.NoError(t, err)
	assert.Equal(t, "openai", short.Provider)
	assert.Equal(t, "gpt-5-nano", short.Model)

	long, err := rules.Route(context.Background(), router.Request{EstimatedInputTokens: 500})
	require.NoError(t, err)
	assert.Equal(t, "openai", long.Provider)
	assert.Equal(t, "gpt-5", long.Model)
}
