package proxy

import (
	"context"
	"encoding/json"
	"net/http"

	"workweave/router/internal/observability"
	"workweave/router/internal/proxy/usage"
)

// unifiedLimitCapture is a request-scoped holder for the raw
// anthropic-ratelimit-unified-* header set. Phase 0 instrumentation only —
// nothing reads it except the telemetry row builder; no effect on routing,
// subsidy, or usage-bypass.
//
// Retry/failover semantics: the captured set describes the LAST
// subscription-served attempt, which may not be the attempt that produced the
// response — a 429'd subscription attempt whose retry serves on the
// deployment key leaves the 429's headers here (that near-cap reading is
// exactly what Phase 0 wants). Consumers disambiguate via the row's
// failover_used and credential_source columns.
type unifiedLimitCapture struct {
	raw map[string]string
}

type unifiedLimitCaptureKey struct{}

// withUnifiedLimitCapture installs an empty capture holder on ctx. The holder
// is a pointer, so the header observer mutates the same struct the caller
// reads later via UnifiedLimitHeadersFrom — no extra plumbing needed.
func withUnifiedLimitCapture(ctx context.Context) context.Context {
	return context.WithValue(ctx, unifiedLimitCaptureKey{}, &unifiedLimitCapture{})
}

// captureUnifiedLimitHeaders records the raw anthropic-ratelimit-unified-*
// headers on ctx's holder when the resolved credential is OAuth (subscription).
// Skipped for BYOK/deployment calls — those headers describe a different account.
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
