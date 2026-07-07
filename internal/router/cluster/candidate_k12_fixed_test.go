package cluster

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCandidateK12FixedLoads is the load-gate for the corrected candidate-k12
// bake-off bundle. Two fixes vs candidate-k12: (1) opus-4-8 quality restored to
// v0.72's proper 0.554 (was cyber-poisoned to ~0.15), (2) alpha 0.7 -> 0.96
// (quality-first). Also trims the superseded opus-4-7 / sonnet-4-6 / kimi-k2.6
// (roster 21 -> 18). Asserts the bundle parses, is K=12 v2, opus-4-8 is restored
// and routable, and the trimmed models are gone.
func TestCandidateK12FixedLoads(t *testing.T) {
	bundle, err := LoadBundle("candidate-k12-fixed")
	require.NoError(t, err, "candidate-k12-fixed must parse end-to-end")
	require.True(t, bundle.IsV2, "candidate-k12-fixed is a v2 bundle")

	require.NotNil(t, bundle.Centroids)
	assert.Equal(t, 12, bundle.Centroids.K)
	assert.Equal(t, 768, bundle.Centroids.Dim)

	models := bundle.Registry.Models()
	assert.Len(t, models, 18, "candidate-k12-fixed roster is 18 (21 - 3 superseded)")

	// trimmed models must be gone.
	for _, m := range []string{"claude-opus-4-7", "claude-sonnet-4-6", "moonshotai/kimi-k2.6"} {
		assert.NotContains(t, models, m, "%s is superseded and must be trimmed", m)
	}
	// opus-4-8 restored + fable-5 present + routable.
	for _, m := range []string{"claude-opus-4-8", "claude-fable-5"} {
		assert.Contains(t, models, m)
		_, ok := bundle.ModelAxes[m]
		assert.Truef(t, ok, "%s must have operational axes", m)
	}

	// quality present for every (cluster, model), and opus-4-8 restored to ~0.554.
	for k := 0; k < bundle.Centroids.K; k++ {
		row, ok := bundle.QualityMeans[k]
		require.Truef(t, ok, "quality_means missing cluster %d", k)
		for _, m := range models {
			_, ok := row[m]
			require.Truef(t, ok, "quality_means cluster %d missing model %q", k, m)
		}
		assert.InDeltaf(t, 0.554, row["claude-opus-4-8"], 0.001,
			"opus-4-8 quality must be restored to v0.72's 0.554 in cluster %d", k)
	}
}
