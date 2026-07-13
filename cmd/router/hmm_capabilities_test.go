package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/router/policy"
)

type policyCapabilityClientFunc func(context.Context) (policy.Capabilities, error)

func (f policyCapabilityClientFunc) Capabilities(ctx context.Context) (policy.Capabilities, error) {
	return f(ctx)
}

func TestRetryPolicyCapabilitiesUntilAvailableAppliesRecoveredCapabilities(t *testing.T) {
	attempts := 0
	want := policy.Capabilities{SchemaVersion: policy.SchemaVersionV1, SupportsShadow: true}
	client := policyCapabilityClientFunc(func(context.Context) (policy.Capabilities, error) {
		attempts++
		if attempts < 3 {
			return policy.Capabilities{}, errors.New("sidecar starting")
		}
		return want, nil
	})
	var applied policy.Capabilities

	err := retryPolicyCapabilitiesUntilAvailable(
		context.Background(), client, time.Second, time.Millisecond,
		func(capabilities policy.Capabilities) { applied = capabilities },
	)

	require.NoError(t, err)
	assert.Equal(t, 3, attempts)
	assert.Equal(t, want, applied)
}

func TestRetryPolicyCapabilitiesUntilAvailableStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	applied := false

	err := retryPolicyCapabilitiesUntilAvailable(
		ctx,
		policyCapabilityClientFunc(func(context.Context) (policy.Capabilities, error) {
			return policy.Capabilities{}, errors.New("sidecar unavailable")
		}),
		time.Second,
		time.Hour,
		func(policy.Capabilities) { applied = true },
	)

	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, applied)
}
