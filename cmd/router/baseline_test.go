package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveDefaultBaselineModel(t *testing.T) {
	t.Run("unset returns default", func(t *testing.T) {
		orig, hadOrig := os.LookupEnv("ROUTER_DEFAULT_BASELINE_MODEL")
		require := func() {
			if hadOrig {
				os.Setenv("ROUTER_DEFAULT_BASELINE_MODEL", orig)
			} else {
				os.Unsetenv("ROUTER_DEFAULT_BASELINE_MODEL")
			}
		}
		os.Unsetenv("ROUTER_DEFAULT_BASELINE_MODEL")
		t.Cleanup(require)
		assert.Equal(t, "claude-sonnet-4-5", resolveDefaultBaselineModel())
	})

	t.Run("explicit empty disables substitution", func(t *testing.T) {
		t.Setenv("ROUTER_DEFAULT_BASELINE_MODEL", "")
		assert.Equal(t, "", resolveDefaultBaselineModel())
	})

	t.Run("explicit value wins", func(t *testing.T) {
		t.Setenv("ROUTER_DEFAULT_BASELINE_MODEL", "claude-opus-4-7")
		assert.Equal(t, "claude-opus-4-7", resolveDefaultBaselineModel())
	})

	t.Run("whitespace trimmed", func(t *testing.T) {
		t.Setenv("ROUTER_DEFAULT_BASELINE_MODEL", "  claude-haiku-4-5  ")
		assert.Equal(t, "claude-haiku-4-5", resolveDefaultBaselineModel())
	})
}
