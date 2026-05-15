package translate

import "regexp"

// anthropicBillingHeaderPrefix matches the Claude Code billing-header line that
// breaks prompt-prefix caching on non-Anthropic upstreams.
var anthropicBillingHeaderPrefix = regexp.MustCompile(`\Ax-anthropic-billing-header:[^\n]*\n?`)

func stripAnthropicBillingHeader(s string) string {
	return anthropicBillingHeaderPrefix.ReplaceAllString(s, "")
}
