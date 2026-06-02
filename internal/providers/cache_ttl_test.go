package providers_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"workweave/router/internal/providers"
)

func TestCacheTTLFor(t *testing.T) {
	t.Parallel()

	// Anthropic sells a 1h extended cache; the pin TTL is sized to it.
	assert.Equal(t, time.Hour, providers.CacheTTLFor(providers.ProviderAnthropic),
		"anthropic should report the 1h extended-cache window")

	// The OSS/compat providers cache best-effort on a minutes-scale window.
	assert.Equal(t, 5*time.Minute, providers.CacheTTLFor(providers.ProviderFireworks),
		"fireworks should report the short best-effort window")

	// Unknown providers fall back to the conservative default.
	assert.Equal(t, providers.DefaultCacheTTL, providers.CacheTTLFor("nonexistent-provider"),
		"unknown provider should fall back to the default TTL")
}
