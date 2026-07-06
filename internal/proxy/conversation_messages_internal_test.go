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
		"messages":[{
			"role":"assistant",
			"content":[{"type":"tool_use","name":"Read","input":{"file_path":"README.md"}}]
		}]
	}`))
	require.NoError(t, err)

	messages := conversationMessagesForRouting(env)
	require.Len(t, messages, 1)
	assert.Equal(t, "assistant", messages[0].Role)
	require.Len(t, messages[0].ToolCalls, 1)
	assert.Equal(t, "Read", messages[0].ToolCalls[0].Name)
	assert.Equal(t, []string{"file_path"}, messages[0].ToolCalls[0].InputKeys)
}
