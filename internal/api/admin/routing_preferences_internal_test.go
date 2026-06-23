package admin

import (
	"testing"

	"workweave/router/internal/auth"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeRoutingWeight(t *testing.T) {
	t.Run("normalizes to a quality weight in [0, 1]", func(t *testing.T) {
		q, ok := normalizeRoutingWeight(70, 30) // total 100
		assert.True(t, ok)
		assert.InDelta(t, 0.7, q, 0.001)
	})

	t.Run("normalizes when the raw values don't sum to 100", func(t *testing.T) {
		q, ok := normalizeRoutingWeight(60, 20) // total 80
		assert.True(t, ok)
		assert.InDelta(t, 0.75, q, 0.001)
	})

	t.Run("rejects all-zero", func(t *testing.T) {
		_, ok := normalizeRoutingWeight(0, 0)
		assert.False(t, ok)
	})

	t.Run("rejects negative", func(t *testing.T) {
		_, ok := normalizeRoutingWeight(-1, 50)
		assert.False(t, ok)
	})
}

func TestRoutingPreferencesFor(t *testing.T) {
	t.Run("nil weight renders the neutral default", func(t *testing.T) {
		got := routingPreferencesFor(&auth.Installation{})
		assert.True(t, got.IsDefault)
		assert.Equal(t, defaultQualityPct, got.Quality)
		assert.Equal(t, defaultPricePct, got.Price)
	})

	t.Run("set weight renders as percentages with price as the remainder", func(t *testing.T) {
		quality := 0.8
		got := routingPreferencesFor(&auth.Installation{RoutingQualityWeight: &quality})
		assert.False(t, got.IsDefault)
		assert.InDelta(t, 80.0, got.Quality, 0.001)
		assert.InDelta(t, 20.0, got.Price, 0.001)
	})
}
