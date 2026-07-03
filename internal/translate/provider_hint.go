package translate

import "strings"

// openRouterProviderHint pins model slugs to caching-capable backends.
// Without it, OpenRouter load-balances by price onto hosts without prefix
// caching, which breaks agentic workloads re-sending large transcripts.
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

// openRouterReasoningHint disables reasoning on models that burn the whole
// max_tokens budget on hidden thinking. Native DeepSeek ignores effort=minimal
// and defaults reasoning-on. moonshotai/* (Kimi) and xiaomi/* (MiMo) are
// included because on tool calls they emit native tool-call tokens inside an
// unbounded reasoning segment that OpenRouter's parser misses, leaving
// tool_calls empty (hermes-agent #24534, vllm #39056).
func openRouterReasoningHint(model string) map[string]any {
	switch {
	case strings.HasPrefix(model, "deepseek/"),
		strings.HasPrefix(model, "moonshotai/"),
		strings.HasPrefix(model, "xiaomi/"),
		model == "z-ai/glm-5.1":
		return map[string]any{"enabled": false}
	}
	return nil
}

// openRouterForcesToolTemperatureZero reports whether tool-calling turns
// should default to temperature 0, for models whose sampling jitter degrades
// tool-arg fidelity (DeepSeek's reasoning-disabled tool turns). Callers still
// skip this when the client set temperature explicitly.
func openRouterForcesToolTemperatureZero(model string) bool {
	return strings.HasPrefix(model, "deepseek/")
}

// isGLM51 reports whether the model id is z-ai/glm-5.1. GLM-5.1's streaming
// tool-call fix is opt-in (tool_stream=true, docs.z.ai/guides/capabilities/stream-tool);
// without it tool_call envelopes arrive with empty arguments like GLM-5. We
// also disable thinking-mode on the DeepInfra path so reasoning doesn't leak
// into the stream; OpenRouter's case is handled by openRouterReasoningHint.
func isGLM51(model string) bool {
	return model == "z-ai/glm-5.1"
}

// isQwen3Family reports whether the model id belongs to the qwen3.x family.
// These variants drift into tool-call/thinking loops without the model
// card's recommended sampling defaults, which we layer in when unset.
func isQwen3Family(model string) bool {
	return strings.HasPrefix(model, "qwen/qwen3")
}

// Qwen3 sampling defaults from the official model card
// (huggingface.co/Qwen/Qwen3-235B-A22B-Instruct-2507), applied only when the
// client hasn't set the field. presence_penalty=1.5 suppresses the
// "same tool, same args, N times" loop the Instruct variant is prone to.
const (
	qwen3Temperature       = 0.7
	qwen3TopP              = 0.8
	qwen3PresencePenalty   = 1.5
	qwen3RepetitionPenalty = 1.05
)
