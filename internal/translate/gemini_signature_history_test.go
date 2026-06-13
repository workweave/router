package translate_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/translate"
)

// "c2ln" is base64url(RawURLEncoding) of "sig" — the embedded-signature form
// embedSignatureInID produces in a tool-call id.
const signedToolID = "toolu_1__thought__c2ln"

func TestHasUnsignedToolCallHistory_AnthropicUnsigned(t *testing.T) {
	env, err := translate.ParseAnthropic([]byte(`{
		"model":"x","messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}
		]}`))
	require.NoError(t, err)
	assert.True(t, env.HasUnsignedToolCallHistory(), "a tool_use with no embedded signature is unsigned (foreign history)")
}

func TestHasUnsignedToolCallHistory_AnthropicSigned(t *testing.T) {
	env, err := translate.ParseAnthropic([]byte(`{
		"model":"x","messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"` + signedToolID + `","name":"Bash","input":{}}]}
		]}`))
	require.NoError(t, err)
	assert.False(t, env.HasUnsignedToolCallHistory(), "an embedded thoughtSignature makes the tool call signed (native Gemini continuation)")
}

func TestHasUnsignedToolCallHistory_NoToolCalls(t *testing.T) {
	env, err := translate.ParseAnthropic([]byte(`{"model":"x","messages":[{"role":"assistant","content":[{"type":"text","text":"hi"}]}]}`))
	require.NoError(t, err)
	assert.False(t, env.HasUnsignedToolCallHistory(), "text-only history has no tool calls to validate")
}

func TestHasUnsignedToolCallHistory_OpenAIUnsigned(t *testing.T) {
	env, err := translate.ParseOpenAI([]byte(`{
		"model":"x","messages":[
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{}"}}]}
		]}`))
	require.NoError(t, err)
	assert.True(t, env.HasUnsignedToolCallHistory())
}

func TestHasUnsignedToolCallHistory_OpenAISigned(t *testing.T) {
	env, err := translate.ParseOpenAI([]byte(`{
		"model":"x","messages":[
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_1__thought__c2ln","type":"function","function":{"name":"Bash","arguments":"{}"}}]}
		]}`))
	require.NoError(t, err)
	assert.False(t, env.HasUnsignedToolCallHistory())
}
