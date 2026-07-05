package apm_test

import (
	"context"
	"testing"
	"time"

	"workweave/router/internal/observability/apm"

	"github.com/stretchr/testify/assert"
)

// Init is guarded by a sync.Once, so only one of these tests can ever
// observe the "never initialized" no-op path in a given process. We rely on
// WV_APM_OTLP_ENDPOINT being unset in the test environment and run the
// no-op-path assertions first in this file; go test executes tests within a
// file in source order, and package-level state is process-wide.

func TestInit_NoEndpointIsNoOp(t *testing.T) {
	t.Setenv("WV_APM_OTLP_ENDPOINT", "")

	assert.NotPanics(t, func() {
		apm.Init()
	}, "Init must not panic when WV_APM_OTLP_ENDPOINT is unset")
}

func TestShutdownWithContext_SafeWhenInitNeverCalled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	assert.NotPanics(t, func() {
		apm.ShutdownWithContext(ctx)
	}, "ShutdownWithContext must be safe to call even if the SDK was never wired up")
}

func TestShutdown_IdempotentAcrossRepeatedCalls(t *testing.T) {
	assert.NotPanics(t, func() {
		apm.Shutdown()
		apm.Shutdown()
		apm.Shutdown()
	}, "Shutdown must be safe to call repeatedly")
}

func TestShutdownWithContext_IdempotentAcrossRepeatedCalls(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	assert.NotPanics(t, func() {
		apm.ShutdownWithContext(ctx)
		apm.ShutdownWithContext(ctx)
	}, "ShutdownWithContext must be safe to call repeatedly")
}

func TestMiddleware_ReturnsNonNilHandler(t *testing.T) {
	// Middleware wraps otelgin regardless of whether Init has run; it should
	// always return a usable gin.HandlerFunc, never nil, since callers wire
	// it into the router unconditionally at startup.
	handler := apm.Middleware()

	assert.NotNil(t, handler)
}
