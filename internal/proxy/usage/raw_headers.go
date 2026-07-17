package usage

import (
	"net/http"
	"strings"
)

// anthropicUnifiedPrefix is the common prefix of every Anthropic unified
// rate-limit header.
const anthropicUnifiedPrefix = "anthropic-ratelimit-unified-"

// RawAnthropicUnifiedHeaders returns every anthropic-ratelimit-unified-*
// header verbatim as a name->value map (lowercased names), or nil if none are
// present.
//
// This exists alongside ParseAnthropicUnifiedHeaders (which extracts only the
// utilization/reset fields the subsidy feature needs) because the Claude Code
// cost-observing-proxy design
// (docs/internal/claude-code-cost-proxy-design.md in the WorkWeave monorepo)
// needs the full header set — unified-status, overage-status,
// overage-disabled-reason, representative-claim — to verify its billing
// classification assumptions against real traffic before depending on them.
// Capturing verbatim rather than adding fields to Snapshot keeps that
// unverified vocabulary out of the routing-critical parse path; a header-shape
// change here can only affect the telemetry column, never CostFactor/Exhausted.
func RawAnthropicUnifiedHeaders(h http.Header) map[string]string {
	var out map[string]string
	for name, vals := range h {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, anthropicUnifiedPrefix) {
			continue
		}
		if len(vals) == 0 {
			continue
		}
		if out == nil {
			out = make(map[string]string)
		}
		out[lower] = vals[0]
	}
	return out
}
