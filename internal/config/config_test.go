package config_test

import (
	"testing"

	"workweave/router/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTranslationCompatibilityMode(t *testing.T) {
	t.Setenv("ROUTER_TRANSLATION_COMPATIBILITY_MODE", "")
	mode, err := config.TranslationCompatibilityMode()
	require.NoError(t, err)
	assert.Equal(t, "shadow", mode)

	t.Setenv("ROUTER_TRANSLATION_COMPATIBILITY_MODE", "EnFoRcE")
	mode, err = config.TranslationCompatibilityMode()
	require.NoError(t, err)
	assert.Equal(t, "enforce", mode)

	t.Setenv("ROUTER_TRANSLATION_COMPATIBILITY_MODE", "invalid")
	_, err = config.TranslationCompatibilityMode()
	require.Error(t, err)
}
