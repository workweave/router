package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/router/cluster"
)

type stubRosterFetcher struct {
	ids      []string
	err      error
	calls    atomic.Int64
	gate     chan struct{} // if non-nil, Roster blocks until closed (forces fetch overlap)
	entered  chan struct{} // if non-nil, Roster signals once per entry before blocking
	deadline chan time.Time
}

func (s *stubRosterFetcher) Roster(ctx context.Context) ([]string, error) {
	s.calls.Add(1)
	if s.deadline != nil {
		deadline, _ := ctx.Deadline()
		s.deadline <- deadline
	}
	if s.entered != nil {
		s.entered <- struct{}{}
	}
	if s.gate != nil {
		<-s.gate
	}
	return s.ids, s.err
}

func TestHMMRosterSource_MapsAndCaches(t *testing.T) {
	fetch := &stubRosterFetcher{ids: []string{"openai/gpt-5.6-sol"}}
	src := newHMMRosterSource(fetch, 0)

	first, err := src.HMMDeployedModels(context.Background())
	require.NoError(t, err)
	require.Len(t, first, 1)
	assert.Equal(t, "gpt-5.6-sol", first[0].Model)

	// Second call inside the TTL window must not re-hit the sidecar.
	second, err := src.HMMDeployedModels(context.Background())
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.Equal(t, int64(1), fetch.calls.Load())
}

func TestHMMRosterSource_UsesConfiguredFetchTimeout(t *testing.T) {
	const fetchTimeout = 10 * time.Second
	fetch := &stubRosterFetcher{
		ids:      []string{"openai/gpt-5.6-sol"},
		deadline: make(chan time.Time, 1),
	}
	src := newHMMRosterSource(fetch, fetchTimeout)

	startedAt := time.Now()
	_, err := src.HMMDeployedModels(context.Background())
	require.NoError(t, err)

	deadline := <-fetch.deadline
	assert.Greater(t, deadline.Sub(startedAt), 9*time.Second)
	assert.Less(t, deadline.Sub(startedAt), 11*time.Second)
}

func TestHMMRosterSource_ServesStaleOnFetchFailure(t *testing.T) {
	fetch := &stubRosterFetcher{ids: []string{"openai/gpt-5.6-sol"}}
	src := newHMMRosterSource(fetch, 0)

	_, err := src.HMMDeployedModels(context.Background())
	require.NoError(t, err)

	// Force the cache stale, then fail the refresh: the prior snapshot survives.
	src.fetchedAt = src.fetchedAt.Add(-2 * hmmRosterTTL)
	fetch.err = errors.New("sidecar down")

	stale, err := src.HMMDeployedModels(context.Background())
	require.NoError(t, err)
	require.Len(t, stale, 1)
	assert.Equal(t, "gpt-5.6-sol", stale[0].Model)

	callsAfterFailure := fetch.calls.Load()
	// Within the backoff window the next reader serves stale without re-hitting
	// the failing sidecar.
	_, err = src.HMMDeployedModels(context.Background())
	require.NoError(t, err)
	assert.Equal(t, callsAfterFailure, fetch.calls.Load(), "backoff must suppress re-fetch during an outage")
}

func TestHMMRosterSource_ErrorsWhenNoSnapshot(t *testing.T) {
	fetch := &stubRosterFetcher{err: errors.New("sidecar down")}
	src := newHMMRosterSource(fetch, 0)

	_, err := src.HMMDeployedModels(context.Background())
	require.Error(t, err)
}

func TestHMMRosterSource_CallerCancelDoesNotAbortSharedFetch(t *testing.T) {
	// One caller cancels mid-fetch; the concurrent live-context caller must
	// still get the roster because the shared fetch runs under a detached context.
	fetch := &stubRosterFetcher{
		ids:     []string{"openai/gpt-5.6-sol"},
		gate:    make(chan struct{}),
		entered: make(chan struct{}, 2),
	}
	src := newHMMRosterSource(fetch, 0)

	cancelCtx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var liveEntries []cluster.DeployedEntry
	var liveErr error

	wg.Add(2)
	// Caller A: will be cancelled mid-fetch.
	go func() {
		defer wg.Done()
		_, _ = src.HMMDeployedModels(cancelCtx)
	}()
	// Caller B: live context, must receive the roster.
	go func() {
		defer wg.Done()
		liveEntries, liveErr = src.HMMDeployedModels(context.Background())
	}()

	// Wait until the (single) shared fetch is in flight, cancel caller A, then
	// release the fetch. Caller B must still succeed.
	<-fetch.entered
	cancel()
	close(fetch.gate)
	wg.Wait()

	require.NoError(t, liveErr, "a live-context caller must not inherit another caller's cancellation")
	require.Len(t, liveEntries, 1)
	assert.Equal(t, "gpt-5.6-sol", liveEntries[0].Model)
}

func TestHMMRosterSource_ColdStartStampedeCollapsesToOneFetch(t *testing.T) {
	// Cold cache + concurrent callers: singleflight collapses to one fetch;
	// every caller must succeed — no 503 because a sibling's fetch "won".
	const callers = 8
	fetch := &stubRosterFetcher{
		ids:     []string{"openai/gpt-5.6-sol"},
		gate:    make(chan struct{}),
		entered: make(chan struct{}, callers),
	}
	src := newHMMRosterSource(fetch, 0)

	var wg sync.WaitGroup
	results := make([][]string, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			entries, err := src.HMMDeployedModels(context.Background())
			errs[idx] = err
			for _, e := range entries {
				results[idx] = append(results[idx], e.Model)
			}
		}(i)
	}

	// Let at least one fetch enter, then release the gate so the collapsed
	// call returns to every waiter.
	<-fetch.entered
	close(fetch.gate)
	wg.Wait()

	for i := range callers {
		require.NoErrorf(t, errs[i], "caller %d must not 503 on a successful cold-start fetch", i)
		assert.Equalf(t, []string{"gpt-5.6-sol"}, results[i], "caller %d got the wrong roster", i)
	}
	assert.LessOrEqual(t, fetch.calls.Load(), int64(2), "stampede must collapse to ~one sidecar fetch")
}
