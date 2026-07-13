package main

import (
	"context"
	"time"

	"workweave/router/internal/router/policy"
)

const hmmCapabilityRetryInterval = time.Second

type policyCapabilityClient interface {
	Capabilities(context.Context) (policy.Capabilities, error)
}

// retryPolicyCapabilitiesUntilAvailable keeps optional behavior conservative
// while a sidecar starts, then applies its declaration without blocking the
// core router's startup.
func retryPolicyCapabilitiesUntilAvailable(
	ctx context.Context,
	client policyCapabilityClient,
	requestTimeout time.Duration,
	retryInterval time.Duration,
	apply func(policy.Capabilities),
) error {
	for {
		requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		capabilities, err := client.Capabilities(requestCtx)
		cancel()
		if err == nil {
			apply(capabilities)
			return nil
		}

		timer := time.NewTimer(retryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
