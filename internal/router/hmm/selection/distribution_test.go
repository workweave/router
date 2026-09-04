package selection_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"weave-os/router/internal/router/hmm/rosterdata"
	"weave-os/router/internal/router/hmm/selection"
)

func TestRoutingDistributionUsesLivePreferenceScorer(t *testing.T) {
	roster := &rosterdata.Roster{
		SchemaVersion: rosterdata.SchemaVersionV7,
		Ranking: rosterdata.Ranking{
			Alpha:              map[string]float64{"low": 0.4},
			AlphaMin:           map[string]float64{"low": 0.05},
			AlphaMax:           map[string]float64{"low": 0.8},
			QualityBiasNeutral: 0.7,
		},
		Clusters: map[string]rosterdata.Cluster{
			"low": {
				Arms: []string{"openai/gpt-5.6-luna", "x-ai/grok-4.6"},
				ArmScores: map[string]float64{
					"openai/gpt-5.6-luna": 30,
					"x-ai/grok-4.6":       25,
				},
				ArmIndices: map[string]rosterdata.ArmIndices{
					"openai/gpt-5.6-luna": {WII: 90, WPI: 10},
					"x-ai/grok-4.6":       {WII: 55, WPI: 0},
				},
			},
		},
	}

	points, err := selection.RoutingDistribution(roster, 3, nil, nil)
	require.NoError(t, err)
	require.Len(t, points, 3)
	require.Len(t, points[0].Models, 1)
	require.Len(t, points[2].Models, 1)
	assert.NotEqual(t, points[0].Models[0].Model, points[2].Models[0].Model)
	assert.Equal(t, 1.0, points[0].Models[0].Share)
	assert.Positive(t, points[0].ProjectedCostPer1KInputUSD)
}

func TestRoutingDistributionHonorsExclusions(t *testing.T) {
	roster := dynamicRoster()
	// Replace test-only IDs with catalog-backed IDs so distribution can resolve them.
	cluster := roster.Clusters["low"]
	cluster.Arms = []string{"openai/gpt-5.6-luna", "x-ai/grok-4.6"}
	cluster.ArmScores = map[string]float64{"openai/gpt-5.6-luna": 30, "x-ai/grok-4.6": 25}
	cluster.ArmIndices = map[string]rosterdata.ArmIndices{
		"openai/gpt-5.6-luna": {WII: 90, WPI: 10},
		"x-ai/grok-4.6":       {WII: 55, WPI: 0},
	}
	roster.Clusters["low"] = cluster

	points, err := selection.RoutingDistribution(
		roster,
		2,
		map[string]struct{}{"gpt-5.6-luna": {}},
		nil,
	)
	require.NoError(t, err)
	for _, point := range points {
		require.Len(t, point.Models, 1)
		assert.NotEqual(t, "gpt-5.6-luna", point.Models[0].Model)
	}
}
