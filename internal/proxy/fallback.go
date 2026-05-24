package proxy

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"workweave/router/internal/observability"
	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"
)

// firstByteGuard wraps an http.ResponseWriter and records whether anything
// has been written to it. Used by dispatchWithFallback to decide whether a
// retry is safe — once any byte has reached the client, we are committed
// to the current attempt and must not start a second.
type firstByteGuard struct {
	inner   http.ResponseWriter
	written bool
}

func newFirstByteGuard(w http.ResponseWriter) *firstByteGuard {
	return &firstByteGuard{inner: w}
}

func (g *firstByteGuard) Header() http.Header { return g.inner.Header() }

func (g *firstByteGuard) Write(p []byte) (int, error) {
	if len(p) > 0 {
		g.written = true
	}
	return g.inner.Write(p)
}

func (g *firstByteGuard) WriteHeader(status int) {
	g.written = true
	g.inner.WriteHeader(status)
}

func (g *firstByteGuard) Flush() {
	if f, ok := g.inner.(http.Flusher); ok {
		f.Flush()
	}
}

// dispatchAttempt is the per-binding work: build the prep body, set up
// translators, call p.Proxy, finalize. Returns the upstream error
// unmodified — dispatchWithFallback inspects it to decide on retry.
type dispatchAttempt func(ctx context.Context, decision router.Decision, p providers.Client) error

// failoverInputs bundles the inputs dispatchWithFallback needs that don't
// belong to a single attempt.
type failoverInputs struct {
	// w is the real client writer. All buffered-error final flushes hit
	// this directly (not the translator chain).
	w http.ResponseWriter
	// guard is the writer the per-attempt code writes through. Its
	// HasWritten() bit gates retry.
	guard *firstByteGuard
	// initialDecision carries the model + cluster metadata from the
	// router. dispatchWithFallback rewrites Provider per-attempt.
	initialDecision router.Decision
	// bindings is the ordered list of (provider, upstream-id, price) the
	// model has in catalog, filtered to providers wired in this deploy.
	// Index 0 is the primary; >0 are fallbacks.
	bindings []catalog.ProviderBinding
	// attempt does one per-binding dispatch.
	attempt dispatchAttempt
}

// dispatchWithFallback runs the per-attempt closure against each binding
// in order, retrying on providers.IsRetryable errors when no bytes have
// reached the client. Returns the index of the binding that succeeded (or
// the last one tried) and the final dispatch error.
//
// On final-attempt buffered error, writes the upstream headers + body
// straight to w so the client sees the underlying provider's response
// envelope rather than a generic 502 from us.
func (s *Service) dispatchWithFallback(ctx context.Context, in failoverInputs) (winnerIdx int, err error) {
	log := observability.Get()
	if len(in.bindings) == 0 {
		return -1, &providers.UpstreamStatusError{Status: http.StatusBadGateway}
	}

	for i, b := range in.bindings {
		// On the primary attempt, ctx already carries client/byok credentials
		// resolved by the caller. On fallback attempts we re-resolve against
		// an empty header set: shouldFailover() already ruled out BYOK and
		// client-credential paths, so the only credential source for a
		// fallback is the deployment env key on the next provider client.
		attemptCtx := ctx
		if i > 0 {
			attemptCtx = resolveAndInjectCredentials(ctx, b.Provider, http.Header{})
		}
		decision := in.initialDecision
		decision.Provider = b.Provider

		p, provErr := s.provider(b.Provider)
		if provErr != nil {
			// Provider was eligible at boot but missing now — treat as
			// retryable transport-class error and try the next binding.
			log.Warn("dispatchWithFallback: provider not configured at runtime",
				"provider", b.Provider,
				"model", decision.Model,
				"attempt_index", i)
			if i < len(in.bindings)-1 {
				continue
			}
			return i, provErr
		}

		// Reflect the current attempt in the response header so debugging
		// logs / x-router-* headers match where the request actually went.
		// Safe to rewrite until the first byte is flushed; on retry the
		// previous value is overwritten.
		if !in.guard.written {
			in.w.Header().Set("x-router-provider", b.Provider)
			if i > 0 {
				in.w.Header().Set("x-router-fallback-from", in.bindings[0].Provider)
				in.w.Header().Set("x-router-fallback-attempt", attemptIdxLabel(i))
			}
		}

		attemptErr := in.attempt(attemptCtx, decision, p)
		if attemptErr == nil {
			if i > 0 {
				log.Info("dispatchWithFallback: succeeded on fallback",
					"model", decision.Model,
					"primary_provider", in.bindings[0].Provider,
					"final_provider", b.Provider,
					"attempt_index", i)
			}
			return i, nil
		}

		// If anything has already been flushed to the client we are
		// committed to this attempt and must return its error — even if
		// the error type itself would be retryable in isolation.
		if in.guard.written {
			return i, attemptErr
		}

		if !providers.IsRetryable(attemptErr) || i == len(in.bindings)-1 {
			// Final attempt or non-retryable. If the upstream buffered an
			// error response, flush it through to the client now.
			flushBufferedIfPresent(in.w, attemptErr)
			return i, attemptErr
		}

		log.Warn("dispatchWithFallback: retrying on next binding",
			"model", decision.Model,
			"failed_provider", b.Provider,
			"attempt_index", i,
			"err", attemptErr)
	}
	// Unreachable — the loop always returns inside.
	return len(in.bindings) - 1, errors.New("dispatchWithFallback: exhausted without return")
}

// shouldFailover reports whether the request is eligible for multi-binding
// failover. Customer-supplied credentials (BYOK or inbound client key)
// bind the request to a single provider — silently retrying on a
// different upstream would 401 with surprising semantics, so we skip.
func (s *Service) shouldFailover(ctx context.Context) bool {
	if s.byokOnly {
		return false
	}
	if CredentialsFromContext(ctx) != nil {
		return false
	}
	return true
}

// resolveBindingsForDispatch returns the ordered binding list the proxy
// should walk. When failover is disabled (BYOK active, single binding, or
// catalog miss), the result is a single-element slice carrying the
// already-resolved decision provider — preserves legacy behavior.
func (s *Service) resolveBindingsForDispatch(ctx context.Context, decision router.Decision) []catalog.ProviderBinding {
	primary := catalog.ProviderBinding{Provider: decision.Provider}
	if !s.shouldFailover(ctx) {
		return []catalog.ProviderBinding{primary}
	}
	available := s.deploymentKeyedProviders
	if available == nil {
		// Legacy "all registered" mode — fall back to single-attempt to
		// avoid retrying on providers whose keys aren't actually wired.
		return []catalog.ProviderBinding{primary}
	}
	bindings := catalog.AvailableBindings(decision.Model, available)
	if len(bindings) <= 1 {
		return []catalog.ProviderBinding{primary}
	}
	// Defensive: the scorer picks bindings[0] at boot time; if for any
	// reason the runtime decision lists a different provider, keep that
	// as the primary attempt and treat the rest as ordered fallbacks
	// minus duplicates.
	if bindings[0].Provider != decision.Provider {
		out := []catalog.ProviderBinding{primary}
		for _, b := range bindings {
			if b.Provider != decision.Provider {
				out = append(out, b)
			}
		}
		return out
	}
	return bindings
}

// flushBufferedIfPresent writes a *providers.UpstreamErrorResponse through
// to the client as the final response. No-op for any other error type.
func flushBufferedIfPresent(w http.ResponseWriter, err error) {
	var resp *providers.UpstreamErrorResponse
	if !errors.As(err, &resp) {
		return
	}
	for k, vs := range resp.Headers {
		canon := http.CanonicalHeaderKey(k)
		if _, hop := providers.HopByHopHeaders[canon]; hop {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

// attemptIdxLabel formats the fallback attempt index for the
// x-router-fallback-attempt response header. Caller already gates on
// i > 0, so i=0 never reaches here.
func attemptIdxLabel(i int) string {
	return strconv.Itoa(i)
}

