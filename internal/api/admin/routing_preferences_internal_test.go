package admin

import (
	"testing"

	"workweave/router/internal/auth"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeRoutingWeights(t *testing.T) {
	t.Run("normalizes to weights summing to <= 1", func(t *testing.T) {
		q, s, ok := normalizeRoutingWeights(70, 30, 50) // total 150
		assert.True(t, ok)
		assert.InDelta(t, 0.4667, q, 0.001)
		assert.InDelta(t, 0.2, s, 0.001)
		assert.LessOrEqual(t, q+s, 1.0)
	})

	t.Run("rejects all-zero", func(t *testing.T) {
		_, _, ok := normalizeRoutingWeights(0, 0, 0)
		assert.False(t, ok)
	})

	t.Run("rejects negative", func(t *testing.T) {
		_, _, ok := normalizeRoutingWeights(-1, 50, 50)
		assert.False(t, ok)
	})
}

func TestRoutingPreferencesFor(t *testing.T) {
	t.Run("nil weights render the neutral default", func(t *testing.T) {
		got := routingPreferencesFor(&auth.Installation{})
		assert.True(t, got.IsDefault)
		assert.Equal(t, defaultQualityPct, got.Quality)
		assert.Equal(t, defaultSpeedPct, got.Speed)
		assert.Equal(t, defaultPricePct, got.Price)
	})

	t.Run("set weights render as percentages with price as the remainder", func(t *testing.T) {
		quality := 0.5
		speed := 0.2
		got := routingPreferencesFor(&auth.Installation{
			RoutingQualityWeight: &quality,
			RoutingSpeedWeight:   &speed,
		})
		assert.False(t, got.IsDefault)
		assert.InDelta(t, 50.0, got.Quality, 0.001)
		assert.InDelta(t, 20.0, got.Speed, 0.001)
		assert.InDelta(t, 30.0, got.Price, 0.001)
	})

	t.Run("half-set row is treated as unset", func(t *testing.T) {
		quality := 0.8
		got := routingPreferencesFor(&auth.Installation{RoutingQualityWeight: &quality})
		assert.True(t, got.IsDefault)
	})
}
