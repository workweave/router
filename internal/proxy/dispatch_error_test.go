package proxy_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router/bandit"
	"workweave/router/internal/router/cluster"
	"workweave/router/internal/router/hmm"
	"workweave/router/internal/router/rl"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyDispatchError_UnknownErrorIsUnmatched(t *testing.T) {
	_, ok := proxy.ClassifyDispatchError(errors.New("boom"))
	assert.False(t, ok, "an error not matching any known sentinel must not be classified")
}

func TestClassifyDispatchError_ProviderNotConfigured(t *testing.T) {
	// This is the exact wrapping service.go's dispatch switch uses (fmt.Errorf("%w: %s", ErrProviderNotConfigured, name)).
	err := fmt.Errorf("%w: %s", proxy.ErrProviderNotConfigured, "some-provider")

	cls, ok := proxy.ClassifyDispatchError(err)

	require.True(t, ok, "ErrProviderNotConfigured must be classified")
	assert.Equal(t, proxy.DispatchErrorProviderNotConfigured, cls.Kind)
	assert.Equal(t, http.StatusBadGateway, cls.Status)
	assert.Equal(t, "Provider not configured.", cls.Message)
	assert.False(t, cls.RetryAfter)
	assert.False(t, cls.Kind.IsClientError(), "provider-not-configured is an upstream/routing problem, not a client-input one")
}

func TestClassifyDispatchError_UpstreamStatusErrorPreservesStatus(t *testing.T) {
	err := &providers.UpstreamStatusError{Status: http.StatusTooManyRequests}

	cls, ok := proxy.ClassifyDispatchError(err)

	require.True(t, ok)
	assert.Equal(t, proxy.DispatchErrorUpstreamStatus, cls.Kind)
	assert.Equal(t, http.StatusTooManyRequests, cls.Status)
}

func TestClassifyDispatchError_ClusterUnavailableRetriesAndLogsError(t *testing.T) {
	cls, ok := proxy.ClassifyDispatchError(cluster.ErrClusterUnavailable)

	require.True(t, ok)
	assert.Equal(t, http.StatusServiceUnavailable, cls.Status)
	assert.True(t, cls.RetryAfter)
	assert.Equal(t, "error", cls.LogLevel)
}

func TestClassifyDispatchError_NoEligibleProviderIsClientErrorAndWarns(t *testing.T) {
	cls, ok := proxy.ClassifyDispatchError(cluster.ErrNoEligibleProvider)

	require.True(t, ok)
	assert.Equal(t, http.StatusBadRequest, cls.Status)
	assert.True(t, cls.Kind.IsClientError())
	assert.Equal(t, "warn", cls.LogLevel)
	assert.False(t, cls.RetryAfter)
}

func TestClassifyDispatchError_BanditRLandHMMUnavailableRetry(t *testing.T) {
	for _, err := range []error{bandit.ErrBanditUnavailable, rl.ErrPolicyUnavailable, hmm.ErrHMMUnavailable} {
		cls, ok := proxy.ClassifyDispatchError(err)
		require.True(t, ok, "expected %v to be classified", err)
		assert.Equal(t, http.StatusServiceUnavailable, cls.Status)
		assert.True(t, cls.RetryAfter)
	}
}

func TestClassifyDispatchError_NotImplementedDoesNotLog(t *testing.T) {
	cls, ok := proxy.ClassifyDispatchError(providers.ErrNotImplemented)

	require.True(t, ok)
	assert.Equal(t, http.StatusNotImplemented, cls.Status)
	assert.Empty(t, cls.LogLevel)
}
