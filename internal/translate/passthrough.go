package translate

import (
	"net/http"
	"strings"

	"workweave/router/internal/providers"
)

// PrepareAnthropicPassthrough builds a PreparedRequest for non-routing
// Anthropic endpoints. Strips inference-time fields and thinking-related betas.
func (e *RequestEnvelope) PrepareAnthropicPassthrough(in http.Header) (providers.PreparedRequest, error) {
	ov, changed := resolvePassthroughOverrides(e.body)
	if !changed {
		return providers.PreparedRequest{Body: e.body, Headers: AnthropicPassthroughHeaders(in)}, nil
	}
	body, err := e.emitSameFormat(ov)
	if err != nil {
		return providers.PreparedRequest{}, err
	}
	return providers.PreparedRequest{Body: body, Headers: AnthropicPassthroughHeaders(in)}, nil
}

// AnthropicPassthroughHeaders returns header overrides for Anthropic passthrough.
func AnthropicPassthroughHeaders(in http.Header) http.Header {
	h := make(http.Header)
	if v := in.Get("anthropic-version"); v != "" {
		h.Set("anthropic-version", v)
	} else {
		h.Set("anthropic-version", "2023-06-01")
	}
	if v := stripThinkingBetas(in.Get("anthropic-beta")); v != "" {
		h.Set("anthropic-beta", v)
	}
	return h
}

func stripThinkingBetas(beta string) string {
	return joinKept(beta, func(token string) bool {
		return !strings.Contains(token, "thinking")
	})
}
