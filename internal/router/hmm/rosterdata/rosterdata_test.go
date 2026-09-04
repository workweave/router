package rosterdata_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"weave-os/router/internal/router/hmm/rosterdata"
)

func TestLoadValidRoster(t *testing.T) {
	roster, err := rosterdata.Load(filepath.Join("testdata", "roster_valid.json"))
	require.NoError(t, err)

	assert.Equal(t, rosterdata.SchemaVersionV6, roster.SchemaVersion)
	require.Len(t, roster.Clusters, 2)

	low := roster.Clusters["low"]
	assert.Equal(t, "low", low.ComplexityLabel)
	assert.Equal(t, []string{"openai/gpt-5.6-luna", "anthropic/claude-haiku-4.5"}, low.Arms)
	assert.Equal(t, []string{"openai/gpt-5.6-luna"}, low.ArmsByHarness["pi"])
	assert.InDelta(t, 0.02, low.CostRefUSD, 1e-9)
	assert.InDelta(t, 8000, low.LatencyRefMS, 1e-9)
	assert.InDelta(t, 20.11744, low.ArmScores["openai/gpt-5.6-luna"], 1e-9)

	maximum := roster.Clusters["maximum"]
	assert.Equal(t, []string{"anthropic/claude-opus-4.8", "openai/gpt-5.6-sol:high", "x-ai/grok-4.5"}, maximum.MembershipByHarness["claude_code"])

	assert.InDelta(t, 0.4, roster.Ranking.Alpha["low"], 1e-9)
	assert.InDelta(t, 0.95, roster.Ranking.Alpha["maximum"], 1e-9)
}

func TestLoadUnknownArmFails(t *testing.T) {
	_, err := rosterdata.Load(filepath.Join("testdata", "roster_unknown_arm.json"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "madeup/not-a-model")
	assert.Contains(t, err.Error(), "invalid arms")
}

func TestLoadMissingFile(t *testing.T) {
	_, err := rosterdata.Load(filepath.Join("testdata", "does_not_exist.json"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read roster")
}

func TestAllArmsUnionsClusterHarnessAndMembership(t *testing.T) {
	roster, err := rosterdata.Load(filepath.Join("testdata", "roster_valid.json"))
	require.NoError(t, err)

	assert.Equal(t, []string{
		"anthropic/claude-haiku-4.5",
		"anthropic/claude-opus-4.8",
		"openai/gpt-5.6-luna",
		"openai/gpt-5.6-sol:high",
		"x-ai/grok-4.5",
	}, roster.AllArms())
}

func TestParseUnknownFieldsTolerated(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "roster_valid.json"))
	require.NoError(t, err)

	roster, parseErr := rosterdata.Parse(data)
	require.NoError(t, parseErr)
	assert.Equal(t, rosterdata.SchemaVersionV6, roster.SchemaVersion)
}

func TestParseSchemaErrors(t *testing.T) {
	valid, err := os.ReadFile(filepath.Join("testdata", "roster_valid.json"))
	require.NoError(t, err)

	cases := []struct {
		name    string
		mutate  func(t *testing.T, doc []byte) []byte
		wantErr string
	}{
		{
			name:    "not json",
			mutate:  func(t *testing.T, doc []byte) []byte { return []byte("{not json") },
			wantErr: "parse roster",
		},
		{
			name:    "wrong type for clusters",
			mutate:  func(t *testing.T, doc []byte) []byte { return []byte(`{"schema_version":"v6","clusters":[1,2]}`) },
			wantErr: "parse roster",
		},
		{
			name:    "missing schema_version",
			mutate:  replaceJSON(`"schema_version": "hmm_router_cluster_roster_v6"`, `"schema_version": ""`),
			wantErr: "missing schema_version",
		},
		{
			name:    "no clusters",
			mutate:  func(t *testing.T, doc []byte) []byte { return []byte(`{"schema_version":"v6","clusters":{}}`) },
			wantErr: "no clusters",
		},
		{
			name: "empty arms",
			mutate: replaceJSON(`"openai/gpt-5.6-luna",
        "anthropic/claude-haiku-4.5"
      ],
      "arms_by_harness"`, `],
      "arms_by_harness"`),
			wantErr: `cluster "low" has no arms`,
		},
		{
			name:    "non-positive cost_ref_usd",
			mutate:  replaceJSON(`"cost_ref_usd": 0.02`, `"cost_ref_usd": 0`),
			wantErr: "non-positive cost_ref_usd",
		},
		{
			name:    "non-positive latency_ref_ms",
			mutate:  replaceJSON(`"latency_ref_ms": 8000`, `"latency_ref_ms": -1`),
			wantErr: "non-positive latency_ref_ms",
		},
		{
			name:    "arm without score",
			mutate:  replaceJSON(`"anthropic/claude-haiku-4.5": 12.5`, `"unrelated/model": 12.5`),
			wantErr: "no arm_scores entry",
		},
		{
			name:    "cluster without alpha",
			mutate:  replaceJSON(`"low": 0.4,`, `"other": 0.4,`),
			wantErr: `cluster "low" has no ranking.alpha entry`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, parseErr := rosterdata.Parse(tc.mutate(t, valid))
			require.Error(t, parseErr)
			assert.Contains(t, parseErr.Error(), tc.wantErr)
		})
	}
}

func TestParseDynamicRoster(t *testing.T) {
	roster, err := rosterdata.Parse([]byte(`{
  "schema_version": "hmm_router_cluster_roster_v7",
  "ranking": {
    "alpha": {"low": 0.4},
    "alpha_min": {"low": 0.05},
    "alpha_max": {"low": 0.8},
    "quality_bias_neutral": 0.7,
    "wii_score_version": "wii-v1",
    "wii_normalization_sha256": "wii-sha",
    "wpi_score_version": "wpi-v1",
    "wpi_normalization_sha256": "wpi-sha"
  },
  "clusters": {
    "low": {
      "complexity_label": "low",
      "arms": ["provider/scored"],
      "arms_by_harness": {"pi": ["provider/manual"]},
      "cost_ref_usd": 0.02,
      "latency_ref_ms": 8000,
      "arm_scores": {"provider/scored": 10, "provider/manual": 20},
      "arm_indices": {"provider/scored": {"wii_v1": 50, "wpi_v1": 10}},
      "manual_pins_by_harness": {"pi": ["provider/manual"]},
      "preferred_vendors_by_harness": {"codex": ["provider"]}
    }
  }
}`))
	require.NoError(t, err)
	assert.Equal(t, 50.0, roster.Clusters["low"].ArmIndices["provider/scored"].WII)
	assert.Equal(t, []string{"provider/manual"}, roster.Clusters["low"].ManualPinsByHarness["pi"])
}

func TestParseDynamicRosterRejectsMissingIndices(t *testing.T) {
	_, err := rosterdata.Parse([]byte(`{
  "schema_version": "hmm_router_cluster_roster_v7",
  "ranking": {
    "alpha": {"low": 0.4}, "alpha_min": {"low": 0.05}, "alpha_max": {"low": 0.8},
    "quality_bias_neutral": 0.7,
    "wii_score_version": "wii-v1", "wii_normalization_sha256": "wii-sha",
    "wpi_score_version": "wpi-v1", "wpi_normalization_sha256": "wpi-sha"
  },
  "clusters": {
    "low": {
      "complexity_label": "low", "arms": ["provider/unscored"],
      "cost_ref_usd": 0.02, "latency_ref_ms": 8000,
      "arm_scores": {"provider/unscored": 10}, "arm_indices": {}
    }
  }
}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no arm_indices entry")
}

func TestParseDynamicRosterRejectsHarnessPinWithoutGlobalCoverage(t *testing.T) {
	_, err := rosterdata.Parse([]byte(`{
  "schema_version": "hmm_router_cluster_roster_v7",
  "ranking": {
    "alpha": {"low": 0.4}, "alpha_min": {"low": 0.05}, "alpha_max": {"low": 0.8},
    "quality_bias_neutral": 0.7,
    "wii_score_version": "wii-v1", "wii_normalization_sha256": "wii-sha",
    "wpi_score_version": "wpi-v1", "wpi_normalization_sha256": "wpi-sha"
  },
  "clusters": {
    "low": {
      "complexity_label": "low", "arms": ["provider/scored", "provider/manual"],
      "arms_by_harness": {"pi": ["provider/manual"]},
      "cost_ref_usd": 0.02, "latency_ref_ms": 8000,
      "arm_scores": {"provider/scored": 10, "provider/manual": 20},
      "arm_indices": {"provider/scored": {"wii_v1": 50, "wpi_v1": 10}},
      "manual_pins_by_harness": {"pi": ["provider/manual"]}
    }
  }
}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `arm "provider/manual" has no arm_indices entry`)
}

// replaceJSON returns a mutator that swaps one occurrence of old for new in
// the fixture document, failing the test if old is absent.
func replaceJSON(old, new string) func(t *testing.T, doc []byte) []byte {
	return func(t *testing.T, doc []byte) []byte {
		t.Helper()
		s := string(doc)
		require.Contains(t, s, old)
		return []byte(strings.Replace(s, old, new, 1))
	}
}
