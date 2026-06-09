package postgres

import (
	"testing"

	"workweave/router/internal/sqlc"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToAuthInstallationRoutingWeights(t *testing.T) {
	quality := 0.7
	speed := 0.2

	t.Run("set weights map to pointers", func(t *testing.T) {
		inst := toAuthInstallation(sqlc.RouterModelRouterInstallation{
			ID:                   uuid.New(),
			ExternalID:           "org-1",
			RoutingQualityWeight: &quality,
			RoutingSpeedWeight:   &speed,
		})
		require.NotNil(t, inst.RoutingQualityWeight)
		require.NotNil(t, inst.RoutingSpeedWeight)
		assert.Equal(t, 0.7, *inst.RoutingQualityWeight)
		assert.Equal(t, 0.2, *inst.RoutingSpeedWeight)
	})

	t.Run("null weights map to nil", func(t *testing.T) {
		inst := toAuthInstallation(sqlc.RouterModelRouterInstallation{
			ID:         uuid.New(),
			ExternalID: "org-2",
		})
		assert.Nil(t, inst.RoutingQualityWeight)
		assert.Nil(t, inst.RoutingSpeedWeight)
	})
}
