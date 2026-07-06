package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/translate"
)

func TestConversationMessagesForRoutingConvertsAtProxyBoundary(t *testing.T) {
	env, err := translate.ParseAnthropic([]byte(`{
		"model":"claude-opus-4-7",
		"messages":[
			{
				"role":"assistant",
				"content":[{"type":"tool_use","name":"Read","input":{"file_path":"README.md"}}]
			},
			{
				"role":"user",
				"content":[{"type":"tool_result","tool_use_id":"toolu_123","is_error":true,"content":"raw output"}]
			}
		]
	}`))
	require.NoError(t, err)

	messages := conversationMessagesForRouting(env)
	require.Len(t, messages, 2)
	assert.Equal(t, "assistant", messages[0].Role)
	require.Len(t, messages[0].ToolCalls, 1)
	assert.Equal(t, "Read", messages[0].ToolCalls[0].Name)
	assert.Equal(t, []string{"file_path"}, messages[0].ToolCalls[0].InputKeys)
	assert.Equal(t, "user", messages[1].Role)
	assert.Empty(t, messages[1].Text)
	require.Len(t, messages[1].ToolResults, 1)
	assert.Equal(t, "toolu_123", messages[1].ToolResults[0].ToolUseID)
	assert.True(t, messages[1].ToolResults[0].IsError)
}
