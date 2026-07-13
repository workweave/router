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

// retryPolicyCapabilitiesUntilAvailable polls the sidecar until Capabilities returns without error, then calls apply; it does not block the caller.
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
