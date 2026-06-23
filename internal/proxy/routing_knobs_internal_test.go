package proxy

import (
	"context"
	"testing"

	"workweave/router/internal/router"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRoutingKnobsForRequest(t *testing.T) {
	ptr := func(f float64) *float64 { return &f }

	t.Run("nil when neither header nor installation knobs present", func(t *testing.T) {
		assert.Nil(t, routingKnobsForRequest(context.Background()))
	})

	t.Run("installation knobs apply when no header override", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), InstallationRoutingKnobsContextKey{}, &router.Overrides{
			Alpha: ptr(0.6),
		})
		got := routingKnobsForRequest(ctx)
		require.NotNil(t, got)
		require.NotNil(t, got.Alpha)
		assert.Equal(t, 0.6, *got.Alpha)
	})

	t.Run("header override wins over installation knobs", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), InstallationRoutingKnobsContextKey{}, &router.Overrides{
			Alpha: ptr(0.6),
		})
		ctx = router.WithRoutingKnobs(ctx, &router.Overrides{Alpha: ptr(0.95)})
		got := routingKnobsForRequest(ctx)
		require.NotNil(t, got)
		require.NotNil(t, got.Alpha)
		assert.Equal(t, 0.95, *got.Alpha)
	})
}
