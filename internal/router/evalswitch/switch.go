// Package evalswitch lets a request-time context signal pick between a
// primary router and a fallback router on a single deployment.
package evalswitch

import (
	"context"

	"workweave/router/internal/router"
)

// ContextKey is the request-context key for the eval routing decision.
type ContextKey struct{}

// Decision controls which router the switch dispatches to. Zero value uses primary.
type Decision struct {
	UseFallback bool
}

// Router dispatches to primary or fallback based on a context Decision.
type Router struct {
	primary  router.Router
	fallback router.Router
}

// New panics if primary or fallback is nil (boot-time fail-fast — a
// misconfigured composition root should never reach the request path).
func New(primary, fallback router.Router) *Router {
	if primary == nil || fallback == nil {
		panic("evalswitch.New: primary and fallback must be non-nil")
	}
	return &Router{primary: primary, fallback: fallback}
}

func (r *Router) Route(ctx context.Context, req router.Request) (router.Decision, error) {
	if d, ok := ctx.Value(ContextKey{}).(Decision); ok && d.UseFallback {
		return r.fallback.Route(ctx, req)
	}
	return r.primary.Route(ctx, req)
}

var _ router.Router = (*Router)(nil)
