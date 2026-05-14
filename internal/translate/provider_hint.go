package translate

import "strings"

// openRouterProviderHint returns the OpenRouter `provider` request-body field
// for OSS model slugs that need pinning to caching-capable backends. Returns
// nil when the target model doesn't need a hint (first-party Anthropic /
// OpenAI / Google, Fireworks, or any unrecognized slug).
//
// Without a hint OpenRouter load-balances by price across every provider
// serving the model. For deepseek/* and moonshotai/* that routinely picks
// third-party hosts (Parasail, Baidu, etc.) which don't implement prefix
// caching — fatal for agentic workloads where every turn re-sends a
// 25-30K-token transcript. Pinning to the model's native provider re-enables
// automatic prefix caching and lets OpenRouter's sticky-routing keep the
// cache warm across turns.
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
