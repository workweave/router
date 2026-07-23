package main

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

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

	// group collapses concurrent refreshes into a single sidecar call, so a
	// stampede (including a cold cache) never fans out and there is exactly
	// one writer per refresh — no marker juggling to protect against a slow
	// fetch clobbering a newer one.
	group singleflight.Group

	mu        sync.Mutex
	cached    []cluster.DeployedEntry
	fetchedAt time.Time
	haveCache bool
}

// newHMMRosterSource initializes the cached HMM roster source.
func newHMMRosterSource(fetch rosterFetcher) *hmmRosterSource {
	return &hmmRosterSource{fetch: fetch}
}

// HMMDeployedModels returns catalog entries for the HMM roster. A fresh cache
// is served without any fetch; otherwise concurrent callers collapse onto one
// sidecar refresh (singleflight). A failing refresh with a prior snapshot
// serves stale and backs off so an outage doesn't hammer the sidecar; a
// failing refresh with no snapshot surfaces the error.
func (s *hmmRosterSource) HMMDeployedModels(ctx context.Context) (entries []cluster.DeployedEntry, err error) {
	s.mu.Lock()
	if s.haveCache && time.Since(s.fetchedAt) < hmmRosterTTL {
		cached := cloneDeployedEntries(s.cached)
		s.mu.Unlock()
		return cached, nil
	}
	s.mu.Unlock()

	// singleflight collapses a concurrent stampede onto one refresh. The
	// shared result is returned to every waiter; only that one goroutine
	// touches the cache, so there is no clobber race in either outcome.
	result, refreshErr, _ := s.group.Do("roster", func() (interface{}, error) {
		return s.refresh(ctx)
	})
	if refreshErr != nil {
		return nil, refreshErr
	}
	return cloneDeployedEntries(result.([]cluster.DeployedEntry)), nil
}

// refresh fetches the roster from the sidecar and updates the cache. On
// failure it serves the prior snapshot (extending fetchedAt by the backoff so
// the next refresh doesn't immediately re-hit a failing sidecar), or returns
// the error when the cache is cold. Runs under singleflight, so it is the sole
// writer for its refresh.
func (s *hmmRosterSource) refresh(ctx context.Context) ([]cluster.DeployedEntry, error) {
	rosterIDs, fetchErr := s.fetch.Roster(ctx)
	if fetchErr != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.haveCache {
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
	return mapped, nil
}

func cloneDeployedEntries(in []cluster.DeployedEntry) []cluster.DeployedEntry {
	out := make([]cluster.DeployedEntry, len(in))
	copy(out, in)
	return out
}
