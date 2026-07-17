package translate

import "github.com/tidwall/gjson"

func anthropicUsageValues(usage gjson.Result) UsageValues {
	return UsageValues{
		InputTokens:              usageResultInt(usage.Get("input_tokens")),
		OutputTokens:             usageResultInt(usage.Get("output_tokens")),
		CacheCreationInputTokens: usageResultInt(usage.Get("cache_creation_input_tokens")),
		CacheReadInputTokens:     usageResultInt(usage.Get("cache_read_input_tokens")),
	}
}

func openAIUsageValues(usage gjson.Result) UsageValues {
	if usage.Get("prompt_tokens").Exists() {
		return UsageValues{
			InputTokens:              usageResultInt(usage.Get("prompt_tokens")),
			OutputTokens:             usageResultInt(usage.Get("completion_tokens")),
			CacheCreationInputTokens: usageResultInt(usage.Get("prompt_tokens_details.cache_creation_tokens")),
			CacheReadInputTokens:     usageResultInt(usage.Get("prompt_tokens_details.cached_tokens")),
			ReasoningTokens:          usageResultInt(usage.Get("completion_tokens_details.reasoning_tokens")),
			AudioInputTokens:         usageResultInt(usage.Get("prompt_tokens_details.audio_tokens")),
			AudioOutputTokens:        usageResultInt(usage.Get("completion_tokens_details.audio_tokens")),
			AcceptedPredictionTokens: usageResultInt(usage.Get("completion_tokens_details.accepted_prediction_tokens")),
			RejectedPredictionTokens: usageResultInt(usage.Get("completion_tokens_details.rejected_prediction_tokens")),
		}
	}
	return UsageValues{
		InputTokens:              usageResultInt(usage.Get("input_tokens")),
		OutputTokens:             usageResultInt(usage.Get("output_tokens")),
		CacheReadInputTokens:     usageResultInt(usage.Get("input_tokens_details.cached_tokens")),
		ReasoningTokens:          usageResultInt(usage.Get("output_tokens_details.reasoning_tokens")),
		AudioInputTokens:         usageResultInt(usage.Get("input_tokens_details.audio_tokens")),
		AudioOutputTokens:        usageResultInt(usage.Get("output_tokens_details.audio_tokens")),
		AcceptedPredictionTokens: usageResultInt(usage.Get("output_tokens_details.accepted_prediction_tokens")),
		RejectedPredictionTokens: usageResultInt(usage.Get("output_tokens_details.rejected_prediction_tokens")),
	}
}

func geminiUsageValues(usage gjson.Result) UsageValues {
	return UsageValues{
		InputTokens:          usageResultInt(usage.Get("promptTokenCount")),
		OutputTokens:         usageResultInt(usage.Get("candidatesTokenCount")),
		CacheReadInputTokens: usageResultInt(usage.Get("cachedContentTokenCount")),
		ReasoningTokens:      usageResultInt(usage.Get("thoughtsTokenCount")),
	}
}

func usageResultInt(result gjson.Result) *int {
	if !result.Exists() {
		return nil
	}
	value := int(result.Int())
	return &value
}

func usageIntValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}
