package translate

import "strings"

// openRouterProviderHint returns the OpenRouter `provider` field for model slugs
// that need pinning to caching-capable backends. Without a hint, OpenRouter
// load-balances by price and picks hosts without prefix caching, which breaks
// agentic workloads re-sending large transcripts each turn.
func openRouterProviderHint(model string) map[string]any {
	switch {
	case strings.HasPrefix(model, "deepseek/"):
		return map[string]any{
			"order":           []string{"deepseek"},
			"allow_fallbacks": false,
		}
	case strings.HasPrefix(model, "moonshotai/"):
		return map[string]any{
			"order":           []string{"moonshotai"},
			"allow_fallbacks": false,
		}
	case strings.HasPrefix(model, "qwen/"), strings.HasPrefix(model, "google/"):
		return map[string]any{"sort": "throughput"}
	}
	return nil
}

// openRouterReasoningHint returns the OpenRouter `reasoning` field to disable
// reasoning on models that burn the entire max_tokens budget on hidden thinking.
// Native DeepSeek serving defaults to reasoning-on and ignores effort=minimal.
//
// Extended to moonshotai/* (Kimi K2.x) and xiaomi/* (MiMo) because on
// tool-calling turns these models otherwise emit native tool-call tokens
// (<|tool_call_begin|>, Hermes <tool_call> XML) inside an unbounded reasoning
// segment that OpenRouter's tool-call parser misses, leaving structured
// tool_calls empty and generation running to the output cap.
// (hermes-agent #24534, vllm #39056).
func openRouterReasoningHint(model string) map[string]any {
	switch {
	case strings.HasPrefix(model, "deepseek/"),
		strings.HasPrefix(model, "moonshotai/"),
		strings.HasPrefix(model, "xiaomi/"):
		return map[string]any{"enabled": false}
	}
	return nil
}

// openRouterForcesToolTemperatureZero reports whether tool-calling turns for
// this model should default to temperature 0. Reserved for models whose
// sampling jitter measurably degrades tool-arg fidelity (whitespace, unicode)
// — DeepSeek's reasoning-disabled tool turns are the documented case.
// Callers still skip the override when the client set temperature explicitly.
func openRouterForcesToolTemperatureZero(model string) bool {
	return strings.HasPrefix(model, "deepseek/")
}

// isQwen3Family reports whether the model id belongs to the qwen3.x family.
// Qwen3 variants (instruct, coder, qwen3-next, qwen3.6-*) are documented to
// drift into tool-call / thinking loops without a non-zero presence_penalty;
// the Qwen3 model card recommends presence_penalty=1.5 to suppress this.
func isQwen3Family(model string) bool {
	return strings.HasPrefix(model, "qwen/qwen3")
}

// qwen3PresencePenalty is the recommended Qwen3 presence_penalty value from
// the official model card; applied only when the client has not set
// presence_penalty itself.
const qwen3PresencePenalty = 1.5
