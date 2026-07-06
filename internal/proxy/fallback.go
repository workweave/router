package proxy

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"workweave/router/internal/observability"
	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/translate"
)

// preludeBuffer absorbs pre-upstream writes (the eager SSE Prelude) so
// per-request failover can retry on another binding without the client
// having seen bytes from a primary that ultimately failed.
//
// Flow: Seal() ends the buffering phase; the first Write/WriteHeader after
// that calls commit(), flushing the buffered status+body to inner and going
// pass-through. If the attempt errors before ever writing, Committed() is
// false and the caller can Discard() and retry or render the error itself.
// Committed() is the retry gate (replaces firstByteGuard.written).
type preludeBuffer struct {
	inner          http.ResponseWriter
	initialHeaders http.Header
	bufStatus      int
	bufBody        bytes.Buffer
	sealed         bool
	committed      bool
}

func newPreludeBuffer(w http.ResponseWriter) *preludeBuffer {
	// Snapshot headers so Discard() can restore them; Prelude only
	// Set/Del's, never appends, so sharing the inner []string is safe.
	snap := make(http.Header, len(w.Header()))
	for k, vs := range w.Header() {
		cp := make([]string, len(vs))
		copy(cp, vs)
		snap[k] = cp
	}
	return &preludeBuffer{inner: w, initialHeaders: snap}
}

func (b *preludeBuffer) Header() http.Header { return b.inner.Header() }

func (b *preludeBuffer) Write(p []byte) (int, error) {
	if b.committed {
		return b.inner.Write(p)
	}
	if b.sealed {
		// Post-Seal first write: commit buffered prelude, then pass through.
		if err := b.commit(); err != nil {
			return 0, err
		}
		return b.inner.Write(p)
	}
	// Pre-Seal: buffer.
	return b.bufBody.Write(p)
}

func (b *preludeBuffer) WriteHeader(status int) {
	if b.committed {
		b.inner.WriteHeader(status)
		return
	}
	if b.sealed {
		// Non-stream error status from Finalize: buffer then commit.
		b.bufStatus = status
		_ = b.commit()
		return
	}
	b.bufStatus = status
}

func (b *preludeBuffer) Flush() {
	if !b.committed {
		// Pre-commit Flush is a no-op — we don't want partial Prelude bytes
		// reaching the client before commit decides.
		return
	}
	if f, ok := b.inner.(http.Flusher); ok {
		f.Flush()
	}
}

// Seal marks the end of the Prelude phase. The next Write or WriteHeader
// will trigger commit().
func (b *preludeBuffer) Seal() { b.sealed = true }

// Committed reports whether any bytes have reached the inner writer.
func (b *preludeBuffer) Committed() bool { return b.committed }

// Discard resets buffered Prelude bytes and headers to the construction-time
// snapshot. No-op once committed. Called between failed attempts and before
// the exhaustion error renderer writes via the unwrapped inner writer.
func (b *preludeBuffer) Discard() {
	if b.committed {
		return
	}
	b.bufStatus = 0
	b.bufBody.Reset()
	b.sealed = false
	h := b.inner.Header()
	for k := range h {
		delete(h, k)
	}
	for k, vs := range b.initialHeaders {
		cp := make([]string, len(vs))
		copy(cp, vs)
		h[k] = cp
	}
}

func (b *preludeBuffer) commit() error {
	if b.committed {
		return nil
	}
	b.committed = true
	if b.bufStatus != 0 {
		b.inner.WriteHeader(b.bufStatus)
	}
	if b.bufBody.Len() > 0 {
		if _, err := b.inner.Write(b.bufBody.Bytes()); err != nil {
			return err
		}
	}
	b.bufBody.Reset()
	if f, ok := b.inner.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// dispatchAttempt does one per-binding dispatch. Returns the upstream error
// unmodified so dispatchWithFallback can decide whether to retry.
type dispatchAttempt func(ctx context.Context, decision router.Decision, p providers.Client) error

// failoverInputs bundles the inputs dispatchWithFallback needs that don't
// belong to a single attempt.
type failoverInputs struct {
	// w is the real client writer; flushErr writes here directly (bypassing
	// buf) so the client sees the upstream error in its expected format.
	w http.ResponseWriter
	// buf is the writer per-attempt code writes through; its Committed()
	// bit gates retry. nil when failover is impossible (single binding,
	// BYOK, legacy mode) — the loop then skips discard/commit entirely.
	buf *preludeBuffer
	// initialDecision carries the model + cluster metadata from the
	// router. dispatchWithFallback rewrites Provider per-attempt.
	initialDecision router.Decision
	// bindings is the ordered (provider, upstream-id, price) list, filtered
	// to providers wired in this deploy. Index 0 is primary, >0 fallbacks.
	bindings []catalog.ProviderBinding
	// attempt does one per-binding dispatch.
	attempt dispatchAttempt
	// flushErr renders the final-attempt upstream error to w in the
	// entry point's wire format. Optional — nil means do nothing on
	// exhaustion (the error is still returned to the caller).
	flushErr func(w http.ResponseWriter, err error)
	// deferFlushOnExhaustion suppresses flushErr on exhaustion (still
	// discards the buffer and returns the error), letting the caller run a
	// higher-level fallback instead (e.g. ProxyMessages' baseline failover
	// re-dispatching the Anthropic model when a routed OSS model exhausts).
	deferFlushOnExhaustion bool
}

// dispatchWithFallback runs the attempt closure against each binding in
// order, retrying on providers.IsRetryable errors while no bytes have
// reached the client. Returns the winning (or last-tried) index and error.
// On final-attempt error it flushes the upstream's own envelope to w
// instead of a generic 502.
func (s *Service) dispatchWithFallback(ctx context.Context, in failoverInputs) (winnerIdx int, err error) {
	log := observability.FromContext(ctx)
	if len(in.bindings) == 0 {
		return -1, &providers.UpstreamStatusError{Status: http.StatusBadGateway}
	}

	for i, b := range in.bindings {
		// Fallback attempts re-resolve credentials against an empty header set:
		// shouldFailover() already ruled out BYOK/client-credential paths, so
		// the only source left is the deployment env key on the next client.
		attemptCtx := ctx
		if i > 0 {
			attemptCtx = resolveAndInjectCredentials(ctx, b.Provider, http.Header{})
			// Drop the previous attempt's buffered Prelude bytes + any
			// header mutations it made. Safe pre-commit only.
			if in.buf != nil {
				in.buf.Discard()
			}
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

		// Same-binding retry: a transient blip often clears on quick retry.
		// Only used for single-binding models; multi-binding models fail
		// straight over to the next binding after one attempt (len>1 guard).
		var attemptErr error
		for sb := 0; ; sb++ {
			// Safe to rewrite until the buffer commits; on retry the previous
			// value is overwritten via Discard's header restore + this Set.
			if !committed(in.buf) {
				in.w.Header().Set(HeaderRouterProvider, b.Provider)
				// Refresh: decision.Model can change on baseline failover, so
				// x-router-model must never name a model that didn't serve.
				in.w.Header().Set(HeaderRouterModel, decision.Model)
				if i > 0 {
					in.w.Header().Set(HeaderRouterFallbackFrom, in.bindings[0].Provider)
					in.w.Header().Set(HeaderRouterFallbackAttempt, attemptIdxLabel(i))
				}
			}

			attemptErr = in.attempt(attemptCtx, decision, p)
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

			// Bytes already reached the client — committed to this attempt's
			// error even if it would otherwise be retryable.
			if committed(in.buf) {
				return i, attemptErr
			}

			// Stop same-binding retry: error non-retryable, budget spent, or
			// a different binding exists (cross-binding failover beats
			// re-hitting the same flaky provider).
			if !providers.IsRetryable(attemptErr) || sb >= maxSameBindingRetries || len(in.bindings) > 1 {
				break
			}
			// Reset before retrying so it begins with a pristine writer.
			if in.buf != nil {
				in.buf.Discard()
			}
			log.Warn("dispatchWithFallback: retrying same binding after transient error",
				"model", decision.Model,
				"provider", b.Provider,
				"attempt_index", i,
				"same_binding_retry", sb+1,
				"err", attemptErr)
			sleep := s.retrySleep
			if sleep == nil {
				sleep = sleepWithContext
			}
			if err := sleep(attemptCtx, sameBindingBackoff(sb)); err != nil {
				return i, attemptErr
			}
		}

		// 404 model-not-found isn't in IsRetryable (retrying the same provider
		// is futile) but a different binding may carry the model, rescuing a
		// stale/renamed upstream id that would otherwise hard-fail the turn.
		canFailover := providers.IsRetryable(attemptErr) || providers.IsUpstreamModelNotFound(attemptErr)
		if !canFailover || i == len(in.bindings)-1 {
			// Final attempt or non-failable: discard so the client sees the
			// error envelope next, not a half-emitted message_start.
			if in.buf != nil {
				in.buf.Discard()
			}
			if in.flushErr != nil && !in.deferFlushOnExhaustion {
				in.flushErr(in.w, attemptErr)
			}
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

// committed is a nil-safe shorthand for in.buf.Committed(); the
// single-binding fast path passes buf=nil and is treated as not committed.
func committed(b *preludeBuffer) bool {
	if b == nil {
		return false
	}
	return b.Committed()
}

const (
	// maxSameBindingRetries bounds same-binding retries after a transient
	// error (5xx/408/429, reset). This is the only failover single-binding
	// models get, since they have no other provider to walk to.
	maxSameBindingRetries = 2
	// sameBindingBackoffBase is the first retry delay, doubling per attempt.
	sameBindingBackoffBase = 250 * time.Millisecond
)

// sameBindingBackoff is the delay before same-binding retry attempt+1
// (0-indexed): exponential off sameBindingBackoffBase.
func sameBindingBackoff(attempt int) time.Duration {
	return sameBindingBackoffBase << attempt
}

// sleepWithContext waits d, returning early with ctx.Err() if ctx is
// canceled or its deadline elapses first — a client disconnect or budget
// expiry must abort the backoff rather than burn the remaining budget.
func sleepWithContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// shouldFailover reports failover eligibility. Customer-supplied
// credentials (BYOK or client key) bind to a single provider — retrying
// elsewhere would 401 unexpectedly, so we skip failover.
func (s *Service) shouldFailover(ctx context.Context) bool {
	if s.byokOnly {
		return false
	}
	if CredentialsFromContext(ctx) != nil {
		return false
	}
	return true
}

// resolveBindingsForDispatch returns the ordered binding list to walk.
// When failover is disabled or unavailable, returns a single-element
// slice carrying the already-resolved decision provider.
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
	// Exclusions must hold during failover too, or a fallback binding could
	// resurrect a provider the scorer already filtered out.
	excluded := s.excludedProvidersForRequest(ctx)
	if len(excluded) > 0 {
		filtered := make(map[string]struct{}, len(available))
		for p := range available {
			if _, drop := excluded[p]; !drop {
				filtered[p] = struct{}{}
			}
		}
		available = filtered
	}
	_, primaryExcluded := excluded[decision.Provider]
	bindings := catalog.AvailableBindings(decision.Model, available)
	if len(bindings) == 0 {
		if primaryExcluded {
			// Decision names an excluded provider with no other bindings (a
			// bug upstream) — return nil so dispatch 502s instead of
			// dispatching to the forbidden provider.
			return nil
		}
		return []catalog.ProviderBinding{primary}
	}
	if len(bindings) == 1 && !primaryExcluded {
		return []catalog.ProviderBinding{primary}
	}
	// Defensive: if the runtime decision's provider differs from
	// bindings[0], keep the decision's as primary and dedupe the rest —
	// unless it's excluded, in which case serve only eligible bindings.
	if bindings[0].Provider != decision.Provider {
		if primaryExcluded {
			return bindings
		}
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

// flushBufferedIfPresent writes an *UpstreamErrorResponse through to the
// client verbatim. No-op for other error types.
//
// Content-Length/Encoding are dropped: MaxBufferedErrorBytes caps the body
// we hold, so the upstream's advertised length may exceed what we actually
// write, breaking HTTP framing. net/http recomputes Content-Length itself.
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
		if canon == "Content-Length" || canon == "Content-Encoding" {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

// emitAnthropicSSEErrorEvent writes an Anthropic-shape `event: error` SSE
// frame and returns *UpstreamStatusError so flushErr becomes a no-op.
// Used when the upstream errors after Prelude already committed HTTP 200 +
// message_start — a JSON error envelope would corrupt the SSE stream, but
// an `event: error` frame terminates cleanly. Returns err unchanged if it
// is not an *UpstreamErrorResponse.
func emitAnthropicSSEErrorEvent(sink http.ResponseWriter, err error) error {
	var resp *providers.UpstreamErrorResponse
	if !errors.As(err, &resp) {
		return err
	}
	anthErrJSON := translate.OpenAIToAnthropicError(resp.Body)
	_, _ = sink.Write([]byte("event: error\ndata: "))
	_, _ = sink.Write(anthErrJSON)
	_, _ = sink.Write([]byte("\n\n"))
	if f, ok := sink.(http.Flusher); ok {
		f.Flush()
	}
	return &providers.UpstreamStatusError{Status: resp.Status}
}

// emitOpenAISSEErrorEvent is emitAnthropicSSEErrorEvent's OpenAI-shape
// counterpart: writes a verbatim `data: {...}` frame, used after
// OpenAIRoutingMarkerWriter has already committed HTTP 200.
func emitOpenAISSEErrorEvent(sink http.ResponseWriter, err error) error {
	var resp *providers.UpstreamErrorResponse
	if !errors.As(err, &resp) {
		return err
	}
	_, _ = sink.Write([]byte("data: "))
	_, _ = sink.Write(resp.Body)
	_, _ = sink.Write([]byte("\n\n"))
	if f, ok := sink.(http.Flusher); ok {
		f.Flush()
	}
	return &providers.UpstreamStatusError{Status: resp.Status}
}

// flushUpstreamErrorAsAnthropic is ProxyMessages' flushErr: translates the
// OpenAI-compat upstream's error body to Anthropic-shape JSON. No-op when
// err is not an *UpstreamErrorResponse.
func flushUpstreamErrorAsAnthropic(w http.ResponseWriter, err error) {
	var resp *providers.UpstreamErrorResponse
	if !errors.As(err, &resp) {
		return
	}
	for k, vs := range resp.Headers {
		canon := http.CanonicalHeaderKey(k)
		if _, hop := providers.HopByHopHeaders[canon]; hop {
			continue
		}
		if canon == "Content-Type" || canon == "Content-Length" {
			continue // body is rewritten below
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.Status)
	_, _ = w.Write(translate.OpenAIToAnthropicError(resp.Body))
}

// attemptIdxLabel formats i for the x-router-fallback-attempt header.
// Caller gates on i > 0.
func attemptIdxLabel(i int) string {
	return strconv.Itoa(i)
}
