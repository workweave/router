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

// hmmRosterRetryBackoff caps how often a failing refresh re-hits the sidecar
// while stale data is being served. Without it, every reader after TTL expiry
// would re-enter the slow failing fetch during a sidecar outage.
const hmmRosterRetryBackoff = 30 * time.Second

// rosterFetcher is the subset of the policy sidecar client the roster source
// needs; satisfied by *policyclient.Client.
type rosterFetcher interface {
	Roster(ctx context.Context) ([]string, error)
}

// hmmRosterSource adapts HMM sidecar roster arms to deployed-model entries.
// On fetch failure it serves the prior snapshot rather than blanking the roster.
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
// from the sidecar roster arms to catalog {model, provider} entries. The lock
// is not held across the sidecar fetch, so concurrent readers don't serialize
// behind one slow call; a post-expiry fetch failure serves stale data and
// backs off (hmmRosterRetryBackoff) so an outage doesn't re-hit the failing
// sidecar on every request.
func (s *hmmRosterSource) HMMDeployedModels(ctx context.Context) (entries []cluster.DeployedEntry, err error) {
	s.mu.Lock()
	if s.haveCache && time.Since(s.fetchedAt) < hmmRosterTTL {
		cached := cloneDeployedEntries(s.cached)
		s.mu.Unlock()
		return cached, nil
	}
	s.mu.Unlock()

	rosterIDs, fetchErr := s.fetch.Roster(ctx)
	if fetchErr != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.haveCache {
			// Keep serving stale, but treat it as fresh for a short backoff so
			// the next reader doesn't immediately re-enter this failing fetch.
			s.fetchedAt = time.Now().Add(hmmRosterRetryBackoff - hmmRosterTTL)
			return cloneDeployedEntries(s.cached), nil
		}
		return nil, fetchErr
	}

	mapped := hmm.DeployedModelsForRosterIDs(rosterIDs)
	s.mu.Lock()
	s.cached = mapped
	s.fetchedAt = time.Now()
	s.haveCache = true
	s.mu.Unlock()
	return cloneDeployedEntries(mapped), nil
}

func cloneDeployedEntries(in []cluster.DeployedEntry) []cluster.DeployedEntry {
	out := make([]cluster.DeployedEntry, len(in))
	copy(out, in)
	return out
}
