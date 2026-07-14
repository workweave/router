package proxy

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/router"
)

func TestClipFeedbackTrainingDelta_ClipsAt256KiBBoundary(t *testing.T) {
	oversized := strings.Repeat("x", maxFeedbackDeltaChars+1)
	clipped, truncated := clipFeedbackTrainingDelta([]router.ConversationMessage{{
		Role: "user",
		ToolResults: []router.ConversationToolResult{{
			ToolUseID: "toolu_1",
			Text:      oversized,
		}},
	}})
	require.Len(t, clipped, 1)
	require.Len(t, clipped[0].ToolResults, 1)
	assert.Equal(t, maxFeedbackDeltaChars, len(clipped[0].ToolResults[0].Text))
	assert.True(t, truncated)

	exact := strings.Repeat("y", maxFeedbackDeltaChars)
	clipped, truncated = clipFeedbackTrainingDelta([]router.ConversationMessage{{
		Role: "user",
		ToolResults: []router.ConversationToolResult{{
			ToolUseID: "toolu_2",
			Text:      exact,
		}},
	}})
	require.Len(t, clipped, 1)
	assert.Equal(t, maxFeedbackDeltaChars, len(clipped[0].ToolResults[0].Text))
	assert.False(t, truncated)
}
