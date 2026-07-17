package translate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGeminiUsageFromBytesPreservesAbsentCounters(t *testing.T) {
	usage := geminiUsageFromBytes([]byte(`{"usageMetadata":{"promptTokenCount":9}}`))
	require.NotNil(t, usage)
	require.NotNil(t, usage.values.InputTokens)
	assert.Equal(t, 9, *usage.values.InputTokens)
	assert.Nil(t, usage.values.OutputTokens)
}

func TestOpenAIUsageValuesPreservesDetailedCounters(t *testing.T) {
	usage := gjson.Parse(`{"prompt_tokens":9,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":3,"audio_tokens":4},"completion_tokens_details":{"reasoning_tokens":5,"audio_tokens":6,"accepted_prediction_tokens":7,"rejected_prediction_tokens":8}}`)
	values := openAIUsageValues(usage)

	require.NotNil(t, values.ReasoningTokens)
	require.NotNil(t, values.AudioInputTokens)
	require.NotNil(t, values.AudioOutputTokens)
	require.NotNil(t, values.AcceptedPredictionTokens)
	require.NotNil(t, values.RejectedPredictionTokens)
	assert.Equal(t, 5, *values.ReasoningTokens)
	assert.Equal(t, 4, *values.AudioInputTokens)
	assert.Equal(t, 6, *values.AudioOutputTokens)
	assert.Equal(t, 7, *values.AcceptedPredictionTokens)
	assert.Equal(t, 8, *values.RejectedPredictionTokens)
}
