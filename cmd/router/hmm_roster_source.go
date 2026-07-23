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

// newHMMRosterSource initializes the cached HMM roster source.
func newHMMRosterSource(fetch rosterFetcher) *hmmRosterSource {
	return &hmmRosterSource{fetch: fetch}
}

// HMMDeployedModels returns catalog entries for the HMM roster. The lock is
// not held across the fetch so concurrent callers don't serialize; a failing
// refresh with a prior snapshot serves stale and backs off. Neither the
// success nor the failure write-back can clobber a concurrent winner: both
// commit only while our in-flight marker is still the current fetchedAt.
func (s *hmmRosterSource) HMMDeployedModels(ctx context.Context) (entries []cluster.DeployedEntry, err error) {
	s.mu.Lock()
	if s.haveCache && time.Since(s.fetchedAt) < hmmRosterTTL {
		cached := cloneDeployedEntries(s.cached)
		s.mu.Unlock()
		return cached, nil
	}
	// Mark stale so a concurrent caller knows a fetch is in flight and
	// doesn't re-dispatch — a fresh fetch sets this back to now(), and a
	// failing fetch sets the backoff watermark.
	fetching := time.Now()
	s.fetchedAt = fetching
	s.mu.Unlock()

	rosterIDs, fetchErr := s.fetch.Roster(ctx)
	if fetchErr != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.haveCache {
			// A concurrent refresh that finished while this fetch was in
			// flight already set fetchedAt to a real timestamp. Only
			// back off if our stale marker is still the value there —
			// otherwise a slow failure would clobber the winner.
			if s.fetchedAt.Equal(fetching) {
				s.fetchedAt = time.Now().Add(hmmRosterRetryBackoff - hmmRosterTTL)
			}
			return cloneDeployedEntries(s.cached), nil
		}
		return nil, fetchErr
	}

	mapped := hmm.DeployedModelsForRosterIDs(rosterIDs)
	s.mu.Lock()
	// Same guard as the failure path: only commit if no concurrent refresh
	// has updated the cache since we marked it in flight, so a slower success
	// can't overwrite a newer winner. Either snapshot is valid to return.
	if s.fetchedAt.Equal(fetching) {
		s.cached = mapped
		s.fetchedAt = time.Now()
		s.haveCache = true
	}
	s.mu.Unlock()
	return cloneDeployedEntries(mapped), nil
}

func cloneDeployedEntries(in []cluster.DeployedEntry) []cluster.DeployedEntry {
	out := make([]cluster.DeployedEntry, len(in))
	copy(out, in)
	return out
}
