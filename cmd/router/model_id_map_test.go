package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseModelIDMap(t *testing.T) {
	t.Run("unset returns nil", func(t *testing.T) {
		got, invalid := parseModelIDMap("")
		assert.Nil(t, got)
		assert.Empty(t, invalid)
	})

	t.Run("parses comma separated pairs", func(t *testing.T) {
		got, invalid := parseModelIDMap("deepseek/deepseek-v4-flash=deepseek-v4-flash, moonshotai/kimi-k2.6 = kimi-k2.6")
		assert.Empty(t, invalid)
		assert.Equal(t, map[string]string{
			"deepseek/deepseek-v4-flash": "deepseek-v4-flash",
			"moonshotai/kimi-k2.6":       "kimi-k2.6",
		}, got)
	})

	t.Run("keeps valid pairs and reports invalid entries", func(t *testing.T) {
		got, invalid := parseModelIDMap("deepseek/deepseek-v4-pro=deepseek-v4-pro,missing_equals,=empty_source,z-ai/glm-5.2=")
		assert.Equal(t, map[string]string{
			"deepseek/deepseek-v4-pro": "deepseek-v4-pro",
		}, got)
		assert.Equal(t, []string{"missing_equals", "=empty_source", "z-ai/glm-5.2="}, invalid)
	})
}
