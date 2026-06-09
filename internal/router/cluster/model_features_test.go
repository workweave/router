package cluster

import (
	"path"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFeaturesMatchQualityMeans is the no-op equivalence gate. The quality and
// axes tables derived from model_features.json must be identical to those
// loaded from quality_means.json + model_axes.json for the shipped roster.
// Identical tables imply identical routing, so consuming model_features.json is
// a true no-op on the current roster (it only changes how a future model is
// onboarded: one appended column instead of a retrain).
func TestFeaturesMatchQualityMeans(t *testing.T) {
	const version = "v0.65"
	dir := bundleDirForVersion(version)

	rawCentroids, err := embeddedArtifacts.ReadFile(path.Join(dir, "centroids.bin"))
	require.NoError(t, err)
	centroids, err := loadCentroids(rawCentroids)
	require.NoError(t, err)

	rawQM, err := embeddedArtifacts.ReadFile(path.Join(dir, "quality_means.json"))
	require.NoError(t, err)
	qualityMeans, err := loadQualityMeans(rawQM)
	require.NoError(t, err)

	rawAxes, err := embeddedArtifacts.ReadFile(path.Join(dir, "model_axes.json"))
	require.NoError(t, err)
	axes, err := loadModelAxes(rawAxes)
	require.NoError(t, err)

	rawRegistry, err := embeddedArtifacts.ReadFile(path.Join(dir, "model_registry.json"))
	require.NoError(t, err)
	registry, err := loadRegistry(rawRegistry)
	require.NoError(t, err)

	rawFeatures, err := embeddedArtifacts.ReadFile(path.Join(dir, "model_features.json"))
	require.NoErrorf(t, err, "model_features.json must be embedded for %s", version)
	featureQualityMeans, featureAxes, err := loadModelFeatures(rawFeatures, centroids.K)
	require.NoError(t, err)

	for _, m := range registry.Models() {
		for k := 0; k < centroids.K; k++ {
			want, ok := qualityMeans[k][m]
			require.Truef(t, ok, "quality_means.json missing %s cluster %d", m, k)
			got, ok := featureQualityMeans[k][m]
			require.Truef(t, ok, "model_features.json missing %s cluster %d", m, k)
			require.Equalf(t, want, got, "quality cell mismatch for %s cluster %d", m, k)
		}
		require.Equalf(t, axes[m], featureAxes[m], "operational axis mismatch for %s", m)
	}
}

// TestLoadModelFeaturesRejectsWrongK guards the K-consistency check: a
// psi_probe column whose length does not match the centroid count must fail
// fast at load (a column built against a different artifact version).
func TestLoadModelFeaturesRejectsWrongK(t *testing.T) {
	raw := []byte(`{"models":{"m":{"psi_probe":[0.1,0.2,0.3],"operational":{}}}}`)
	_, _, err := loadModelFeatures(raw, 16)
	require.Error(t, err)
	require.Contains(t, err.Error(), "psi_probe length 3")
}
