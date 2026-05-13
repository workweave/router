package capability_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/router/capability"
)

func TestTierFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		model string
		want  capability.Tier
	}{
		{"claude-haiku-4-5", capability.TierLow},
		{"gemini-3.1-flash-lite-preview", capability.TierLow},
		{"claude-sonnet-4-5", capability.TierMid},
		{"qwen/qwen3-coder", capability.TierMid},
		{"claude-opus-4-7", capability.TierHigh},
		{"gpt-5.5", capability.TierHigh},
		{"moonshotai/kimi-k2.5", capability.TierHigh},
		{"deepseek/deepseek-v4-pro", capability.TierHigh},
		{"fictional-foo-1.0", capability.TierUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, capability.TierFor(tc.model))
		})
	}
}

// TestTierOrdering pins the integer ordering the planner relies on:
// Unknown < Low < Mid < High. If anyone reorders the iota, this fails.
func TestTierOrdering(t *testing.T) {
	t.Parallel()
	assert.Less(t, capability.TierUnknown, capability.TierLow)
	assert.Less(t, capability.TierLow, capability.TierMid)
	assert.Less(t, capability.TierMid, capability.TierHigh)
}

func TestValidate(t *testing.T) {
	t.Parallel()

	t.Run("all deployed models have tiers", func(t *testing.T) {
		t.Parallel()
		// Mirror the v0.27 deployed_models set — the production-bound
		// bundle. Adding a model to that registry without adding it to
		// the tier table should be caught here too.
		deployed := []string{
			"claude-haiku-4-5",
			"claude-sonnet-4-5",
			"claude-opus-4-7",
			"gemini-3.1-flash-lite-preview",
			"gemini-3.1-pro-preview",
			"gemini-3-flash-preview",
			"gpt-4.1",
			"gpt-5.4-mini",
			"gpt-5.5",
			"gemini-2.5-flash",
			"qwen/qwen3-235b-a22b-2507",
			"qwen/qwen3-30b-a3b-instruct-2507",
			"qwen/qwen3-coder-next",
			"qwen/qwen3-next-80b-a3b-instruct",
			"qwen/qwen3.5-flash-02-23",
			"qwen/qwen3-coder",
			"deepseek/deepseek-v4-flash",
			"deepseek/deepseek-v4-pro",
			"moonshotai/kimi-k2.5",
			"mistralai/mistral-small-2603",
		}
		require.NoError(t, capability.Validate(deployed))
	})

	t.Run("missing model surfaces in error", func(t *testing.T) {
		t.Parallel()
		err := capability.Validate([]string{"claude-opus-4-7", "fictional-foo-1.0"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "fictional-foo-1.0")
		assert.NotContains(t, err.Error(), "claude-opus-4-7")
	})

	t.Run("empty deployed list is valid", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, capability.Validate(nil))
	})
}
