package postgres

import (
	"testing"

	"workweave/router/internal/sqlc"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToAuthInstallationRoutingQualityWeight(t *testing.T) {
	t.Run("set weight maps to pointer", func(t *testing.T) {
		quality := 0.7
		inst := toAuthInstallation(sqlc.RouterModelRouterInstallation{
			ID:                   uuid.New(),
			ExternalID:           "org-1",
			RoutingQualityWeight: &quality,
		})
		require.NotNil(t, inst.RoutingQualityWeight)
		assert.Equal(t, 0.7, *inst.RoutingQualityWeight)
	})

	t.Run("null weight maps to nil", func(t *testing.T) {
		inst := toAuthInstallation(sqlc.RouterModelRouterInstallation{
			ID:         uuid.New(),
			ExternalID: "org-2",
		})
		assert.Nil(t, inst.RoutingQualityWeight)
	})
}
