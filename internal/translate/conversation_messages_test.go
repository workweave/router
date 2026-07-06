package translate_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/translate"
)

func TestConversationMessagesStripsClaudeInjectedBlocks(t *testing.T) {
	env, err := translate.ParseAnthropic([]byte(`{
		"model":"claude-opus-4-7",
		"messages":[{
			"role":"user",
			"content":[
				{"type":"text","text":"<system-reminder>internal reminder</system-reminder>"},
				{"type":"text","text":"<command-name>do-not-route-on-this</command-name>"},
				{"type":"text","text":"can you help me brainstorm a bit"}
			]
		}]
	}`))
	require.NoError(t, err)

	messages := env.ConversationMessages()
	require.Len(t, messages, 1)
	assert.Equal(t, "user", messages[0].Role)
	assert.Equal(t, "can you help me brainstorm a bit", messages[0].Text)
}

func TestConversationMessagesGeminiMissingRoleDefaultsToUser(t *testing.T) {
	env, err := translate.ParseGemini([]byte(`{
		"contents":[
			{"parts":[{"text":"can you help me brainstorm a bit"}]},
			{"role":"model","parts":[{"text":"sure"}]}
		]
	}`))
	require.NoError(t, err)

	messages := env.ConversationMessages()
	require.Len(t, messages, 2)
	assert.Equal(t, "user", messages[0].Role)
	assert.Equal(t, "can you help me brainstorm a bit", messages[0].Text)
	assert.Equal(t, "assistant", messages[1].Role)
}

func TestConversationMessagesPreservesAnthropicToolResultMarker(t *testing.T) {
	env, err := translate.ParseAnthropic([]byte(`{
		"model":"claude-opus-4-7",
		"messages":[{
			"role":"user",
			"content":[{
				"type":"tool_result",
				"tool_use_id":"toolu_123",
				"is_error":true,
				"content":"large raw tool output"
			}]
		}]
	}`))
	require.NoError(t, err)

	messages := env.ConversationMessages()
	require.Len(t, messages, 1)
	assert.Equal(t, "user", messages[0].Role)
	assert.Empty(t, messages[0].Text)
	require.Len(t, messages[0].ToolResults, 1)
	assert.Equal(t, "toolu_123", messages[0].ToolResults[0].ToolUseID)
	assert.True(t, messages[0].ToolResults[0].IsError)
}
