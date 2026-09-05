package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"weave-os/router/internal/providers"
)

func TestUpstreamIDsForProvider_MiniMax(t *testing.T) {
	ids := upstreamIDsForProvider(providers.ProviderMiniMax)

	assert.Equal(t, "MiniMax-M3", ids["minimax/minimax-m3"])
	assert.Equal(t, "MiniMax-M2.7", ids["minimax/minimax-m2.7"])
}
