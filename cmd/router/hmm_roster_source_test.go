package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubRosterFetcher struct {
	ids     []string
	err     error
	calls   atomic.Int64
	gate    chan struct{} // if non-nil, Roster blocks until closed (forces fetch overlap)
	entered chan struct{} // if non-nil, Roster signals once per entry before blocking
}

func (s *stubRosterFetcher) Roster(context.Context) ([]string, error) {
	s.calls.Add(1)
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
	src := newHMMRosterSource(fetch)

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

func TestHMMRosterSource_ServesStaleOnFetchFailure(t *testing.T) {
	fetch := &stubRosterFetcher{ids: []string{"openai/gpt-5.6-sol"}}
	src := newHMMRosterSource(fetch)

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
	src := newHMMRosterSource(fetch)

	_, err := src.HMMDeployedModels(context.Background())
	require.Error(t, err)
}

func TestHMMRosterSource_ColdStartStampedeCollapsesToOneFetch(t *testing.T) {
	// Cold cache + concurrent callers: singleflight collapses them onto one
	// sidecar fetch, and every caller gets the successfully-fetched roster
	// (no caller 503s because a sibling's fetch "won"). The gate forces the
	// callers to overlap before the fetch returns.
	const callers = 8
	fetch := &stubRosterFetcher{
		ids:     []string{"openai/gpt-5.6-sol"},
		gate:    make(chan struct{}),
		entered: make(chan struct{}, callers),
	}
	src := newHMMRosterSource(fetch)

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
