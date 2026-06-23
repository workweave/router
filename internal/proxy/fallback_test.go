package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"
)

// fakeClient is a per-attempt scripted providers.Client. It exposes the
// minimum surface the dispatch helper needs: Proxy returns whatever the
// next scripted outcome says, while optionally writing bytes to w to
// simulate a partial flush. Passthrough is unused here.
type fakeClient struct {
	name     string
	outcomes []fakeOutcome
	calls    int
}

type fakeOutcome struct {
	writeBytes []byte // bytes to write to w before returning
	err        error  // nil = success
}

func (f *fakeClient) Proxy(ctx context.Context, _ router.Decision, _ providers.PreparedRequest, w http.ResponseWriter, _ *http.Request) error {
	idx := f.calls
	f.calls++
	if idx >= len(f.outcomes) {
		return fmt.Errorf("fakeClient %q: unexpected call %d", f.name, idx)
	}
	out := f.outcomes[idx]
	if len(out.writeBytes) > 0 {
		if _, werr := w.Write(out.writeBytes); werr != nil {
			return werr
		}
	}
	return out.err
}

func (f *fakeClient) Passthrough(context.Context, providers.PreparedRequest, http.ResponseWriter, *http.Request) error {
	return providers.ErrNotImplemented
}

// newServiceWithProviders builds a *Service with just enough state for
// dispatchWithFallback to operate. The router and other dependencies are
// nil — dispatchWithFallback doesn't read them.
func newServiceWithProviders(t *testing.T, providerMap map[string]providers.Client) *Service {
	t.Helper()
	s := &Service{providers: providerMap}
	return s
}

func responseHeaderTimeoutErr(t *testing.T) error {
	t.Helper()
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer close(release)

	client := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 10 * time.Millisecond}}
	_, err := client.Get(upstream.URL)

	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Contains(t, err.Error(), "timeout awaiting response headers")
	return err
}

func TestDispatchWithFallback_PrimarySucceedsNoRetry(t *testing.T) {
	primary := &fakeClient{name: "fireworks", outcomes: []fakeOutcome{{writeBytes: []byte("ok")}}}
	fallback := &fakeClient{name: "openrouter"} // should never be called

	s := newServiceWithProviders(t, map[string]providers.Client{
		"fireworks":  primary,
		"openrouter": fallback,
	})

	rec := httptest.NewRecorder()
	buf := newPreludeBuffer(rec)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	winnerIdx, err := s.dispatchWithFallback(context.Background(), failoverInputs{
		w:               rec,
		buf:             buf,
		initialDecision: router.Decision{Model: "deepseek/deepseek-v4-pro"},
		bindings: []catalog.ProviderBinding{
			{Provider: "fireworks"},
			{Provider: "openrouter"},
		},
		attempt: func(ctx context.Context, d router.Decision, p providers.Client) error {
			// Production closures call buf.Seal() between the Prelude
			// phase and the upstream call. These tests have no Prelude,
			// so Seal happens immediately before p.Proxy.
			buf.Seal()
			return p.Proxy(ctx, d, providers.PreparedRequest{}, buf, r)
		},
	})

	require.NoError(t, err)
	assert.Equal(t, 0, winnerIdx, "primary must win on success")
	assert.Equal(t, 1, primary.calls, "primary called exactly once")
	assert.Equal(t, 0, fallback.calls, "fallback must not be called when primary succeeds")
	assert.Equal(t, "ok", rec.Body.String())
}

func TestDispatchWithFallback_RetriesOnRetryableBufferedError(t *testing.T) {
	primary := &fakeClient{
		name:     "fireworks",
		outcomes: []fakeOutcome{{err: &providers.UpstreamErrorResponse{Status: 503, Body: []byte(`fireworks down`)}}},
	}
	fallback := &fakeClient{
		name:     "openrouter",
		outcomes: []fakeOutcome{{writeBytes: []byte("rescued")}},
	}

	s := newServiceWithProviders(t, map[string]providers.Client{
		"fireworks":  primary,
		"openrouter": fallback,
	})

	rec := httptest.NewRecorder()
	buf := newPreludeBuffer(rec)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	winnerIdx, err := s.dispatchWithFallback(context.Background(), failoverInputs{
		w:               rec,
		buf:             buf,
		initialDecision: router.Decision{Model: "deepseek/deepseek-v4-pro"},
		bindings: []catalog.ProviderBinding{
			{Provider: "fireworks"},
			{Provider: "openrouter"},
		},
		attempt: func(ctx context.Context, d router.Decision, p providers.Client) error {
			// Production closures call buf.Seal() between the Prelude
			// phase and the upstream call. These tests have no Prelude,
			// so Seal happens immediately before p.Proxy.
			buf.Seal()
			return p.Proxy(ctx, d, providers.PreparedRequest{}, buf, r)
		},
	})

	require.NoError(t, err, "fallback should succeed cleanly")
	assert.Equal(t, 1, winnerIdx, "fallback (index 1) must win")
	assert.Equal(t, 1, primary.calls)
	assert.Equal(t, 1, fallback.calls)
	assert.Equal(t, "rescued", rec.Body.String(), "client sees only the fallback's successful bytes")
	assert.Equal(t, "fireworks", rec.Header().Get(HeaderRouterFallbackFrom))
}

func TestDispatchWithFallback_RetriesOnTransportError(t *testing.T) {
	primary := &fakeClient{
		name:     "fireworks",
		outcomes: []fakeOutcome{{err: errors.New("upstream call: dial tcp: i/o timeout")}},
	}
	fallback := &fakeClient{
		name:     "openrouter",
		outcomes: []fakeOutcome{{writeBytes: []byte("rescued")}},
	}

	s := newServiceWithProviders(t, map[string]providers.Client{
		"fireworks":  primary,
		"openrouter": fallback,
	})

	rec := httptest.NewRecorder()
	buf := newPreludeBuffer(rec)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	winnerIdx, err := s.dispatchWithFallback(context.Background(), failoverInputs{
		w:               rec,
		buf:             buf,
		initialDecision: router.Decision{Model: "deepseek/deepseek-v4-pro"},
		bindings: []catalog.ProviderBinding{
			{Provider: "fireworks"},
			{Provider: "openrouter"},
		},
		attempt: func(ctx context.Context, d router.Decision, p providers.Client) error {
			// Production closures call buf.Seal() between the Prelude
			// phase and the upstream call. These tests have no Prelude,
			// so Seal happens immediately before p.Proxy.
			buf.Seal()
			return p.Proxy(ctx, d, providers.PreparedRequest{}, buf, r)
		},
	})

	require.NoError(t, err)
	assert.Equal(t, 1, winnerIdx)
	assert.Equal(t, 1, primary.calls)
	assert.Equal(t, 1, fallback.calls)
}

func TestDispatchWithFallback_RetriesOnResponseHeaderTimeout(t *testing.T) {
	primary := &fakeClient{
		name:     "deepinfra",
		outcomes: []fakeOutcome{{err: responseHeaderTimeoutErr(t)}},
	}
	fallback := &fakeClient{
		name:     "openrouter",
		outcomes: []fakeOutcome{{writeBytes: []byte("rescued")}},
	}

	s := newServiceWithProviders(t, map[string]providers.Client{
		providers.ProviderDeepInfra:  primary,
		providers.ProviderOpenRouter: fallback,
	})

	rec := httptest.NewRecorder()
	buf := newPreludeBuffer(rec)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	winnerIdx, err := s.dispatchWithFallback(context.Background(), failoverInputs{
		w:               rec,
		buf:             buf,
		initialDecision: router.Decision{Model: "deepseek/deepseek-v4-flash"},
		bindings: []catalog.ProviderBinding{
			{Provider: providers.ProviderDeepInfra},
			{Provider: providers.ProviderOpenRouter},
		},
		attempt: func(ctx context.Context, d router.Decision, p providers.Client) error {
			buf.Seal()
			return p.Proxy(ctx, d, providers.PreparedRequest{}, buf, r)
		},
	})

	require.NoError(t, err)
	assert.Equal(t, 1, winnerIdx)
	assert.Equal(t, 1, primary.calls)
	assert.Equal(t, 1, fallback.calls)
	assert.Equal(t, "rescued", rec.Body.String())
	assert.Equal(t, providers.ProviderDeepInfra, rec.Header().Get(HeaderRouterFallbackFrom))
}

// TestDispatchWithFallback_RetriesOnUpstreamIdleTimeout covers the mid-stream
// stall watchdog (prod incident 2026-06-09: /v1/responses streams went silent
// after headers, zero output tokens until the 600s cap). The adapter aborts
// the stalled stream and returns providers.ErrUpstreamIdleTimeout without
// having written anything through the prelude buffer, so the dispatch loop
// must rescue the turn on the next binding.
func TestDispatchWithFallback_RetriesOnUpstreamIdleTimeout(t *testing.T) {
	primary := &fakeClient{
		name:     "openai",
		outcomes: []fakeOutcome{{err: providers.ErrUpstreamIdleTimeout}},
	}
	fallback := &fakeClient{
		name:     "openrouter",
		outcomes: []fakeOutcome{{writeBytes: []byte("rescued")}},
	}

	s := newServiceWithProviders(t, map[string]providers.Client{
		providers.ProviderOpenAI:     primary,
		providers.ProviderOpenRouter: fallback,
	})

	rec := httptest.NewRecorder()
	buf := newPreludeBuffer(rec)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	winnerIdx, err := s.dispatchWithFallback(context.Background(), failoverInputs{
		w:               rec,
		buf:             buf,
		initialDecision: router.Decision{Model: "gpt-5.5"},
		bindings: []catalog.ProviderBinding{
			{Provider: providers.ProviderOpenAI},
			{Provider: providers.ProviderOpenRouter},
		},
		attempt: func(ctx context.Context, d router.Decision, p providers.Client) error {
			buf.Seal()
			return p.Proxy(ctx, d, providers.PreparedRequest{}, buf, r)
		},
	})

	require.NoError(t, err)
	assert.Equal(t, 1, winnerIdx)
	assert.Equal(t, 1, primary.calls)
	assert.Equal(t, 1, fallback.calls)
	assert.Equal(t, "rescued", rec.Body.String())
	assert.Equal(t, providers.ProviderOpenAI, rec.Header().Get(HeaderRouterFallbackFrom))
}

func TestDispatchWithFallback_NoRetryOnNonRetryableStatus(t *testing.T) {
	primary := &fakeClient{
		name:     "fireworks",
		outcomes: []fakeOutcome{{err: &providers.UpstreamErrorResponse{Status: 400, Body: []byte(`bad model`)}}},
	}
	fallback := &fakeClient{name: "openrouter"}

	s := newServiceWithProviders(t, map[string]providers.Client{
		"fireworks":  primary,
		"openrouter": fallback,
	})

	rec := httptest.NewRecorder()
	buf := newPreludeBuffer(rec)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	winnerIdx, err := s.dispatchWithFallback(context.Background(), failoverInputs{
		w:               rec,
		buf:             buf,
		initialDecision: router.Decision{Model: "deepseek/deepseek-v4-pro"},
		bindings: []catalog.ProviderBinding{
			{Provider: "fireworks"},
			{Provider: "openrouter"},
		},
		attempt: func(ctx context.Context, d router.Decision, p providers.Client) error {
			// Production closures call buf.Seal() between the Prelude
			// phase and the upstream call. These tests have no Prelude,
			// so Seal happens immediately before p.Proxy.
			buf.Seal()
			return p.Proxy(ctx, d, providers.PreparedRequest{}, buf, r)
		},
		flushErr: flushBufferedIfPresent,
	})

	require.Error(t, err)
	assert.Equal(t, 0, winnerIdx, "primary stays the winner — no retry on 400")
	assert.Equal(t, 0, fallback.calls, "fallback must not be called on non-retryable error")
	// 400's buffered body must reach the client since we won't retry.
	assert.Equal(t, "bad model", rec.Body.String())
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDispatchWithFallback_ModelNotFoundFailsOverToNextBinding(t *testing.T) {
	// A 404 means the primary provider doesn't serve this model (stale/wrong
	// upstream id). It is NOT in IsRetryable, so it must not retry the same
	// binding — but a different provider binding may carry the model, so the
	// dispatcher should fail over rather than hard-fail the turn.
	primary := &fakeClient{
		name:     "bedrock",
		outcomes: []fakeOutcome{{err: &providers.UpstreamErrorResponse{Status: 404, Body: []byte(`{"message":"model does not exist"}`)}}},
	}
	fallback := &fakeClient{
		name:     "openrouter",
		outcomes: []fakeOutcome{{writeBytes: []byte("rescued")}},
	}

	s := newServiceWithProviders(t, map[string]providers.Client{
		"bedrock":    primary,
		"openrouter": fallback,
	})

	rec := httptest.NewRecorder()
	buf := newPreludeBuffer(rec)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	winnerIdx, err := s.dispatchWithFallback(context.Background(), failoverInputs{
		w:               rec,
		buf:             buf,
		initialDecision: router.Decision{Model: "qwen/qwen3-next-80b-a3b-instruct"},
		bindings: []catalog.ProviderBinding{
			{Provider: "bedrock"},
			{Provider: "openrouter"},
		},
		attempt: func(ctx context.Context, d router.Decision, p providers.Client) error {
			buf.Seal()
			return p.Proxy(ctx, d, providers.PreparedRequest{}, buf, r)
		},
		flushErr: flushBufferedIfPresent,
	})

	require.NoError(t, err, "404 on primary must fail over to the next binding")
	assert.Equal(t, 1, winnerIdx, "fallback (index 1) wins")
	assert.Equal(t, 1, primary.calls, "primary tried once — a 404 must not trigger same-binding retry")
	assert.Equal(t, 1, fallback.calls)
	assert.Equal(t, "rescued", rec.Body.String())
}

func TestDispatchWithFallback_ModelNotFoundSingleBindingFlushes(t *testing.T) {
	// 404 on the only binding: no provider to fail over to, and a 404 must
	// not same-binding-retry (futile). The 404 envelope flushes after one try.
	only := &fakeClient{
		name:     "bedrock",
		outcomes: []fakeOutcome{{err: &providers.UpstreamErrorResponse{Status: 404, Body: []byte(`nope`)}}},
	}

	s := newServiceWithProviders(t, map[string]providers.Client{"bedrock": only})

	rec := httptest.NewRecorder()
	buf := newPreludeBuffer(rec)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	_, err := s.dispatchWithFallback(context.Background(), failoverInputs{
		w:               rec,
		buf:             buf,
		initialDecision: router.Decision{Model: "qwen/qwen3-next-80b-a3b-instruct"},
		bindings:        []catalog.ProviderBinding{{Provider: "bedrock"}},
		attempt: func(ctx context.Context, d router.Decision, p providers.Client) error {
			buf.Seal()
			return p.Proxy(ctx, d, providers.PreparedRequest{}, buf, r)
		},
		flushErr: flushBufferedIfPresent,
	})

	require.Error(t, err)
	assert.Equal(t, 1, only.calls, "404 must not same-binding-retry")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestDispatchWithFallback_NoRetryAfterBytesFlushed(t *testing.T) {
	// Primary writes some bytes, *then* errors. Even though the error is
	// retryable in isolation, the dispatcher can't retry — partial SSE is
	// already on the wire and the client is committed.
	primary := &fakeClient{
		name: "fireworks",
		outcomes: []fakeOutcome{{
			writeBytes: []byte("event: message_start\n\n"),
			err:        errors.New("connection reset mid-stream"),
		}},
	}
	fallback := &fakeClient{name: "openrouter"}

	s := newServiceWithProviders(t, map[string]providers.Client{
		"fireworks":  primary,
		"openrouter": fallback,
	})

	rec := httptest.NewRecorder()
	buf := newPreludeBuffer(rec)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	winnerIdx, err := s.dispatchWithFallback(context.Background(), failoverInputs{
		w:               rec,
		buf:             buf,
		initialDecision: router.Decision{Model: "deepseek/deepseek-v4-pro"},
		bindings: []catalog.ProviderBinding{
			{Provider: "fireworks"},
			{Provider: "openrouter"},
		},
		attempt: func(ctx context.Context, d router.Decision, p providers.Client) error {
			// Production closures call buf.Seal() between the Prelude
			// phase and the upstream call. These tests have no Prelude,
			// so Seal happens immediately before p.Proxy.
			buf.Seal()
			return p.Proxy(ctx, d, providers.PreparedRequest{}, buf, r)
		},
	})

	require.Error(t, err)
	assert.Equal(t, 0, winnerIdx)
	assert.Equal(t, 0, fallback.calls, "must not retry once bytes have been flushed to the client")
}

func TestDispatchWithFallback_BothFailFinalBodyFlushed(t *testing.T) {
	primary := &fakeClient{
		name:     "fireworks",
		outcomes: []fakeOutcome{{err: &providers.UpstreamErrorResponse{Status: 503, Body: []byte(`primary down`)}}},
	}
	fallback := &fakeClient{
		name:     "openrouter",
		outcomes: []fakeOutcome{{err: &providers.UpstreamErrorResponse{Status: 502, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`fallback also down`)}}},
	}

	s := newServiceWithProviders(t, map[string]providers.Client{
		"fireworks":  primary,
		"openrouter": fallback,
	})

	rec := httptest.NewRecorder()
	buf := newPreludeBuffer(rec)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	winnerIdx, err := s.dispatchWithFallback(context.Background(), failoverInputs{
		w:               rec,
		buf:             buf,
		initialDecision: router.Decision{Model: "deepseek/deepseek-v4-pro"},
		bindings: []catalog.ProviderBinding{
			{Provider: "fireworks"},
			{Provider: "openrouter"},
		},
		attempt: func(ctx context.Context, d router.Decision, p providers.Client) error {
			// Production closures call buf.Seal() between the Prelude
			// phase and the upstream call. These tests have no Prelude,
			// so Seal happens immediately before p.Proxy.
			buf.Seal()
			return p.Proxy(ctx, d, providers.PreparedRequest{}, buf, r)
		},
		flushErr: flushBufferedIfPresent,
	})

	require.Error(t, err)
	assert.Equal(t, 1, winnerIdx, "last-attempted index returned even on failure")
	// Client sees the FINAL upstream's error envelope, not the primary's.
	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Equal(t, "fallback also down", rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
}

// noopSleep is the injected backoff for retry tests — keeps them fast and
// deterministic while still driving the same-binding retry loop.
func noopSleep(context.Context, time.Duration) error { return nil }

func TestDispatchWithFallback_SingleBindingExhaustsRetries(t *testing.T) {
	// A single-binding model (Anthropic/OpenAI/Google) has no provider to
	// fail over to, so a persistent retryable error is retried in place up
	// to maxSameBindingRetries before the buffered envelope flushes.
	only := &fakeClient{
		name: "anthropic",
		outcomes: []fakeOutcome{
			{err: &providers.UpstreamErrorResponse{Status: 503, Body: []byte(`down`)}},
			{err: &providers.UpstreamErrorResponse{Status: 503, Body: []byte(`down`)}},
			{err: &providers.UpstreamErrorResponse{Status: 503, Body: []byte(`down`)}},
		},
	}

	s := newServiceWithProviders(t, map[string]providers.Client{"anthropic": only})
	s.retrySleep = noopSleep

	rec := httptest.NewRecorder()
	buf := newPreludeBuffer(rec)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	winnerIdx, err := s.dispatchWithFallback(context.Background(), failoverInputs{
		w:               rec,
		buf:             buf,
		initialDecision: router.Decision{Model: "claude-opus-4-7"},
		bindings:        []catalog.ProviderBinding{{Provider: "anthropic"}},
		attempt: func(ctx context.Context, d router.Decision, p providers.Client) error {
			buf.Seal()
			return p.Proxy(ctx, d, providers.PreparedRequest{}, buf, r)
		},
		flushErr: flushBufferedIfPresent,
	})

	require.Error(t, err)
	assert.Equal(t, 0, winnerIdx)
	assert.Equal(t, 1+maxSameBindingRetries, only.calls, "initial attempt + maxSameBindingRetries")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "final upstream envelope still flushes on exhaustion")
}

func TestDispatchWithFallback_SingleBindingRetrySucceeds(t *testing.T) {
	// Transient blip on the sole provider clears on retry — the turn is
	// rescued instead of dying with a 503.
	only := &fakeClient{
		name: "anthropic",
		outcomes: []fakeOutcome{
			{err: &providers.UpstreamErrorResponse{Status: 503, Body: []byte(`blip`)}},
			{writeBytes: []byte("rescued")},
		},
	}

	s := newServiceWithProviders(t, map[string]providers.Client{"anthropic": only})
	s.retrySleep = noopSleep

	rec := httptest.NewRecorder()
	buf := newPreludeBuffer(rec)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	winnerIdx, err := s.dispatchWithFallback(context.Background(), failoverInputs{
		w:               rec,
		buf:             buf,
		initialDecision: router.Decision{Model: "claude-opus-4-7"},
		bindings:        []catalog.ProviderBinding{{Provider: "anthropic"}},
		attempt: func(ctx context.Context, d router.Decision, p providers.Client) error {
			buf.Seal()
			return p.Proxy(ctx, d, providers.PreparedRequest{}, buf, r)
		},
		flushErr: flushBufferedIfPresent,
	})

	require.NoError(t, err)
	assert.Equal(t, 0, winnerIdx)
	assert.Equal(t, 2, only.calls, "one failure + one successful retry")
	assert.Equal(t, "rescued", rec.Body.String(), "client sees only the successful retry's bytes")
}

func TestDispatchWithFallback_SingleBindingNonRetryableNoRetry(t *testing.T) {
	// A 4xx is the model/request's fault, not a transient blip — retrying
	// the same binding would just burn latency. Fail fast.
	only := &fakeClient{
		name:     "anthropic",
		outcomes: []fakeOutcome{{err: &providers.UpstreamErrorResponse{Status: 400, Body: []byte(`bad`)}}},
	}

	s := newServiceWithProviders(t, map[string]providers.Client{"anthropic": only})
	s.retrySleep = noopSleep

	rec := httptest.NewRecorder()
	buf := newPreludeBuffer(rec)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	_, err := s.dispatchWithFallback(context.Background(), failoverInputs{
		w:               rec,
		buf:             buf,
		initialDecision: router.Decision{Model: "claude-opus-4-7"},
		bindings:        []catalog.ProviderBinding{{Provider: "anthropic"}},
		attempt: func(ctx context.Context, d router.Decision, p providers.Client) error {
			buf.Seal()
			return p.Proxy(ctx, d, providers.PreparedRequest{}, buf, r)
		},
		flushErr: flushBufferedIfPresent,
	})

	require.Error(t, err)
	assert.Equal(t, 1, only.calls, "non-retryable status must not retry")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDispatchWithFallback_SingleBindingBackoffAbortsOnCancel(t *testing.T) {
	// If the client disconnects (ctx canceled) during backoff, the loop
	// stops rather than burning the next attempt.
	only := &fakeClient{
		name: "anthropic",
		outcomes: []fakeOutcome{
			{err: &providers.UpstreamErrorResponse{Status: 503, Body: []byte(`down`)}},
			{err: &providers.UpstreamErrorResponse{Status: 503, Body: []byte(`down`)}},
		},
	}

	s := newServiceWithProviders(t, map[string]providers.Client{"anthropic": only})
	s.retrySleep = func(context.Context, time.Duration) error { return context.Canceled }

	rec := httptest.NewRecorder()
	buf := newPreludeBuffer(rec)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	_, err := s.dispatchWithFallback(context.Background(), failoverInputs{
		w:               rec,
		buf:             buf,
		initialDecision: router.Decision{Model: "claude-opus-4-7"},
		bindings:        []catalog.ProviderBinding{{Provider: "anthropic"}},
		attempt: func(ctx context.Context, d router.Decision, p providers.Client) error {
			buf.Seal()
			return p.Proxy(ctx, d, providers.PreparedRequest{}, buf, r)
		},
		flushErr: flushBufferedIfPresent,
	})

	require.Error(t, err)
	assert.Equal(t, 1, only.calls, "backoff abort stops before the second attempt")
}

func TestSameBindingBackoff(t *testing.T) {
	assert.Equal(t, 250*time.Millisecond, sameBindingBackoff(0))
	assert.Equal(t, 500*time.Millisecond, sameBindingBackoff(1))
}

func TestSleepWithContext(t *testing.T) {
	assert.NoError(t, sleepWithContext(context.Background(), time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, sleepWithContext(ctx, time.Hour), context.Canceled)
}

func TestShouldFailover(t *testing.T) {
	s := &Service{}
	t.Run("clean context allows failover", func(t *testing.T) {
		assert.True(t, s.shouldFailover(context.Background()))
	})
	t.Run("BYOK-only deployment skips failover", func(t *testing.T) {
		s := &Service{byokOnly: true}
		assert.False(t, s.shouldFailover(context.Background()))
	})
	t.Run("inbound credentials in context skip failover", func(t *testing.T) {
		s := &Service{}
		ctx := context.WithValue(context.Background(), CredentialsContextKey{}, &Credentials{APIKey: []byte("sk-byok"), Source: "byok"})
		assert.False(t, s.shouldFailover(ctx))
	})
	t.Run("subscription OAuth credential skips failover", func(t *testing.T) {
		// A subscription token authenticates only against Anthropic; failing
		// over to another vendor would 401, so it must bind to one provider.
		s := &Service{}
		ctx := context.WithValue(context.Background(), CredentialsContextKey{},
			&Credentials{APIKey: []byte("sk-ant-oat01-token"), Source: "subscription", OAuth: true})
		assert.False(t, s.shouldFailover(ctx))
	})
}

func TestResolveBindingsForDispatch(t *testing.T) {
	t.Run("BYOK active returns single primary", func(t *testing.T) {
		s := &Service{}
		ctx := context.WithValue(context.Background(), CredentialsContextKey{}, &Credentials{APIKey: []byte("k"), Source: "client"})
		bs := s.resolveBindingsForDispatch(ctx, router.Decision{Model: "deepseek/deepseek-v4-pro", Provider: "fireworks"})
		require.Len(t, bs, 1)
		assert.Equal(t, "fireworks", bs[0].Provider)
	})
	t.Run("nil deploymentKeyedProviders falls back to single attempt", func(t *testing.T) {
		s := &Service{} // deploymentKeyedProviders == nil
		bs := s.resolveBindingsForDispatch(context.Background(), router.Decision{Model: "deepseek/deepseek-v4-pro", Provider: "fireworks"})
		require.Len(t, bs, 1, "legacy 'all registered' mode disables failover to avoid retrying on unwired providers")
		assert.Equal(t, "fireworks", bs[0].Provider)
	})
	t.Run("multi-binding model with both keys returns ordered list", func(t *testing.T) {
		s := &Service{deploymentKeyedProviders: map[string]struct{}{"fireworks": {}, "openrouter": {}}}
		bs := s.resolveBindingsForDispatch(context.Background(), router.Decision{Model: "deepseek/deepseek-v4-pro", Provider: "fireworks"})
		require.GreaterOrEqual(t, len(bs), 2, "deepseek/deepseek-v4-pro must have at least 2 bindings in catalog")
		assert.Equal(t, "fireworks", bs[0].Provider, "catalog order: fireworks primary")
		assert.Equal(t, "openrouter", bs[1].Provider, "catalog order: openrouter fallback")
	})
	t.Run("single-binding Anthropic model returns one binding even with multiple keys wired", func(t *testing.T) {
		s := &Service{deploymentKeyedProviders: map[string]struct{}{"anthropic": {}, "openrouter": {}}}
		bs := s.resolveBindingsForDispatch(context.Background(), router.Decision{Model: "claude-opus-4-7", Provider: "anthropic"})
		require.Len(t, bs, 1)
		assert.Equal(t, "anthropic", bs[0].Provider)
	})
	t.Run("excluded provider cannot be resurrected as a fallback binding", func(t *testing.T) {
		s := &Service{deploymentKeyedProviders: map[string]struct{}{"fireworks": {}, "openrouter": {}}}
		ctx := context.WithValue(context.Background(), InstallationExcludedProvidersContextKey{}, []string{"openrouter"})
		bs := s.resolveBindingsForDispatch(ctx, router.Decision{Model: "deepseek/deepseek-v4-pro", Provider: "fireworks"})
		require.Len(t, bs, 1, "openrouter fallback binding must be filtered out by the provider exclusion")
		assert.Equal(t, "fireworks", bs[0].Provider)
	})
	t.Run("all bindings excluded with excluded primary returns empty walk", func(t *testing.T) {
		// When exclusion filters out every binding AND the decision names an
		// excluded provider, the walk must be empty so dispatchWithFallback
		// 502s instead of serving the forbidden provider.
		s := &Service{deploymentKeyedProviders: map[string]struct{}{"fireworks": {}, "openrouter": {}}}
		ctx := context.WithValue(context.Background(), InstallationExcludedProvidersContextKey{}, []string{"fireworks", "openrouter"})
		bs := s.resolveBindingsForDispatch(ctx, router.Decision{Model: "deepseek/deepseek-v4-pro", Provider: "fireworks"})
		assert.Empty(t, bs, "no eligible binding may remain when the primary itself is excluded")
	})
	t.Run("catalog-miss model keeps legacy single-attempt primary", func(t *testing.T) {
		// A model with no catalog bindings (zero before any filtering) must
		// keep the legacy [primary] walk when the primary is not excluded.
		s := &Service{deploymentKeyedProviders: map[string]struct{}{"anthropic": {}}}
		ctx := context.WithValue(context.Background(), InstallationExcludedProvidersContextKey{}, []string{"openrouter"})
		bs := s.resolveBindingsForDispatch(ctx, router.Decision{Model: "not-in-catalog", Provider: "anthropic"})
		require.Len(t, bs, 1)
		assert.Equal(t, "anthropic", bs[0].Provider)
	})
	t.Run("excluded primary is never re-added as the first attempt", func(t *testing.T) {
		// Defense in depth: routing already filters excluded providers, so a
		// decision naming one is an upstream bug — dispatch must serve only
		// the eligible bindings rather than re-prepending the excluded
		// primary.
		s := &Service{deploymentKeyedProviders: map[string]struct{}{"fireworks": {}, "openrouter": {}}}
		ctx := context.WithValue(context.Background(), InstallationExcludedProvidersContextKey{}, []string{"fireworks"})
		bs := s.resolveBindingsForDispatch(ctx, router.Decision{Model: "deepseek/deepseek-v4-pro", Provider: "fireworks"})
		require.NotEmpty(t, bs)
		for _, b := range bs {
			assert.NotEqual(t, "fireworks", b.Provider,
				"an excluded provider must not appear anywhere in the dispatch walk")
		}
	})
}

// TestProvidersIsRetryable round-trips the dispatcher's classifier
// against the inputs it'll actually see in production.
func TestProvidersIsRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"buffered 503", &providers.UpstreamErrorResponse{Status: 503}, true},
		{"buffered 502", &providers.UpstreamErrorResponse{Status: 502}, true},
		{"buffered 429", &providers.UpstreamErrorResponse{Status: 429}, true},
		{"buffered 408", &providers.UpstreamErrorResponse{Status: 408}, true},
		{"buffered 400", &providers.UpstreamErrorResponse{Status: 400}, false},
		{"buffered 401", &providers.UpstreamErrorResponse{Status: 401}, false},
		{"buffered 404 not retryable (handled via failover, not retry)", &providers.UpstreamErrorResponse{Status: 404}, false},
		{"flushed UpstreamStatusError 503", &providers.UpstreamStatusError{Status: 503}, false},
		{"transport error", errors.New("dial tcp: connection refused"), true},
		{"context.Canceled", context.Canceled, false},
		{"context.DeadlineExceeded", context.DeadlineExceeded, false},
		{"wrapped context.Canceled", fmt.Errorf("upstream call: %w", context.Canceled), false},
		{"wrapped context.DeadlineExceeded", fmt.Errorf("upstream call: %w", context.DeadlineExceeded), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, providers.IsRetryable(c.err))
		})
	}
}

func TestProvidersIsUpstreamModelNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"buffered 404", &providers.UpstreamErrorResponse{Status: 404}, true},
		{"buffered 503", &providers.UpstreamErrorResponse{Status: 503}, false},
		{"buffered 400", &providers.UpstreamErrorResponse{Status: 400}, false},
		{"flushed 404 (already on wire)", &providers.UpstreamStatusError{Status: 404}, false},
		{"transport error", errors.New("dial tcp"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, providers.IsUpstreamModelNotFound(c.err))
		})
	}
}

// TestPreludeBuffer_BuffersUntilSealedFirstWrite asserts the core
// preludeBuffer contract: pre-Seal writes are buffered; the first
// post-Seal write triggers commit, ordering the buffered Prelude bytes
// ahead of the upstream content on the wire.
func TestPreludeBuffer_BuffersUntilSealedFirstWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/json") // simulate middleware default
	buf := newPreludeBuffer(rec)

	// Simulate translator.Prelude phase: set SSE content type, write status,
	// emit message_start.
	buf.Header().Set("Content-Type", "text/event-stream")
	buf.Header().Del("Content-Length")
	buf.WriteHeader(http.StatusOK)
	_, _ = buf.Write([]byte("event: message_start\n\n"))
	assert.False(t, buf.Committed(), "pre-seal writes do not commit")
	assert.Empty(t, rec.Body.String(), "inner writer untouched pre-seal")

	// Seal + first upstream chunk write.
	buf.Seal()
	_, _ = buf.Write([]byte("event: content_block_start\n\n"))
	assert.True(t, buf.Committed(), "post-seal write commits")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "event: message_start\n\nevent: content_block_start\n\n", rec.Body.String(),
		"buffered prelude flushed in order before the post-seal chunk")
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"),
		"Prelude's Content-Type override flushed with the commit")
}

// TestPreludeBuffer_DiscardResetsBufferAndHeaders asserts that Discard()
// drops buffered bytes and restores Header() to the construction-time
// snapshot, so a retry can begin with a pristine writer.
func TestPreludeBuffer_DiscardResetsBufferAndHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/json")
	rec.Header().Set("Content-Length", "0")
	buf := newPreludeBuffer(rec)

	// Attempt 1: Prelude writes + status + body.
	buf.Header().Set("Content-Type", "text/event-stream")
	buf.Header().Del("Content-Length")
	buf.Header().Set(HeaderRouterFallbackFrom, "fireworks")
	buf.WriteHeader(http.StatusOK)
	_, _ = buf.Write([]byte("attempt-1-prelude"))

	// Primary errored before any bytes flushed: Discard.
	buf.Discard()

	assert.False(t, buf.Committed(), "Discard does not commit")
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"),
		"Content-Type restored to construction-time snapshot")
	assert.Equal(t, "0", rec.Header().Get("Content-Length"),
		"Content-Length deleted by Prelude restored")
	assert.Empty(t, rec.Header().Get(HeaderRouterFallbackFrom),
		"Prelude-added headers removed")

	// Attempt 2: Prelude + success.
	buf.Header().Set("Content-Type", "text/event-stream")
	buf.WriteHeader(http.StatusOK)
	_, _ = buf.Write([]byte("attempt-2-prelude"))
	buf.Seal()
	_, _ = buf.Write([]byte("first-upstream-byte"))

	assert.Equal(t, "attempt-2-preludefirst-upstream-byte", rec.Body.String(),
		"only attempt-2's bytes reach the client; attempt-1 was discarded")
}

// TestPreludeBuffer_NoOpFlushPreCommit asserts that Flush() does not
// reach the inner writer until commit fires. This is critical: a
// translator's Flush() call between Prelude writes and the upstream
// commit must not leak partial bytes to the client.
func TestPreludeBuffer_NoOpFlushPreCommit(t *testing.T) {
	flushCount := 0
	w := &fakeFlushTracker{ResponseRecorder: httptest.NewRecorder(), onFlush: func() { flushCount++ }}
	buf := newPreludeBuffer(w)

	buf.WriteHeader(http.StatusOK)
	_, _ = buf.Write([]byte("prelude"))
	buf.Flush()
	assert.Equal(t, 0, flushCount, "pre-commit Flush is a no-op")

	buf.Seal()
	_, _ = buf.Write([]byte("chunk")) // triggers commit
	assert.Equal(t, 1, flushCount, "commit flushes inner exactly once")

	buf.Flush()
	assert.Equal(t, 2, flushCount, "post-commit Flush passes through")
}

type fakeFlushTracker struct {
	*httptest.ResponseRecorder
	onFlush func()
}

func (f *fakeFlushTracker) Flush() { f.onFlush() }
