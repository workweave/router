package policy

// ClusterOverrideResult is the outcome of applying per-key cluster allowlists
// to a sidecar decision.
type ClusterOverrideResult struct {
	// RosterID is the selected roster arm after overrides.
	RosterID string
	// ArmID is the resolver arm ID for the selected roster arm, so binding
	// resolution can go through ByArmID. On arm-enumerating resolvers a roster
	// ID can be ambiguous (shared across providers) and absent from ByRosterID;
	// the arm ID is always unambiguous. Empty when no arm maps to the selection.
	ArmID string
	// Group is the classifier group the selection came from.
	Group string
	// Applied is true when an override was configured for at least one ranked
	// group and produced a definite selection. When false, the caller keeps the
	// sidecar's own selection (fail-open / no override configured).
	Applied bool
	// Changed is true when the override selected a different arm than the
	// sidecar's own pick (for reason annotation and telemetry).
	Changed bool
}

// ApplyClusterArmOverrides re-selects the served arm under per-key cluster
// allowlists. Walks ranked fallback in order; for each group intersects the
// override's catalog IDs (mapped to roster IDs) with eligible candidates so
// global exclusions still win. Fail-open when no override matches any group.
func ApplyClusterArmOverrides(
	overrides map[string][]string,
	rankedFallback []PreviewGroup,
	resolved ResolvedCandidates,
	sidecarRosterID string,
) ClusterOverrideResult {
	if len(overrides) == 0 || len(rankedFallback) == 0 {
		return ClusterOverrideResult{}
	}

	catalogToRoster := make(map[string]string, len(resolved.Candidates))
	rosterToArm := make(map[string]string, len(resolved.Candidates))
	eligibleRosterIDs := make(map[string]struct{}, len(resolved.Candidates))
	for _, candidate := range resolved.Candidates {
		if _, exists := catalogToRoster[candidate.CatalogID]; !exists {
			catalogToRoster[candidate.CatalogID] = candidate.RosterID
		}
		if _, exists := rosterToArm[candidate.RosterID]; !exists {
			rosterToArm[candidate.RosterID] = candidate.ArmID
		}
		eligibleRosterIDs[candidate.RosterID] = struct{}{}
	}

	for _, group := range rankedFallback {
		override, hasOverride := overrides[group.Group]
		effective := effectiveArms(group, override, hasOverride, catalogToRoster, eligibleRosterIDs)
		if len(effective) == 0 {
			continue
		}
		selected := effective[0]
		return ClusterOverrideResult{
			RosterID: selected,
			ArmID:    rosterToArm[selected],
			Group:    group.Group,
			Applied:  true,
			Changed:  selected != sidecarRosterID,
		}
	}

	// Overrides were configured but emptied every ranked group's arms. Report
	// not-applied so the caller keeps the sidecar's selection (fail-open); the
	// alternative — no eligible arm anywhere — would hard-fail the turn.
	return ClusterOverrideResult{}
}

// effectiveArms returns the ordered eligible roster arms for one ranked group
// under an optional override.
func effectiveArms(
	group PreviewGroup,
	override []string,
	hasOverride bool,
	catalogToRoster map[string]string,
	eligibleRosterIDs map[string]struct{},
) []string {
	if !hasOverride {
		return group.EligibleArms
	}
	// An override may add models the artifact never placed in this cluster, so
	// eligibility uses the full request-resolved candidate set (honors global
	// exclusions/provider filters), not just this group's artifact arms.
	out := make([]string, 0, len(override))
	seen := make(map[string]struct{}, len(override))
	for _, catalogID := range override {
		rosterID, mapped := catalogToRoster[catalogID]
		if !mapped {
			continue
		}
		if _, eligible := eligibleRosterIDs[rosterID]; !eligible {
			continue
		}
		if _, dup := seen[rosterID]; dup {
			continue
		}
		seen[rosterID] = struct{}{}
		out = append(out, rosterID)
	}
	return out
}
