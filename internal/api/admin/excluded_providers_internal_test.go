package admin

import (
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/router/cluster"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeExcludedProviderModels struct {
	entries []cluster.DeployedEntry
}

func (f fakeExcludedProviderModels) DefaultDeployedModels() []cluster.DeployedEntry {
	return f.entries
}

func TestDeployedProvidersDTOIncludesRegisteredProviders(t *testing.T) {
	got := deployedProvidersDTO(fakeExcludedProviderModels{
		entries: []cluster.DeployedEntry{
			{Model: "deepseek/deepseek-v4-flash", Provider: providers.ProviderOpenRouter},
		},
	})

	require.Contains(t, got, providers.ProviderOpenRouter)
	assert.Contains(t, got, providers.ProviderTrustedRouter,
		"provider exclusions must allow trustedrouter even when no deployed-model row currently binds to it")
}
