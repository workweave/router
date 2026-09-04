// Package rosterdata decodes and validates the generated HMM roster JSON as
// declarative data, replacing the sidecar-embedded roster. It is the input to the
// router's deterministic arm selection; see docs/HMM_GO_SELECTION.md.
package rosterdata

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"

	"weave-os/router/internal/router/hmm"
)

type SchemaVersion string

const (
	SchemaVersionV6   SchemaVersion = "hmm_router_cluster_roster_v6"
	SchemaVersionV7   SchemaVersion = "hmm_router_cluster_roster_v7"
	SchemaVersionV75C SchemaVersion = "hmm_router_cluster_roster_v7_5c"
)

// Roster is the parsed generated roster document. Unknown top-level fields
// are tolerated; the fields here are the ones the router consumes.
type Roster struct {
	SchemaVersion SchemaVersion      `json:"schema_version"`
	Ranking       Ranking            `json:"ranking"`
	Clusters      map[string]Cluster `json:"clusters"`
}

// Ranking carries the ranking metadata the roster builder used; Alpha is the
// per-cluster WMI blend weight.
type Ranking struct {
	Alpha                  map[string]float64 `json:"alpha"`
	AlphaMin               map[string]float64 `json:"alpha_min"`
	AlphaMax               map[string]float64 `json:"alpha_max"`
	QualityBiasNeutral     float64            `json:"quality_bias_neutral"`
	WIIScoreVersion        string             `json:"wii_score_version"`
	WIINormalizationSHA256 string             `json:"wii_normalization_sha256"`
	WPIScoreVersion        string             `json:"wpi_score_version"`
	WPINormalizationSHA256 string             `json:"wpi_normalization_sha256"`
}

// ArmIndices are the immutable quality and price axes used to dynamically
// rank an arm. Both are absolute 0-100 indices; WPI is not a serving price.
type ArmIndices struct {
	WII float64 `json:"wii_v1"`
	WPI float64 `json:"wpi_v1"`
}

// Cluster is one complexity cluster's ordered arm roster and reference costs.
type Cluster struct {
	ComplexityLabel           string                `json:"complexity_label"`
	Arms                      []string              `json:"arms"`
	ArmsByHarness             map[string][]string   `json:"arms_by_harness"`
	MembershipByHarness       map[string][]string   `json:"membership_by_harness"`
	CostRefUSD                float64               `json:"cost_ref_usd"`
	LatencyRefMS              float64               `json:"latency_ref_ms"`
	ArmScores                 map[string]float64    `json:"arm_scores"`
	ArmIndices                map[string]ArmIndices `json:"arm_indices"`
	ManualPinsByHarness       map[string][]string   `json:"manual_pins_by_harness"`
	PreferredVendorsByHarness map[string][]string   `json:"preferred_vendors_by_harness"`
}

// AllArms returns every distinct arm ID referenced by the roster — cluster
// arms, per-harness arms, and per-harness membership — in sorted order.
func (r *Roster) AllArms() []string {
	seen := make(map[string]struct{})
	for _, cluster := range r.Clusters {
		for _, arm := range cluster.Arms {
			seen[arm] = struct{}{}
		}
		for _, arms := range cluster.ArmsByHarness {
			for _, arm := range arms {
				seen[arm] = struct{}{}
			}
		}
		for _, arms := range cluster.MembershipByHarness {
			for _, arm := range arms {
				seen[arm] = struct{}{}
			}
		}
	}
	arms := make([]string, 0, len(seen))
	for arm := range seen {
		arms = append(arms, arm)
	}
	sort.Strings(arms)
	return arms
}

// Parse decodes and schema-validates a roster document. It does not check
// arms against the model catalog; Load does.
func Parse(data []byte) (*Roster, error) {
	var roster Roster
	if err := json.Unmarshal(data, &roster); err != nil {
		return nil, fmt.Errorf("rosterdata: parse roster: %w", err)
	}
	if err := validateSchema(&roster); err != nil {
		return nil, fmt.Errorf("rosterdata: invalid roster: %w", err)
	}
	return &roster, nil
}

// Load reads and fully validates the roster at path, including catalog
// validation of every arm via hmm.ValidateRosterIDs.
func Load(path string) (*Roster, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("rosterdata: read roster %q: %w", path, err)
	}
	roster, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if diagnostics := hmm.ValidateRosterIDs(roster.AllArms()); len(diagnostics) > 0 {
		lines := make([]string, 0, len(diagnostics))
		for _, d := range diagnostics {
			lines = append(lines, fmt.Sprintf("%s: %s", d.RosterID, d.Reason))
		}
		return nil, fmt.Errorf("rosterdata: roster %q has %d invalid arms: %s", path, len(diagnostics), strings.Join(lines, "; "))
	}
	return roster, nil
}

func validateSchema(r *Roster) error {
	if r.SchemaVersion == "" {
		return fmt.Errorf("missing schema_version")
	}
	if len(r.Clusters) == 0 {
		return fmt.Errorf("no clusters")
	}
	for label, cluster := range r.Clusters {
		if len(cluster.Arms) == 0 {
			return fmt.Errorf("cluster %q has no arms", label)
		}
		if cluster.CostRefUSD <= 0 {
			return fmt.Errorf("cluster %q has non-positive cost_ref_usd", label)
		}
		if cluster.LatencyRefMS <= 0 {
			return fmt.Errorf("cluster %q has non-positive latency_ref_ms", label)
		}
		for _, arm := range cluster.Arms {
			if _, ok := cluster.ArmScores[arm]; !ok {
				return fmt.Errorf("cluster %q arm %q has no arm_scores entry", label, arm)
			}
		}
		if _, ok := r.Ranking.Alpha[label]; !ok {
			return fmt.Errorf("cluster %q has no ranking.alpha entry", label)
		}
		if r.SchemaVersion == SchemaVersionV7 || r.SchemaVersion == SchemaVersionV75C {
			if err := validateDynamicCluster(r, label, cluster); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateDynamicCluster(r *Roster, label string, cluster Cluster) error {
	if r.Ranking.WIIScoreVersion == "" || r.Ranking.WIINormalizationSHA256 == "" ||
		r.Ranking.WPIScoreVersion == "" || r.Ranking.WPINormalizationSHA256 == "" {
		return fmt.Errorf("v7 roster has incomplete WII/WPI provenance")
	}
	neutral := r.Ranking.QualityBiasNeutral
	defaultAlpha := r.Ranking.Alpha[label]
	minAlpha, hasMin := r.Ranking.AlphaMin[label]
	maxAlpha, hasMax := r.Ranking.AlphaMax[label]
	if !finiteUnit(neutral) || neutral <= 0 || neutral >= 1 {
		return fmt.Errorf("ranking.quality_bias_neutral must be inside (0,1)")
	}
	if !hasMin || !hasMax || !finiteUnit(minAlpha) || !finiteUnit(defaultAlpha) || !finiteUnit(maxAlpha) || minAlpha > defaultAlpha || defaultAlpha > maxAlpha {
		return fmt.Errorf("cluster %q has invalid alpha calibration", label)
	}
	globalPins := armSet(cluster.ManualPinsByHarness["*"])
	for _, arm := range cluster.Arms {
		indices, ok := cluster.ArmIndices[arm]
		if !ok {
			if _, globallyPinned := globalPins[arm]; globallyPinned {
				continue
			}
			return fmt.Errorf("cluster %q arm %q has no arm_indices entry", label, arm)
		}
		if !finiteRange(indices.WII, 0, 100) || !finiteRange(indices.WPI, 0, 100) {
			return fmt.Errorf("cluster %q arm %q has invalid WII/WPI indices", label, arm)
		}
	}
	for harness, arms := range cluster.ArmsByHarness {
		harnessPins := armSet(cluster.ManualPinsByHarness[harness])
		for _, arm := range arms {
			indices, ok := cluster.ArmIndices[arm]
			if !ok {
				_, globallyPinned := globalPins[arm]
				_, pinnedForHarness := harnessPins[arm]
				if globallyPinned || pinnedForHarness {
					continue
				}
				return fmt.Errorf("cluster %q harness %q arm %q has no arm_indices entry", label, harness, arm)
			}
			if !finiteRange(indices.WII, 0, 100) || !finiteRange(indices.WPI, 0, 100) {
				return fmt.Errorf("cluster %q harness %q arm %q has invalid WII/WPI indices", label, harness, arm)
			}
		}
	}
	return nil
}

func armSet(arms []string) map[string]struct{} {
	set := make(map[string]struct{}, len(arms))
	for _, arm := range arms {
		set[arm] = struct{}{}
	}
	return set
}

func finiteUnit(value float64) bool { return finiteRange(value, 0, 1) }

func finiteRange(value, minimum, maximum float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= minimum && value <= maximum
}
