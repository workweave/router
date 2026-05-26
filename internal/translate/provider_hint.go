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
		strings.HasPrefix(model, "xiaomi/"),
		model == "z-ai/glm-5.1":
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

// isGLM51 reports whether the model id is z-ai/glm-5.1. GLM-5.1 shipped the
// streaming tool-call fix behind an opt-in flag (tool_stream=true per Z.AI
// docs https://docs.z.ai/guides/capabilities/stream-tool); without it the
// model reverts to the GLM-5 behavior where tool_call envelopes arrive with
// empty arguments. We also disable thinking-mode on the DeepInfra path
// (chat_template_kwargs.enable_thinking=false) so reasoning blocks don't leak
// into the response stream; OpenRouter handles the same via openRouterReasoningHint.
func isGLM51(model string) bool {
	return model == "z-ai/glm-5.1"
}

// isQwen3Family reports whether the model id belongs to the qwen3.x family.
// Qwen3 variants (instruct, coder, qwen3-next, qwen3.6-*) are documented to
// drift into tool-call / thinking loops without the recommended sampling
// defaults; the Qwen3 model card publishes a tuned set of params for
// agentic/code workloads which we layer in when the client did not set them.
func isQwen3Family(model string) bool {
	return strings.HasPrefix(model, "qwen/qwen3")
}

// Qwen3 sampling defaults from the official model card and Unsloth runbook
// (huggingface.co/Qwen/Qwen3-235B-A22B-Instruct-2507 — "Sampling Parameters").
// Applied only when the client has not set the corresponding field, so callers
// with their own tuning still win. presence_penalty=1.5 specifically suppresses
// the "same tool, same args, N times in a row" loop the Instruct variant is
// prone to without CoT.
const (
	qwen3Temperature       = 0.7
	qwen3TopP              = 0.8
	qwen3PresencePenalty   = 1.5
	qwen3RepetitionPenalty = 1.05
)
