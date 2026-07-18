package usage_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"workweave/router/internal/proxy/usage"
)

func TestRawAnthropicUnifiedHeaders(t *testing.T) {
	t.Run("captures every unified-prefixed header, case-insensitively", func(t *testing.T) {
		h := http.Header{}
		h.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
		h.Set("anthropic-ratelimit-unified-overage-status", "rejected")
		h.Set("Content-Type", "application/json")
		h.Set("Request-Id", "req_123")

		got := usage.RawAnthropicUnifiedHeaders(h)
		assert.Equal(t, "allowed", got["anthropic-ratelimit-unified-status"])
		assert.Equal(t, "rejected", got["anthropic-ratelimit-unified-overage-status"])
		assert.Len(t, got, 2, "only unified-prefixed headers should be captured")
	})

	t.Run("no unified headers -> nil, not empty map", func(t *testing.T) {
		h := http.Header{}
		h.Set("Content-Type", "application/json")
		assert.Nil(t, usage.RawAnthropicUnifiedHeaders(h))
	})

	t.Run("empty header set -> nil", func(t *testing.T) {
		assert.Nil(t, usage.RawAnthropicUnifiedHeaders(http.Header{}))
	})
}
