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
// Exists alongside ParseAnthropicUnifiedHeaders because Phase 0 telemetry needs
// the full header set verbatim to verify billing vocabulary before any cost math
// depends on it. Keeping unverified fields out of Snapshot means a header-shape
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
