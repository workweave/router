package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"workweave/router/internal/observability/otel"
	"workweave/router/internal/providers"
	"workweave/router/internal/providers/httputil"
	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/translate"
)

// fallbackOutcome captures the result of a single fallback attempt, surfaced
// into the ProxyMessages complete log so a postmortem on a Bedrock-mantle
// stall can tell whether the retry path saved the request or not.
type fallbackOutcome struct {
	Attempted    bool
	FromProvider string
	ToProvider   string
	Reason       string // "sse_idle" | "5xx" | "connect"
	FirstErr     error
}

// proxyWithFallback dispatches the OpenAI-compat-formatted request via the
// chosen provider and, on a pre-first-byte retryable failure, re-emits the
// body for the next eligible binding in the catalog and tries once more.
//
// Retryable failures: ErrUpstreamIdleTimeout (SSE inactivity watchdog) and
// UpstreamStatusError{Status: 5xx}. We can only retry safely while
// t.UpstreamFirstByteNanos is zero — once any upstream byte has been
// translated into the response writer, the client has committed to this
// response and the second attempt would mix two streams.
//
// enabledProviders carries the per-request provider eligibility set produced
// by enabledProvidersForRequest — BYOK requests narrow this beyond the
// deploy-wired set, and a fallback that ignored it could silently spend
// deployment credentials on a provider the request never authorized. A nil
// or empty map disables the eligibility filter (deploy-wired only).
//
// On fallback the caller's decision.Provider is mutated to the new binding
// so downstream cost math, billing, and the ProxyMessages complete log
// reflect the actually-served provider. decision.Model is unchanged because
// catalog bindings share a model id.
func (s *Service) proxyWithFallback(
	ctx context.Context,
	decision *router.Decision,
	env *translate.RequestEnvelope,
	opts translate.EmitOptions,
	enabledProviders map[string]struct{},
	sink http.ResponseWriter,
	r *http.Request,
) (proxyErr error, outcome fallbackOutcome) {
	t := otel.TimingFrom(ctx)

	p, err := s.provider(decision.Provider)
	if err != nil {
		return err, outcome
	}
	prep, err := env.PrepareOpenAI(r.Header, opts)
	if err != nil {
		return fmt.Errorf("emit body: %w", err), outcome
	}
	proxyErr = p.Proxy(ctx, *decision, prep, sink, r)
	if proxyErr == nil {
		return nil, outcome
	}

	// Can we retry?
	reason, retryable := classifyFallbackError(proxyErr)
	if !retryable {
		return proxyErr, outcome
	}
	if t != nil && t.UpstreamFirstByteNanos.Load() != 0 {
		// Upstream already streamed at least one byte; the translator has
		// emitted deltas. Switching providers mid-stream would interleave
		// two model outputs in one Anthropic message envelope.
		return proxyErr, outcome
	}

	nextBinding, ok := s.findFallbackBinding(decision.Model, decision.Provider, enabledProviders)
	if !ok {
		return proxyErr, outcome
	}

	origProvider := decision.Provider
	decision.Provider = nextBinding.Provider
	opts.TargetProvider = nextBinding.Provider

	prep2, err := env.PrepareOpenAI(r.Header, opts)
	if err != nil {
		return fmt.Errorf("emit body (fallback): %w", err), outcome
	}

	// Strip the original provider's credentials from ctx before re-resolving
	// for the fallback target. BYOK credentials and inbound client headers are
	// provider-specific; without this, a Bedrock BYOK key in ctx would be
	// forwarded as the Authorization header to OpenRouter when
	// resolveAndInjectCredentials finds no creds for the new provider and
	// leaves the original CredentialsContextKey value in place. That's a
	// cross-provider credential leak.
	ctx2 := context.WithValue(ctx, CredentialsContextKey{}, (*Credentials)(nil))
	ctx2 = resolveAndInjectCredentials(ctx2, decision.Provider, r.Header)
	p2, err := s.provider(decision.Provider)
	if err != nil {
		return err, outcome
	}

	outcome = fallbackOutcome{
		Attempted:    true,
		FromProvider: origProvider,
		ToProvider:   nextBinding.Provider,
		Reason:       reason,
		FirstErr:     proxyErr,
	}
	return p2.Proxy(ctx2, *decision, prep2, sink, r), outcome
}

// classifyFallbackError reports whether err warrants a single retry against
// the next eligible binding and returns a short reason tag for logs.
func classifyFallbackError(err error) (reason string, retryable bool) {
	if errors.Is(err, httputil.ErrUpstreamIdleTimeout) {
		return "sse_idle", true
	}
	var statusErr *providers.UpstreamStatusError
	if errors.As(err, &statusErr) && statusErr.Status >= 500 {
		return "5xx", true
	}
	return "", false
}

// findFallbackBinding walks the catalog's ordered binding list and returns
// the first entry that is (a) wired in this deploy, (b) eligible for this
// request, and (c) not the just-failed provider. Returns false when no
// eligible alternative exists.
//
// enabledProviders is the per-request eligibility set from
// enabledProvidersForRequest — BYOK requests narrow it beyond the deploy-wired
// set, and routing on a binding outside this set would charge deployment
// credentials for a provider the request did not authorize. nil disables the
// per-request filter (deploy-wired only).
func (s *Service) findFallbackBinding(modelID, failedProvider string, enabledProviders map[string]struct{}) (catalog.ProviderBinding, bool) {
	m, ok := catalog.ByID(modelID)
	if !ok {
		return catalog.ProviderBinding{}, false
	}
	for _, b := range m.Providers {
		if b.Provider == failedProvider {
			continue
		}
		if _, wired := s.providers[b.Provider]; !wired {
			continue
		}
		if enabledProviders != nil {
			if _, eligible := enabledProviders[b.Provider]; !eligible {
				continue
			}
		}
		return b, true
	}
	return catalog.ProviderBinding{}, false
}
