package proxy

import (
	"testing"

	"workweave/router/internal/providers"

	"github.com/stretchr/testify/assert"
)

func TestResolveForceModel(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantID       string
		wantProvider string
		wantKnown    bool
	}{
		// Catalog matches: provider comes from the primary binding, even
		// when the model name doesn't follow the bare-prefix heuristic. These
		// resolve to a real catalog entry, so known is true.
		{
			name:         "catalog anthropic",
			input:        "claude-opus-4-7",
			wantID:       "claude-opus-4-7",
			wantProvider: providers.ProviderAnthropic,
			wantKnown:    true,
		},
		{
			name:         "catalog google",
			input:        "gemini-3.1-flash-lite-preview",
			wantID:       "gemini-3.1-flash-lite-preview",
			wantProvider: providers.ProviderGoogle,
			wantKnown:    true,
		},
		{
			name:         "catalog bedrock — slash form",
			input:        "qwen/qwen3-235b-a22b-2507",
			wantID:       "qwen/qwen3-235b-a22b-2507",
			wantProvider: providers.ProviderBedrock,
			wantKnown:    true,
		},
		{
			name:         "catalog bedrock — bare suffix match",
			input:        "qwen3-235b-a22b-2507",
			wantID:       "qwen/qwen3-235b-a22b-2507",
			wantProvider: providers.ProviderBedrock,
			wantKnown:    true,
		},
		{
			name:         "alias gpt",
			input:        "gpt",
			wantID:       "gpt-5.5",
			wantProvider: providers.ProviderOpenAI,
			wantKnown:    true,
		},
		{
			name:         "alias gpt hyphen minor version",
			input:        "gpt-5-5",
			wantID:       "gpt-5.5",
			wantProvider: providers.ProviderOpenAI,
			wantKnown:    true,
		},
		{
			name:         "alias claude",
			input:        "claude",
			wantID:       "claude-opus-4-8",
			wantProvider: providers.ProviderAnthropic,
			wantKnown:    true,
		},
		{
			name:         "alias opus",
			input:        "opus",
			wantID:       "claude-opus-4-8",
			wantProvider: providers.ProviderAnthropic,
			wantKnown:    true,
		},
		{
			name:         "alias opus dotted version",
			input:        "opus-4.8",
			wantID:       "claude-opus-4-8",
			wantProvider: providers.ProviderAnthropic,
			wantKnown:    true,
		},
		{
			name:         "alias mixed case and whitespace",
			input:        "  Gemini  ",
			wantID:       "gemini-3-pro-preview",
			wantProvider: providers.ProviderGoogle,
			wantKnown:    true,
		},
		{
			name:         "alias qwen",
			input:        "qwen",
			wantID:       "qwen/qwen3-coder",
			wantProvider: providers.ProviderFireworks,
			wantKnown:    true,
		},
		// Heuristic fallback: not in the catalog, so known is false. The
		// provider is a best-effort guess for logging only; the handler rejects
		// these rather than pinning a model with no known tier.
		{
			name:         "heuristic openai — gpt-6 not in catalog",
			input:        "gpt-6",
			wantID:       "gpt-6",
			wantProvider: providers.ProviderOpenAI,
			wantKnown:    false,
		},
		{
			name:         "heuristic openai — o3",
			input:        "o3",
			wantID:       "o3",
			wantProvider: providers.ProviderOpenAI,
			wantKnown:    false,
		},
		{
			name:         "heuristic openrouter — unknown slash model",
			input:        "mistral/mistral-small-2603",
			wantID:       "mistral/mistral-small-2603",
			wantProvider: providers.ProviderOpenRouter,
			wantKnown:    false,
		},
		{
			name:         "heuristic anthropic — unknown bareword",
			input:        "totally-not-a-model",
			wantID:       "totally-not-a-model",
			wantProvider: providers.ProviderAnthropic,
			wantKnown:    false,
		},
		// Truncated command (the bug this guard closes): "/force-model gpt-"
		// parses to "gpt-", which matches no catalog entry.
		{
			name:         "truncated gpt- is not known",
			input:        "gpt-",
			wantID:       "gpt-",
			wantProvider: providers.ProviderOpenAI,
			wantKnown:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotProvider, gotKnown := resolveForceModel(tt.input)
			assert.Equal(t, tt.wantID, gotID, "canonical id")
			assert.Equal(t, tt.wantProvider, gotProvider, "provider")
			assert.Equal(t, tt.wantKnown, gotKnown, "known")
		})
	}
}
