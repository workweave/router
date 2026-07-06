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

func TestConversationMessagesPreservesOpenAIToolResultMarker(t *testing.T) {
	env, err := translate.ParseOpenAI([]byte(`{
		"model":"gpt-4o",
		"messages":[
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_123","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"README.md\"}"}}]},
			{"role":"tool","tool_call_id":"call_123","content":"large raw tool output"}
		]
	}`))
	require.NoError(t, err)

	messages := env.ConversationMessages()
	require.Len(t, messages, 2)
	assert.Equal(t, "assistant", messages[0].Role)
	require.Len(t, messages[0].ToolCalls, 1)
	assert.Equal(t, "Read", messages[0].ToolCalls[0].Name)
	assert.Equal(t, "user", messages[1].Role)
	assert.Empty(t, messages[1].Text)
	require.Len(t, messages[1].ToolResults, 1)
	assert.Equal(t, "call_123", messages[1].ToolResults[0].ToolUseID)
}

func TestConversationMessagesPreservesGeminiToolResultMarker(t *testing.T) {
	env, err := translate.ParseGemini([]byte(`{
		"contents":[
			{"role":"model","parts":[{"functionCall":{"name":"Bash","args":{"command":"pwd"}}}]},
			{"role":"user","parts":[{"functionResponse":{"name":"Bash","response":{"output":"large raw tool output"}}}]}
		]
	}`))
	require.NoError(t, err)

	messages := env.ConversationMessages()
	require.Len(t, messages, 2)
	assert.Equal(t, "assistant", messages[0].Role)
	require.Len(t, messages[0].ToolCalls, 1)
	assert.Equal(t, "Bash", messages[0].ToolCalls[0].Name)
	assert.Equal(t, "user", messages[1].Role)
	assert.Empty(t, messages[1].Text)
	require.Len(t, messages[1].ToolResults, 1)
	assert.Equal(t, "Bash", messages[1].ToolResults[0].ToolUseID)
}

func TestConversationMessagesDropsNamelessOpenAIToolCalls(t *testing.T) {
	env, err := translate.ParseOpenAI([]byte(`{
		"model":"gpt-4o",
		"messages":[{
			"role":"assistant",
			"content":null,
			"tool_calls":[
				{"id":"call_empty","type":"function","function":{"name":"","arguments":"{\"path\":\"a\"}"}},
				{"id":"call_read","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"README.md\"}"}}
			]
		}]
	}`))
	require.NoError(t, err)

	messages := env.ConversationMessages()
	require.Len(t, messages, 1)
	require.Len(t, messages[0].ToolCalls, 1)
	assert.Equal(t, "Read", messages[0].ToolCalls[0].Name)
}

func TestConversationMessagesDropsNamelessGeminiToolCalls(t *testing.T) {
	env, err := translate.ParseGemini([]byte(`{
		"contents":[{
			"role":"model",
			"parts":[
				{"functionCall":{"name":"","args":{"path":"a"}}},
				{"functionCall":{"name":"Read","args":{"file_path":"README.md"}}}
			]
		}]
	}`))
	require.NoError(t, err)

	messages := env.ConversationMessages()
	require.Len(t, messages, 1)
	require.Len(t, messages[0].ToolCalls, 1)
	assert.Equal(t, "Read", messages[0].ToolCalls[0].Name)
}

func TestAvailableToolNamesProviderNeutral(t *testing.T) {
	anthropicEnv, err := translate.ParseAnthropic([]byte(`{
		"model":"claude-opus-4-7",
		"tools":[
			{"name":"Read","input_schema":{"type":"object"}},
			{"name":"Grep","input_schema":{"type":"object"}},
			{"name":"Read","input_schema":{"type":"object"}}
		],
		"messages":[{"role":"user","content":"inspect"}]
	}`))
	require.NoError(t, err)
	assert.Equal(t, []string{"Grep", "Read"}, anthropicEnv.AvailableToolNames())

	openAIEnv, err := translate.ParseOpenAI([]byte(`{
		"model":"gpt-4o",
		"tools":[
			{"type":"function","function":{"name":"Bash","parameters":{"type":"object"}}},
			{"type":"function","function":{"name":"","parameters":{"type":"object"}}}
		],
		"messages":[{"role":"user","content":"inspect"}]
	}`))
	require.NoError(t, err)
	assert.Equal(t, []string{"Bash"}, openAIEnv.AvailableToolNames())
}
