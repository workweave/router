package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubRosterFetcher struct {
	ids   []string
	err   error
	calls int
}

func (s *stubRosterFetcher) Roster(context.Context) ([]string, error) {
	s.calls++
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
	assert.Equal(t, 1, fetch.calls)
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
}

func TestHMMRosterSource_ErrorsWhenNoSnapshot(t *testing.T) {
	fetch := &stubRosterFetcher{err: errors.New("sidecar down")}
	src := newHMMRosterSource(fetch)

	_, err := src.HMMDeployedModels(context.Background())
	require.Error(t, err)
}
