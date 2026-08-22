package proxy

import (
	"context"
	"net/http"
)

// Skip reasons reported by gateSummarizerCall, used as log fields so an
// operator can tell a policy refusal apart from a tenant-boundary one.
const (
	summarizerSkipProviderExcluded = "provider_excluded"
	summarizerSkipModelExcluded    = "model_excluded"
	summarizerSkipTenantBoundary   = "tenant_boundary"
)

// summarizerGate is the resolved verdict for one synthetic summarizer call:
// whether it may run at all, and under whose credentials.
type summarizerGate struct {
	// Creds are the caller's own forwarded credentials for the summarizer's
	// provider, or nil to run on the deployment key.
	Creds *Credentials
	// Allowed reports whether the call may be dispatched.
	Allowed bool
	// SkipReason is one of the summarizerSkip* constants when Allowed is false.
	SkipReason string
}

// gateSummarizerCall decides whether a summary call may be dispatched to
// provider/model. Summaries ship the full prior conversation, so policy
// exclusions apply — policyExcludedProviders, not session strike-outs, which
// are transient evidence. On BYOK/client-keyed requests without matching
// forwarded creds, skip rather than spend the deployment key across the tenant
// boundary.
func (s *Service) gateSummarizerCall(ctx context.Context, provider, model string, headers http.Header) summarizerGate {
	if _, excluded := s.policyExcludedProviders(ctx)[provider]; excluded {
		return summarizerGate{SkipReason: summarizerSkipProviderExcluded}
	}
	if model != "" {
		if _, excluded := s.excludedModelsForRequest(ctx)[model]; excluded {
			return summarizerGate{SkipReason: summarizerSkipModelExcluded}
		}
	}
	creds := resolveSummarizerCreds(ctx, provider, headers)
	if creds == nil && s.requestUsesNonDeploymentCreds(ctx, headers) {
		return summarizerGate{SkipReason: summarizerSkipTenantBoundary}
	}
	return summarizerGate{Creds: creds, Allowed: true}
}

// summarizerContext returns the context for the summary call: caller creds when
// available, otherwise stripped so a subscription OAuth token can't 401 or
// cross a tenant boundary.
func (g summarizerGate) summarizerContext(ctx context.Context) context.Context {
	if g.Creds != nil {
		return context.WithValue(ctx, CredentialsContextKey{}, g.Creds)
	}
	return clearCredentials(ctx)
}
