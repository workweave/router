package proxy

import (
	"context"
	"encoding/json"
	"net/http"

	"workweave/router/internal/observability"
	"workweave/router/internal/proxy/usage"
)

// unifiedLimitCapture is a request-scoped holder for the raw
// anthropic-ratelimit-unified-* header set observed on THIS request's own
// subscription-served upstream call. It exists purely as Phase 0
// instrumentation for the Claude Code cost-observing-proxy design
// (docs/internal/claude-code-cost-proxy-design.md) — verifying the header
// vocabulary (unified-status, overage-status, overage-disabled-reason,
// representative-claim) against real subscription traffic before any cost
// math depends on it. Nothing reads the captured value except the telemetry
// row builder; it has no effect on routing, subsidy, or usage-bypass.
type unifiedLimitCapture struct {
	raw map[string]string
}

type unifiedLimitCaptureKey struct{}

// withUnifiedLimitCapture installs an empty capture holder on ctx. The holder
// is a pointer, so the header observer (which runs on a copy of ctx derived
// from this one) mutates the SAME holder this function's caller retrieves
// later via UnifiedLimitHeadersFrom — no channel or extra plumbing needed,
// mirroring how CredentialsFromContext/clearCredentials share request state.
func withUnifiedLimitCapture(ctx context.Context) context.Context {
	return context.WithValue(ctx, unifiedLimitCaptureKey{}, &unifiedLimitCapture{})
}

// captureUnifiedLimitHeaders records the raw anthropic-ratelimit-unified-*
// header set on ctx's capture holder, if the resolved credential for this
// upstream call was a subscription (OAuth) credential and the request carries
// a capture holder. Anthropic-only: Codex/OpenAI headers are a different
// family (design doc §3) and out of scope for this instrumentation.
func captureUnifiedLimitHeaders(ctx context.Context, h http.Header) {
	c, ok := ctx.Value(unifiedLimitCaptureKey{}).(*unifiedLimitCapture)
	if !ok || c == nil {
		return
	}
	creds := CredentialsFromContext(ctx)
	if creds == nil || !creds.OAuth {
		return
	}
	raw := usage.RawAnthropicUnifiedHeaders(h)
	if raw == nil {
		return
	}
	c.raw = raw
}

// UnifiedLimitHeadersFrom returns the raw header map captured for this
// request (nil if withUnifiedLimitCapture was never installed, or nothing was
// captured — e.g. no subscription call was made this turn).
func UnifiedLimitHeadersFrom(ctx context.Context) map[string]string {
	c, ok := ctx.Value(unifiedLimitCaptureKey{}).(*unifiedLimitCapture)
	if !ok || c == nil {
		return nil
	}
	return c.raw
}

// unifiedLimitHeadersJSON marshals the raw header map captured for this
// request into the pre-marshaled []byte form InsertTelemetryParams expects for
// jsonb columns (nil when nothing was captured, so the column stays NULL
// rather than storing an empty object).
func unifiedLimitHeadersJSON(ctx context.Context) []byte {
	raw := UnifiedLimitHeadersFrom(ctx)
	if len(raw) == 0 {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		// Telemetry loss is acceptable; a marshal failure must never fail the
		// request, so log and leave the column NULL.
		observability.FromContext(ctx).Debug("Failed to marshal unified_limit_headers for telemetry", "err", err)
		return nil
	}
	return b
}
