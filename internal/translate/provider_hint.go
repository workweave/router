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
func openRouterReasoningHint(model string) map[string]any {
	if strings.HasPrefix(model, "deepseek/") {
		return map[string]any{"enabled": false}
	}
	return nil
}
