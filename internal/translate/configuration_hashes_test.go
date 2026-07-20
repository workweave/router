package translate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolConfigurationSHA256ChangesWithSchema(t *testing.T) {
	first, err := ParseAnthropic([]byte(`{
		"model":"claude-sonnet-4-5",
		"max_tokens":32,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]
	}`))
	require.NoError(t, err)
	second, err := ParseAnthropic([]byte(`{
		"model":"claude-sonnet-4-5",
		"max_tokens":32,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"path":{"type":"integer"}}}}]
	}`))
	require.NoError(t, err)

	assert.NotEmpty(t, first.ToolConfigurationSHA256())
	assert.NotEqual(t, first.ToolConfigurationSHA256(), second.ToolConfigurationSHA256())
}

func TestToolConfigurationSHA256NormalizesJSONKeyOrder(t *testing.T) {
	first, err := ParseAnthropic([]byte(`{
		"model":"claude-sonnet-4-5",
		"max_tokens":32,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]
	}`))
	require.NoError(t, err)
	second, err := ParseAnthropic([]byte(`{
		"model":"claude-sonnet-4-5",
		"max_tokens":32,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"input_schema":{"properties":{"path":{"type":"string"}},"type":"object"},"name":"Read"}]
	}`))
	require.NoError(t, err)

	assert.Equal(t, first.ToolConfigurationSHA256(), second.ToolConfigurationSHA256())
}

func TestReasoningConfigurationSHA256NormalizesReasoningIntent(t *testing.T) {
	first, err := ParseOpenAI([]byte(`{
		"model":"gpt-5",
		"messages":[{"role":"user","content":"hi"}],
		"reasoning_effort":"high"
	}`))
	require.NoError(t, err)
	second, err := ParseOpenAI([]byte(`{
		"model":"gpt-5",
		"messages":[{"role":"user","content":"hi"}],
		"reasoning":{"effort":"high"}
	}`))
	require.NoError(t, err)

	assert.Equal(t, first.ReasoningConfigurationSHA256(), second.ReasoningConfigurationSHA256())
}

func TestConfigurationHashesHandleNilEnvelope(t *testing.T) {
	var envelope *RequestEnvelope

	assert.NotEmpty(t, envelope.ToolConfigurationSHA256())
	assert.NotEmpty(t, envelope.ReasoningConfigurationSHA256())
}
