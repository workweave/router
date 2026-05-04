package router_test

import (
	"testing"

	"workweave/router/internal/router"

	"github.com/stretchr/testify/assert"
)

func TestLookup_UnknownModel(t *testing.T) {
	spec := router.Lookup("unknown-model-99")
	assert.False(t, spec.Supports(router.CapAdaptiveThinking))
	assert.False(t, spec.Supports(router.CapExtendedThinking))
	assert.False(t, spec.Supports(router.CapReasoning))
}

func TestLookup_DateSuffixNormalization(t *testing.T) {
	tests := []struct {
		name          string
		model         string
		wantAdaptive  bool
		wantExtended  bool
		wantReasoning bool
	}{
		{"anthropic haiku dated", "claude-haiku-4-5-20251001", false, true, false},
		{"anthropic opus dated", "claude-opus-4-7-20260301", true, true, false},
		{"openai dated", "gpt-4o-2024-08-06", false, false, false},
		{"google flash registered", "gemini-2.5-flash", false, false, false},
		{"google pro registered", "gemini-2.5-pro", false, false, false},
		{"unknown with date suffix", "mystery-model-20250101", false, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := router.Lookup(tc.model)
			assert.Equal(t, tc.wantAdaptive, spec.Supports(router.CapAdaptiveThinking))
			assert.Equal(t, tc.wantExtended, spec.Supports(router.CapExtendedThinking))
			assert.Equal(t, tc.wantReasoning, spec.Supports(router.CapReasoning))
		})
	}
}
