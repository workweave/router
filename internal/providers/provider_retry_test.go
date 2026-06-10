package providers_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/providers"
)

// TestIsRetryable_UpstreamIdleTimeout pins the classification of the SSE
// idle-watchdog sentinel: a mid-stream zero-progress stall is the upstream's
// fault and must be retryable so dispatchWithFallback can rescue the turn on
// the next attempt. Both the bare sentinel and a wrapped chain must classify,
// since adapters may annotate the error on the way out.
func TestIsRetryable_UpstreamIdleTimeout(t *testing.T) {
	assert.True(t, providers.IsRetryable(providers.ErrUpstreamIdleTimeout))
	assert.True(t, providers.IsRetryable(fmt.Errorf("stream upstream response: %w", providers.ErrUpstreamIdleTimeout)))
}

func TestIsRetryable_ResponseHeaderTimeout(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer close(release)

	client := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 10 * time.Millisecond}}
	_, err := client.Post(upstream.URL, "application/json", strings.NewReader(`{}`))

	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "timeout awaiting response headers")
	assert.True(t, providers.IsRetryable(err))
}

func TestIsRetryable_RequestDeadlineExceeded(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL, strings.NewReader(`{}`))
	require.NoError(t, err)
	_, err = http.DefaultClient.Do(req)

	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.False(t, providers.IsRetryable(err))
}
