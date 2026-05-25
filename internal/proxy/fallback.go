package proxy

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strconv"

	"workweave/router/internal/observability"
	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/translate"
)

// preludeBuffer wraps an http.ResponseWriter to absorb pre-upstream writes
// (the eager SSE Prelude added in main #220) so per-request failover can
// retry on a different binding without committing the response to the
// client based on bytes a failed primary upstream never produced.
//
// Lifecycle per attempt:
//  1. Construction snapshots the inner Header() so Discard() can undo
//     Prelude's `Set("Content-Type", "text/event-stream")` + `Del`s.
//  2. The translator chain wires on top of this writer; the per-attempt
//     closure calls translator.Prelude(...). WriteHeader + Write land in
//     the buffer; inner is untouched.
//  3. Closure calls Seal() to mark "Prelude phase done."
//  4. p.Proxy(...) runs. The first post-Seal call (Write or WriteHeader)
//     triggers commit(): buffered status + body flushed + Flush(); the
//     buffer goes pass-through for the rest of the request.
//  5. If p.Proxy returns an error without ever writing (openaicompat
//     buffers errors and returns *UpstreamErrorResponse without touching
//     the chain), Committed() is false. Dispatch decides:
//     - retry: caller invokes Discard() to reset buffer + headers.
//     - exhaustion: caller invokes Discard() then writes the format-
//     specific error envelope via the unwrapped inner writer.
//
// Committed() is the new retry gate; replaces firstByteGuard.written.
type preludeBuffer struct {
	inner          http.ResponseWriter
	initialHeaders http.Header
	bufStatus      int
	bufBody        bytes.Buffer
	sealed         bool
	committed      bool
}

func newPreludeBuffer(w http.ResponseWriter) *preludeBuffer {
	// Shallow-clone the existing header map so Discard() can restore it.
	// Prelude mutates via Set/Del only — never appends to existing slices
	// — so sharing the inner []string is safe.
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
		// Translator may write a >=400 status via Finalize on non-stream
		// errors; capture into the buffered status, then commit so the
		// inner WriteHeader fires with the right code.
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

// Discard drops buffered Prelude bytes and restores the inner Header()
// to its construction-time snapshot. Legal only pre-commit; no-op after.
// Called between attempts when the previous attempt failed pre-byte, and
// before the format-specific exhaustion error renderer writes via the
// unwrapped inner writer.
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

// dispatchAttempt is the per-binding work: build the prep body, set up
// translators, call p.Proxy, finalize. Returns the upstream error
// unmodified — dispatchWithFallback inspects it to decide on retry.
type dispatchAttempt func(ctx context.Context, decision router.Decision, p providers.Client) error

// failoverInputs bundles the inputs dispatchWithFallback needs that don't
// belong to a single attempt.
type failoverInputs struct {
	// w is the real client writer. The format-specific flushErr writes
	// here directly (bypassing buf) so the customer sees the upstream
	// error envelope in the format their client expects.
	w http.ResponseWriter
	// buf is the writer the per-attempt code writes through. Its
	// Committed() bit gates retry. nil when the entry point determined
	// failover is impossible (single binding, BYOK, legacy mode) — in
	// that case the dispatch loop walks the single binding and skips
	// the discard/commit lifecycle entirely.
	buf *preludeBuffer
	// initialDecision carries the model + cluster metadata from the
	// router. dispatchWithFallback rewrites Provider per-attempt.
	initialDecision router.Decision
	// bindings is the ordered list of (provider, upstream-id, price) the
	// model has in catalog, filtered to providers wired in this deploy.
	// Index 0 is the primary; >0 are fallbacks.
	bindings []catalog.ProviderBinding
	// attempt does one per-binding dispatch.
	attempt dispatchAttempt
	// flushErr renders the final-attempt upstream error to w in the
	// entry-point's wire format. For ProxyMessages this translates
	// OpenAI/Fireworks/etc. JSON to Anthropic-shape; for
	// ProxyOpenAIChatCompletion it passes through verbatim. Optional —
	// nil means do nothing on exhaustion (the upstream error error value
	// is still returned to the caller).
	flushErr func(w http.ResponseWriter, err error)
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
	log := observability.FromContext(ctx)
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

		// Reflect the current attempt in the response header so debugging
		// logs / x-router-* headers match where the request actually went.
		// Safe to rewrite until the buffer commits; on retry the previous
		// value is overwritten via Discard's header restore + this Set.
		if !committed(in.buf) {
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

		// If anything has already reached the client (preludeBuffer.commit
		// fired during the attempt), we are committed to this attempt and
		// must return its error — even if the error type itself would be
		// retryable in isolation.
		if committed(in.buf) {
			return i, attemptErr
		}

		if !providers.IsRetryable(attemptErr) || i == len(in.bindings)-1 {
			// Final attempt or non-retryable. Drop the buffered Prelude
			// (the next bytes the client sees should be the error envelope,
			// not a half-emitted message_start) and let the entry point
			// render the upstream error in the right wire format.
			if in.buf != nil {
				in.buf.Discard()
			}
			if in.flushErr != nil {
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

// committed is a nil-safe shorthand for in.buf.Committed(). The
// single-binding fast path passes buf=nil, in which case we treat the
// request as "not committed" — irrelevant since single-binding never
// retries anyway, but keeps the gate uniform.
func committed(b *preludeBuffer) bool {
	if b == nil {
		return false
	}
	return b.Committed()
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
// to the client as the final response, body unchanged. Used as the
// flushErr callback by entry points whose inbound wire format matches
// the upstream's (OpenAI Chat Completions inbound + OpenAI-compat
// upstream). No-op for any other error type.
//
// Content-Length and Content-Encoding are dropped: providers.MaxBufferedErrorBytes
// caps the body we hold, so the upstream's advertised length may exceed the
// bytes we actually Write — forwarding it verbatim would either deadlock
// clients waiting for missing bytes or break HTTP framing. The Go net/http
// layer recomputes Content-Length from the bytes that pass through Write.
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
// frame to sink and returns *UpstreamStatusError to signal "bytes already
// flushed to client" (so the dispatch loop's format-specific flushErr —
// which only acts on *UpstreamErrorResponse — becomes a no-op).
//
// Used by single-binding cross-format streaming closures when the upstream
// errors after translator.Prelude() has already committed HTTP 200 +
// `message_start` to the wire: appending a JSON error envelope at that
// point produces a corrupt SSE stream, while an `event: error` frame
// terminates cleanly within the format the client is parsing.
//
// Returns err unchanged when it is not a *UpstreamErrorResponse.
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

// emitOpenAISSEErrorEvent writes an OpenAI-shape `data: {...}` SSE frame
// carrying the upstream error envelope verbatim, then returns
// *UpstreamStatusError (see emitAnthropicSSEErrorEvent for the rationale).
// Used by single-binding ProxyOpenAIChatCompletion streaming closures
// where the OpenAIRoutingMarkerWriter has already committed HTTP 200 +
// the routing-marker chat.completion chunk.
//
// Returns err unchanged when it is not a *UpstreamErrorResponse.
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

// flushUpstreamErrorAsAnthropic is the flushErr callback for ProxyMessages.
// On failover exhaustion the upstream is an OpenAI-compat provider
// (Fireworks/DeepInfra/Bedrock/OpenRouter) emitting an OpenAI-shape error
// envelope; the client is an Anthropic Messages caller expecting
// `{"type":"error","error":{...}}`. Translates the body via
// translate.OpenAIToAnthropicError + forces Content-Type to application/json.
// No-op when err is not an *UpstreamErrorResponse.
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

// attemptIdxLabel formats the fallback attempt index for the
// x-router-fallback-attempt response header. Caller already gates on
// i > 0, so i=0 never reaches here.
func attemptIdxLabel(i int) string {
	return strconv.Itoa(i)
}
