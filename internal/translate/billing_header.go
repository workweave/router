package translate

import "regexp"

// anthropicBillingHeaderPrefix matches the "x-anthropic-billing-header: <line>"
// prelude Claude Code injects into the leading system text block. Carries a
// per-request `cch=<hex>` nonce plus a per-session `cc_version` suffix;
// forwarding it to non-Anthropic upstreams (OpenRouter / DeepSeek / Gemini
// OpenAI-compat) busts their automatic prompt-prefix cache because bytes
// 14–118 of the body change every single turn. Anthropic strips this
// server-side before computing the cache key; foreign upstreams cannot.
//
// Trailing newline is optional: Claude Code emits the header as a STANDALONE
// system text block with no terminator, then the rest of the system prompt
// follows in a SEPARATE text block. The flatten step joins them with "\n",
// which is where the newline observed downstream comes from.
var anthropicBillingHeaderPrefix = regexp.MustCompile(`\Ax-anthropic-billing-header:[^\n]*\n?`)

// stripAnthropicBillingHeader removes the leading Claude Code billing-header
// line from a system text block, if present. Idempotent. Returns the input
// unchanged when no header is found.
func stripAnthropicBillingHeader(s string) string {
	return anthropicBillingHeaderPrefix.ReplaceAllString(s, "")
}
