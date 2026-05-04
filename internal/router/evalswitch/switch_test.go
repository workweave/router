package evalswitch_test

import (
	"context"
	"errors"
	"testing"

	"workweave/router/internal/router"
	"workweave/router/internal/router/evalswitch"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubRouter struct {
	decision router.Decision
	err      error
	called   bool
}

func (s *stubRouter) Route(_ context.Context, _ router.Request) (router.Decision, error) {
	s.called = true
	if s.err != nil {
		return router.Decision{}, s.err
	}
	return s.decision, nil
}

func TestSwitch_DispatchesToPrimaryByDefault(t *testing.T) {
	primary := &stubRouter{decision: router.Decision{Provider: "anthropic", Model: "primary", Reason: "primary:test"}}
	fallback := &stubRouter{decision: router.Decision{Provider: "anthropic", Model: "fallback", Reason: "fallback:test"}}

	sw := evalswitch.New(primary, fallback)
	d, err := sw.Route(context.Background(), router.Request{})

	require.NoError(t, err)
	assert.Equal(t, "primary", d.Model)
	assert.True(t, primary.called)
	assert.False(t, fallback.called)
}

func TestSwitch_DispatchesToFallbackWhenContextSignals(t *testing.T) {
	primary := &stubRouter{decision: router.Decision{Provider: "anthropic", Model: "primary", Reason: "primary:test"}}
	fallback := &stubRouter{decision: router.Decision{Provider: "anthropic", Model: "fallback", Reason: "fallback:test"}}

	sw := evalswitch.New(primary, fallback)
	ctx := context.WithValue(context.Background(), evalswitch.ContextKey{}, evalswitch.Decision{UseFallback: true})
	d, err := sw.Route(ctx, router.Request{})

	require.NoError(t, err)
	assert.Equal(t, "fallback", d.Model)
	assert.False(t, primary.called)
	assert.True(t, fallback.called)
}

func TestSwitch_IgnoresContextValueWhenUseFallbackFalse(t *testing.T) {
	primary := &stubRouter{decision: router.Decision{Model: "primary"}}
	fallback := &stubRouter{decision: router.Decision{Model: "fallback"}}

	sw := evalswitch.New(primary, fallback)
	ctx := context.WithValue(context.Background(), evalswitch.ContextKey{}, evalswitch.Decision{UseFallback: false})
	d, err := sw.Route(ctx, router.Request{})

	require.NoError(t, err)
	assert.Equal(t, "primary", d.Model)
	assert.True(t, primary.called)
	assert.False(t, fallback.called)
}

func TestSwitch_PropagatesPrimaryError(t *testing.T) {
	wantErr := errors.New("primary boom")
	primary := &stubRouter{err: wantErr}
	fallback := &stubRouter{decision: router.Decision{Model: "fallback"}}

	sw := evalswitch.New(primary, fallback)
	_, err := sw.Route(context.Background(), router.Request{})

	assert.ErrorIs(t, err, wantErr)
	assert.False(t, fallback.called, "primary error must not silently fail over to fallback")
}

func TestSwitch_PropagatesFallbackError(t *testing.T) {
	wantErr := errors.New("fallback boom")
	primary := &stubRouter{decision: router.Decision{Model: "primary"}}
	fallback := &stubRouter{err: wantErr}

	sw := evalswitch.New(primary, fallback)
	ctx := context.WithValue(context.Background(), evalswitch.ContextKey{}, evalswitch.Decision{UseFallback: true})
	_, err := sw.Route(ctx, router.Request{})

	assert.ErrorIs(t, err, wantErr)
	assert.False(t, primary.called)
}

func TestSwitch_NewPanicsOnNilRouter(t *testing.T) {
	primary := &stubRouter{}
	assert.Panics(t, func() { evalswitch.New(nil, primary) })
	assert.Panics(t, func() { evalswitch.New(primary, nil) })
}
