package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/translate"
)

func TestExcludeGemini3xOnUnsignedHistory_ExcludesGeminiKeepsOthers(t *testing.T) {
	available := map[string]struct{}{
		"gemini-3.1-pro-preview":        {},
		"gemini-3.1-flash-lite-preview": {},
		"claude-opus-4-8":               {},
		"deepseek/deepseek-v4-pro":      {},
	}
	env, err := translate.ParseAnthropic([]byte(`{"model":"x","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}]}]}`))
	require.NoError(t, err)

	out, added := excludeGemini3xOnUnsignedHistory(env, map[string]struct{}{}, available)
	assert.ElementsMatch(t, []string{"gemini-3.1-flash-lite-preview", "gemini-3.1-pro-preview"}, added)
	_, geminiExcluded := out["gemini-3.1-pro-preview"]
	assert.True(t, geminiExcluded, "gemini-3.x is excluded when history carries unsigned tool calls")
	_, opusExcluded := out["claude-opus-4-8"]
	assert.False(t, opusExcluded, "non-gemini models stay eligible")
}

func TestExcludeGemini3xOnUnsignedHistory_SignedHistoryIsNoop(t *testing.T) {
	available := map[string]struct{}{"gemini-3.1-pro-preview": {}, "claude-opus-4-8": {}}
	env, err := translate.ParseAnthropic([]byte(`{"model":"x","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1__thought__c2ln","name":"Bash","input":{}}]}]}`))
	require.NoError(t, err)

	out, added := excludeGemini3xOnUnsignedHistory(env, map[string]struct{}{}, available)
	assert.Nil(t, added, "signed (native Gemini) history must not exclude Gemini")
	assert.Empty(t, out)
}

func TestGemini3xRequiresSignedHistory(t *testing.T) {
	assert.True(t, gemini3xRequiresSignedHistory("gemini-3.1-pro-preview"))
	assert.True(t, gemini3xRequiresSignedHistory("gemini-3.5-flash"))
	assert.False(t, gemini3xRequiresSignedHistory("gemini-2.5-flash"))
	assert.False(t, gemini3xRequiresSignedHistory("claude-opus-4-8"))
}
