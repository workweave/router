package router

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsHMMStrategy(t *testing.T) {
	assert.True(t, IsHMMStrategy(StrategyHMM))
	assert.True(t, IsHMMStrategy(StrategyHMMEmbedding))
	assert.False(t, IsHMMStrategy(StrategyCluster))
	assert.False(t, IsHMMStrategy(StrategyRL))
}
