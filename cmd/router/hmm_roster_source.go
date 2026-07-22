package main

import (
	"context"
	"sync"
	"time"

	"workweave/router/internal/router/cluster"
	"workweave/router/internal/router/hmm"
)

// hmmRosterTTL reuses the same freshness window as the control plane's own
// deployed-models cache; the HMM roster only changes on a bandit-state swap.
const hmmRosterTTL = 5 * time.Minute

// rosterFetcher is the subset of the policy sidecar client the roster source
// needs; satisfied by *policyclient.Client.
type rosterFetcher interface {
	Roster(ctx context.Context) ([]string, error)
}

// hmmRosterSource adapts the HMM sidecar's roster arms into the deployed-models
// shape the control plane reads. It caches per hmmRosterTTL and, on a fetch
// failure with a prior snapshot, serves the stale list rather than blanking the
// settings roster — the same stale-on-failure contract as the cluster catalog.
type hmmRosterSource struct {
	fetch rosterFetcher

	mu        sync.Mutex
	cached    []cluster.DeployedEntry
	fetchedAt time.Time
	haveCache bool
}

func newHMMRosterSource(fetch rosterFetcher) *hmmRosterSource {
	return &hmmRosterSource{fetch: fetch}
}

// HMMDeployedModels returns the models the HMM strategy routes across, mapped
// from the sidecar roster arms to catalog {model, provider} entries.
func (s *hmmRosterSource) HMMDeployedModels(ctx context.Context) (entries []cluster.DeployedEntry, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.haveCache && time.Since(s.fetchedAt) < hmmRosterTTL {
		return cloneDeployedEntries(s.cached), nil
	}

	rosterIDs, fetchErr := s.fetch.Roster(ctx)
	if fetchErr != nil {
		if s.haveCache {
			return cloneDeployedEntries(s.cached), nil
		}
		return nil, fetchErr
	}

	mapped := hmm.DeployedModelsForRosterIDs(rosterIDs)
	s.cached = mapped
	s.fetchedAt = time.Now()
	s.haveCache = true
	return cloneDeployedEntries(mapped), nil
}

func cloneDeployedEntries(in []cluster.DeployedEntry) []cluster.DeployedEntry {
	out := make([]cluster.DeployedEntry, len(in))
	copy(out, in)
	return out
}
