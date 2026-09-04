package selection_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"weave-os/router/internal/router/hmm/rosterdata"
	"weave-os/router/internal/router/hmm/selection"
)

func candidateSet(ids ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return set
}

func testRoster() *rosterdata.Roster {
	return &rosterdata.Roster{
		SchemaVersion: rosterdata.SchemaVersionV6,
		Clusters: map[string]rosterdata.Cluster{
			"low": {
				Arms: []string{"vendor-a/cheap", "vendor-b/cheap"},
				ArmsByHarness: map[string][]string{
					"claude-code": {"vendor-b/cheap", "vendor-a/cheap"},
					"codex_cli":   {"vendor-b/cheap"},
				},
			},
			"balanced": {
				Arms: []string{"vendor-a/mid", "vendor-b/mid", "vendor-a/cheap"},
			},
			"high": {
				Arms: []string{"vendor-a/top", "vendor-b/top"},
			},
			"effort": {
				Arms: []string{"vendor-a/deep:high", "vendor-a/deep"},
			},
			"efforts": {
				Arms: []string{"vendor-a/deep:xhigh", "vendor-a/deep:low", "vendor-a/deep"},
			},
		},
	}
}

func dynamicRoster() *rosterdata.Roster {
	return &rosterdata.Roster{
		SchemaVersion: rosterdata.SchemaVersionV7,
		Ranking: rosterdata.Ranking{
			Alpha:              map[string]float64{"low": 0.4},
			AlphaMin:           map[string]float64{"low": 0.05},
			AlphaMax:           map[string]float64{"low": 0.8},
			QualityBiasNeutral: 0.7,
		},
		Clusters: map[string]rosterdata.Cluster{
			"low": {
				Arms: []string{"vendor-a/quality", "vendor-b/cheap"},
				ArmScores: map[string]float64{
					"vendor-a/quality": 30,
					"vendor-b/cheap":   25,
				},
				ArmIndices: map[string]rosterdata.ArmIndices{
					"vendor-a/quality": {WII: 90, WPI: 10},
					"vendor-b/cheap":   {WII: 55, WPI: 0},
				},
			},
		},
	}
}

func TestSelectGroupsWithPreferenceReweightsWithinClassifierBand(t *testing.T) {
	roster := dynamicRoster()
	candidates := candidateSet("vendor-a/quality", "vendor-b/cheap")
	groups := []selection.Group{{Label: "low"}}

	neutral := 0.7
	pick, scores, ok := selection.SelectGroupsWithPreference(roster, groups, "", candidates, &neutral)
	require.True(t, ok)
	assert.Equal(t, "vendor-a/quality", pick.Arm)
	assert.Equal(t, float32(30), scores["low"]["vendor-a/quality"])

	priceHeavy := 0.0
	pick, _, ok = selection.SelectGroupsWithPreference(roster, groups, "", candidates, &priceHeavy)
	require.True(t, ok)
	assert.Equal(t, "vendor-b/cheap", pick.Arm)

	qualityHeavy := 1.0
	pick, _, ok = selection.SelectGroupsWithPreference(roster, groups, "", candidates, &qualityHeavy)
	require.True(t, ok)
	assert.Equal(t, "vendor-a/quality", pick.Arm)
}

func TestSelectGroupsWithPreferencePreservesPolicyTiers(t *testing.T) {
	roster := dynamicRoster()
	cluster := roster.Clusters["low"]
	cluster.ManualPinsByHarness = map[string][]string{"pi": {"vendor-b/cheap"}}
	cluster.PreferredVendorsByHarness = map[string][]string{"codex": {"vendor-b"}}
	roster.Clusters["low"] = cluster
	qualityHeavy := 1.0
	candidates := candidateSet("vendor-a/quality", "vendor-b/cheap")

	pick, _, ok := selection.SelectGroupsWithPreference(roster, []selection.Group{{Label: "low"}}, "pi", candidates, &qualityHeavy)
	require.True(t, ok)
	assert.Equal(t, "vendor-b/cheap", pick.Arm, "manual pin must outrank the dynamic score")

	pick, _, ok = selection.SelectGroupsWithPreference(roster, []selection.Group{{Label: "low"}}, "codex", candidates, &qualityHeavy)
	require.True(t, ok)
	assert.Equal(t, "vendor-b/cheap", pick.Arm, "vendor affinity must outrank the dynamic score")
}

func TestSelectGroupsWithPreferenceKeepsFallbackOrder(t *testing.T) {
	roster := dynamicRoster()
	high := roster.Clusters["low"]
	high.Arms = []string{"vendor-c/high"}
	high.ArmScores = map[string]float64{"vendor-c/high": 80}
	high.ArmIndices = map[string]rosterdata.ArmIndices{"vendor-c/high": {WII: 100, WPI: 100}}
	roster.Clusters["high"] = high
	roster.Ranking.Alpha["high"] = 0.85
	roster.Ranking.AlphaMin["high"] = 0.6
	roster.Ranking.AlphaMax["high"] = 0.98
	qualityHeavy := 1.0

	pick, _, ok := selection.SelectGroupsWithPreference(
		roster,
		[]selection.Group{{Label: "low"}, {Label: "high"}},
		"",
		candidateSet("vendor-a/quality", "vendor-c/high"),
		&qualityHeavy,
	)
	require.True(t, ok)
	assert.Equal(t, "low", pick.Group)
}

func TestArmOrder(t *testing.T) {
	roster := testRoster()

	order, harnessSpecific := selection.ArmOrder(roster.Clusters["low"], "claude-code")
	assert.True(t, harnessSpecific)
	assert.Equal(t, []string{"vendor-b/cheap", "vendor-a/cheap"}, order)

	order, harnessSpecific = selection.ArmOrder(roster.Clusters["low"], "codex")
	assert.False(t, harnessSpecific)
	assert.Equal(t, []string{"vendor-a/cheap", "vendor-b/cheap"}, order)

	// Roster harness keys use underscores while ClientApp is hyphenated.
	order, harnessSpecific = selection.ArmOrder(roster.Clusters["low"], "codex-cli")
	assert.True(t, harnessSpecific)
	assert.Equal(t, []string{"vendor-b/cheap"}, order)

	order, harnessSpecific = selection.ArmOrder(roster.Clusters["balanced"], "claude-code")
	assert.False(t, harnessSpecific)
	assert.Equal(t, []string{"vendor-a/mid", "vendor-b/mid", "vendor-a/cheap"}, order)
}

func TestSelect(t *testing.T) {
	roster := testRoster()

	tests := []struct {
		name         string
		rankedGroups []string
		harness      string
		candidates   map[string]struct{}
		wantOK       bool
		want         selection.Pick
	}{
		{
			name:         "rank one arm of the top group wins",
			rankedGroups: []string{"balanced", "low", "high"},
			candidates:   candidateSet("vendor-b/mid", "vendor-a/mid", "vendor-a/cheap"),
			wantOK:       true,
			want:         selection.Pick{Group: "balanced", Arm: "vendor-a/mid"},
		},
		{
			name:         "lower-ranked roster arm loses to roster order not candidate order",
			rankedGroups: []string{"balanced"},
			candidates:   candidateSet("vendor-a/cheap", "vendor-b/mid"),
			wantOK:       true,
			want:         selection.Pick{Group: "balanced", Arm: "vendor-b/mid"},
		},
		{
			name:         "empty intersection falls back to the next ranked group",
			rankedGroups: []string{"high", "balanced", "low"},
			candidates:   candidateSet("vendor-a/cheap"),
			wantOK:       true,
			want:         selection.Pick{Group: "balanced", Arm: "vendor-a/cheap", FallbackDepth: 1},
		},
		{
			name:         "fallback walks every ranked group in order",
			rankedGroups: []string{"high", "balanced", "low"},
			candidates:   candidateSet("vendor-b/cheap"),
			wantOK:       true,
			want:         selection.Pick{Group: "low", Arm: "vendor-b/cheap", FallbackDepth: 2},
		},
		{
			name:         "harness-specific order flips the pick",
			rankedGroups: []string{"low"},
			harness:      "claude-code",
			candidates:   candidateSet("vendor-a/cheap", "vendor-b/cheap"),
			wantOK:       true,
			want:         selection.Pick{Group: "low", Arm: "vendor-b/cheap", HarnessOrder: true},
		},
		{
			name:         "unknown harness uses the pooled order",
			rankedGroups: []string{"low"},
			harness:      "codex",
			candidates:   candidateSet("vendor-a/cheap", "vendor-b/cheap"),
			wantOK:       true,
			want:         selection.Pick{Group: "low", Arm: "vendor-a/cheap"},
		},
		{
			name:         "hyphenated harness matches an underscore roster key",
			rankedGroups: []string{"low"},
			harness:      "codex-cli",
			candidates:   candidateSet("vendor-a/cheap", "vendor-b/cheap"),
			wantOK:       true,
			want:         selection.Pick{Group: "low", Arm: "vendor-b/cheap", HarnessOrder: true},
		},
		{
			name:         "effort-suffixed arm matches its base candidate roster ID",
			rankedGroups: []string{"effort"},
			candidates:   candidateSet("vendor-a/deep"),
			wantOK:       true,
			want:         selection.Pick{Group: "effort", Arm: "vendor-a/deep:high"},
		},
		{
			name:         "ranked label missing from the roster is walked past",
			rankedGroups: []string{"retired", "low"},
			candidates:   candidateSet("vendor-a/cheap"),
			wantOK:       true,
			want:         selection.Pick{Group: "low", Arm: "vendor-a/cheap", FallbackDepth: 1},
		},
		{
			name:         "no ranked group intersects the candidates",
			rankedGroups: []string{"high", "balanced", "low"},
			candidates:   candidateSet("vendor-c/other"),
			wantOK:       false,
		},
		{
			name:         "no ranked groups",
			rankedGroups: nil,
			candidates:   candidateSet("vendor-a/cheap"),
			wantOK:       false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pick, ok := selection.Select(roster, tc.rankedGroups, tc.harness, tc.candidates)
			require.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.want, pick)
			}
		})
	}
}

func TestSelectGroupsHonorsTheSidecarArmAllowlist(t *testing.T) {
	roster := testRoster()
	candidates := candidateSet("vendor-a/mid", "vendor-b/mid", "vendor-a/cheap")

	// Rank one of the group is a candidate but the sidecar excluded it (e.g. a
	// capability constraint), so the next allowed arm of the same group serves.
	pick, ok := selection.SelectGroups(
		roster,
		[]selection.Group{{Label: "balanced", AllowedArms: []string{"vendor-b/mid"}}},
		"",
		candidates,
	)
	require.True(t, ok)
	assert.Equal(t, selection.Pick{Group: "balanced", Arm: "vendor-b/mid"}, pick)

	// A group whose allowlist excludes every candidate falls through.
	pick, ok = selection.SelectGroups(
		roster,
		[]selection.Group{
			{Label: "balanced", AllowedArms: []string{"vendor-c/other"}},
			{Label: "low", AllowedArms: []string{"vendor-a/cheap"}},
		},
		"",
		candidates,
	)
	require.True(t, ok)
	assert.Equal(t, selection.Pick{Group: "low", Arm: "vendor-a/cheap", FallbackDepth: 1}, pick)

	// An empty allowlist is "no restriction", not "no arms": a sidecar roster
	// that disagrees with the router's must not shrink the candidate set.
	pick, ok = selection.SelectGroups(
		roster,
		[]selection.Group{{Label: "balanced"}},
		"",
		candidates,
	)
	require.True(t, ok)
	assert.Equal(t, selection.Pick{Group: "balanced", Arm: "vendor-a/mid"}, pick)

	// A bare allowlist entry names the base model and permits any of its
	// effort-qualified roster arms.
	pick, ok = selection.SelectGroups(
		roster,
		[]selection.Group{{Label: "effort", AllowedArms: []string{"vendor-a/deep"}}},
		"",
		candidateSet("vendor-a/deep"),
	)
	require.True(t, ok)
	assert.Equal(t, selection.Pick{Group: "effort", Arm: "vendor-a/deep:high"}, pick)
}

func TestSelectGroupsAllowlistDistinguishesEffortVariants(t *testing.T) {
	roster := testRoster()
	candidates := candidateSet("vendor-a/deep")

	// Each effort variant is a distinct arm; allowlisting one must not
	// admit a higher-effort arm that ranks above it.
	pick, ok := selection.SelectGroups(
		roster,
		[]selection.Group{{Label: "efforts", AllowedArms: []string{"vendor-a/deep:low"}}},
		"",
		candidates,
	)
	require.True(t, ok)
	assert.Equal(t, selection.Pick{Group: "efforts", Arm: "vendor-a/deep:low"}, pick)

	// An effort-qualified allowlist entry does not admit the unsuffixed arm,
	// which dispatches at the provider's default effort.
	pick, ok = selection.SelectGroups(
		roster,
		[]selection.Group{
			{Label: "efforts", AllowedArms: []string{"vendor-a/deep:medium"}},
			{Label: "low", AllowedArms: []string{"vendor-a/cheap"}},
		},
		"",
		candidateSet("vendor-a/deep", "vendor-a/cheap"),
	)
	require.True(t, ok)
	assert.Equal(t, selection.Pick{Group: "low", Arm: "vendor-a/cheap", FallbackDepth: 1}, pick)
}

func TestSelectIsDeterministic(t *testing.T) {
	roster := testRoster()
	candidates := candidateSet("vendor-a/mid", "vendor-b/mid", "vendor-a/cheap", "vendor-b/cheap")
	first, ok := selection.Select(roster, []string{"balanced", "low"}, "", candidates)
	require.True(t, ok)
	for range 100 {
		pick, ok := selection.Select(roster, []string{"balanced", "low"}, "", candidates)
		require.True(t, ok)
		require.Equal(t, first, pick)
	}
}

// TestSelectParityFixture pins expected picks against a fixture roster so
// loader/walk semantics drift shows up as a concrete pick change.
func TestSelectParityFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "parity_roster.json"))
	require.NoError(t, err)
	roster, err := rosterdata.Parse(data)
	require.NoError(t, err)

	tests := []struct {
		name         string
		rankedGroups []string
		harness      string
		candidates   []string
		wantGroup    string
		wantArm      string
	}{
		{
			name:         "full candidate set serves the top group's rank one",
			rankedGroups: []string{"balanced", "low", "high"},
			candidates:   []string{"vendor-a/mid", "vendor-b/mid", "vendor-a/cheap", "vendor-a/top"},
			wantGroup:    "balanced",
			wantArm:      "vendor-a/mid",
		},
		{
			name:         "rank one excluded serves rank two of the same group",
			rankedGroups: []string{"balanced", "low", "high"},
			candidates:   []string{"vendor-b/mid", "vendor-a/cheap", "vendor-a/top"},
			wantGroup:    "balanced",
			wantArm:      "vendor-b/mid",
		},
		{
			name:         "group exhausted falls through to the next ranked group",
			rankedGroups: []string{"balanced", "high", "low"},
			candidates:   []string{"vendor-a/top", "vendor-b/cheap"},
			wantGroup:    "high",
			wantArm:      "vendor-a/top",
		},
		{
			name:         "harness order overrides the pooled order",
			rankedGroups: []string{"low", "balanced", "high"},
			harness:      "claude-code",
			candidates:   []string{"vendor-a/cheap", "vendor-b/cheap"},
			wantGroup:    "low",
			wantArm:      "vendor-b/cheap",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pick, ok := selection.Select(roster, tc.rankedGroups, tc.harness, candidateSet(tc.candidates...))
			require.True(t, ok)
			assert.Equal(t, tc.wantGroup, pick.Group)
			assert.Equal(t, tc.wantArm, pick.Arm)
		})
	}
}
