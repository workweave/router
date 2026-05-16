package proxy

import (
	"testing"

	"workweave/router/internal/providers"

	"github.com/stretchr/testify/assert"
)

func TestInferProviderForModel(t *testing.T) {
	tests := []struct {
		model    string
		expected string
	}{
		{"claude-opus-4-7", providers.ProviderAnthropic},
		{"claude-sonnet-4-6", providers.ProviderAnthropic},
		{"claude-haiku-4-5", providers.ProviderAnthropic},
		{"gpt-5", providers.ProviderOpenAI},
		{"gpt-4o", providers.ProviderOpenAI},
		{"o1", providers.ProviderOpenAI},
		{"o3", providers.ProviderOpenAI},
		{"o3-mini", providers.ProviderOpenAI},
		{"o4-mini", providers.ProviderOpenAI},
		{"gemini-2.5-pro", providers.ProviderGoogle},
		{"gemini-3.1-flash-lite-preview", providers.ProviderGoogle},
		{"deepseek/deepseek-v4-pro", providers.ProviderOpenRouter},
		{"qwen/qwen3-235b-a22b-2507", providers.ProviderOpenRouter},
		{"mistral/mistral-small-2603", providers.ProviderOpenRouter},
		// Unrecognized model falls back to Anthropic.
		{"unknown-model-xyz", providers.ProviderAnthropic},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := inferProviderForModel(tt.model)
			assert.Equal(t, tt.expected, got)
		})
	}
}
