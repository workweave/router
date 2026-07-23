package main

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"workweave/router/internal/policyclient"
	"workweave/router/internal/router/cluster"
	"workweave/router/internal/router/hmm"
)

// hmmRosterTTL reuses the same freshness window as the control plane's own
// deployed-models cache; the HMM roster only changes on a bandit-state swap.
const hmmRosterTTL = 5 * time.Minute

// hmmRosterRetryBackoff caps how often a failing refresh re-hits the sidecar
// while stale data is being served, to avoid hammering it during an outage.
const hmmRosterRetryBackoff = 30 * time.Second

// rosterFetcher is the subset of the policy sidecar client the roster source
// needs; satisfied by *policyclient.Client.
type rosterFetcher interface {
	Roster(ctx context.Context) ([]string, error)
}

// hmmRosterSource adapts HMM sidecar roster arms to deployed-model entries.
// On fetch failure it serves the prior snapshot rather than blanking the roster.
type hmmRosterSource struct {
	fetch        rosterFetcher
	fetchTimeout time.Duration

	// group collapses concurrent refreshes via singleflight — exactly one
	// writer per refresh, no clobber risk from a slow fetch racing a newer one.
	group singleflight.Group

	mu        sync.Mutex
	cached    []cluster.DeployedEntry
	fetchedAt time.Time
	haveCache bool
}

// newHMMRosterSource initializes the cached HMM roster source.
func newHMMRosterSource(fetch rosterFetcher, fetchTimeout time.Duration) *hmmRosterSource {
	if fetchTimeout <= 0 {
		fetchTimeout = policyclient.DefaultTimeout
	}
	return &hmmRosterSource{fetch: fetch, fetchTimeout: fetchTimeout}
}

// HMMDeployedModels returns catalog entries for the HMM roster. Serves a
// cached snapshot when fresh; collapses concurrent refreshes via singleflight.
// On failure with a prior snapshot returns stale; otherwise surfaces the error.
func (s *hmmRosterSource) HMMDeployedModels(ctx context.Context) (entries []cluster.DeployedEntry, err error) {
	s.mu.Lock()
	if s.haveCache && time.Since(s.fetchedAt) < hmmRosterTTL {
		cached := cloneDeployedEntries(s.cached)
		s.mu.Unlock()
		return cached, nil
	}
	s.mu.Unlock()

	// singleflight collapses concurrent callers onto one refresh; the shared
	// fetch runs under a detached context so a cancelling caller doesn't abort
	// others. Each caller honors its own ctx via the select below.
	ch := s.group.DoChan("roster", func() (any, error) {
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.fetchTimeout)
		defer cancel()
		return s.refresh(fetchCtx)
	})

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return cloneDeployedEntries(res.Val.([]cluster.DeployedEntry)), nil
	}
}

// refresh fetches the roster from the sidecar and updates the cache;
// on failure with a prior snapshot backs off rather than erroring.
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
