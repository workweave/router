package hmm

import (
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/router/cluster"
)

// DeployedModelsForRosterIDs maps sidecar roster IDs to catalog {model, provider} entries.
// Unknown IDs are dropped; first occurrence wins on duplicates.
func DeployedModelsForRosterIDs(rosterIDs []string) []cluster.DeployedEntry {
	inverse := make(map[string]catalog.Model, len(catalog.Models))
	for _, m := range catalog.Models {
		rosterID := rosterIDFor(m)
		if rosterID == "" {
			continue
		}
		if _, exists := inverse[rosterID]; !exists {
			inverse[rosterID] = m
		}
	}

	out := make([]cluster.DeployedEntry, 0, len(rosterIDs))
	seen := make(map[string]struct{}, len(rosterIDs))
	for _, rosterID := range rosterIDs {
		m, ok := inverse[rosterID]
		if !ok {
			continue
		}
		if _, dup := seen[m.ID]; dup {
			continue
		}
		seen[m.ID] = struct{}{}
		out = append(out, cluster.DeployedEntry{Model: m.ID, Provider: m.PrimaryProvider()})
	}
	return out
}
