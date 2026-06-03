package proxy

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"workweave/router/internal/providers"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// evictionStubPinStore records every call to IncrementUpstreamErrors /
// ResetUpstreamErrors / Upsert so each two-strike branch can be
// asserted independently. The increment counter is configurable to
// drive the threshold path without re-running the whole turn loop.
type evictionStubPinStore struct {
	mu             sync.Mutex
	incrementCalls int
	incrementNext  []int // values returned by IncrementUpstreamErrors, in order
	resetCalls     int
	upserts        []sessionpin.Pin
}

func (s *evictionStubPinStore) Get(context.Context, [sessionpin.SessionKeyLen]byte, string) (sessionpin.Pin, bool, error) {
	return sessionpin.Pin{}, false, nil
}

func (s *evictionStubPinStore) Upsert(_ context.Context, p sessionpin.Pin) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upserts = append(s.upserts, p)
	return nil
}

func (s *evictionStubPinStore) UpdateUsage(context.Context, [sessionpin.SessionKeyLen]byte, string, sessionpin.Usage) error {
	return nil
}

func (s *evictionStubPinStore) IncrementUpstreamErrors(context.Context, [sessionpin.SessionKeyLen]byte, string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.incrementCalls++
	if len(s.incrementNext) == 0 {
		return 0, nil
	}
	v := s.incrementNext[0]
	s.incrementNext = s.incrementNext[1:]
	return v, nil
}

func (s *evictionStubPinStore) ResetUpstreamErrors(context.Context, [sessionpin.SessionKeyLen]byte, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resetCalls++
	return nil
}

func (s *evictionStubPinStore) SweepExpired(context.Context) error { return nil }

func newEvictionTestService(store *evictionStubPinStore) *Service {
	return NewService(
		nil,
		nil,
		nil,
		false,
		nil,
		store,
		false,
		"anthropic", "claude-haiku-4-5",
		nil,
	)
}

func nonZeroSessionKey() [sessionpin.SessionKeyLen]byte {
	var k [sessionpin.SessionKeyLen]byte
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

// TestMaybeEvictPin_FirstStrikeOnlyIncrements pins the two-strike
// invariant: a SINGLE 400 must increment the counter but NOT expire
// the pin. The eviction must wait for the second consecutive strike,
// so an isolated bad request (a one-off malformed prompt, brief
// upstream schema validation hiccup) doesn't flush an otherwise-warm
// session's cache.
func TestMaybeEvictPin_FirstStrikeOnlyIncrements(t *testing.T) {
	store := &evictionStubPinStore{incrementNext: []int{1}}
	svc := newEvictionTestService(store)

	upstreamErr := &providers.UpstreamErrorResponse{Status: http.StatusBadRequest}
	svc.maybeEvictPinAfterUpstreamErr(
		context.Background(),
		true, // stickyHit
		upstreamErr,
		"cluster:v0.57 model=gpt-5.5 provider=openai",
		uuid.New(),
		nonZeroSessionKey(),
		sessionpin.DefaultRole,
	)

	assert.Equal(t, 1, store.incrementCalls, "first 4xx must increment exactly once")
	assert.Equal(t, 0, store.resetCalls, "reset must not fire on a failed turn")
	assert.Empty(t, store.upserts, "first strike must not expire the pin — eviction waits for strike #2")
}

// TestMaybeEvictPin_SecondStrikeExpires guards the actual eviction
// path. The session 93e918bf wedge happened because there was NO
// eviction path at all; this test fails the moment that regression
// returns.
func TestMaybeEvictPin_SecondStrikeExpires(t *testing.T) {
	store := &evictionStubPinStore{incrementNext: []int{pinEvictionStrikeThreshold}}
	svc := newEvictionTestService(store)

	upstreamErr := &providers.UpstreamStatusError{Status: http.StatusBadRequest}
	installationID := uuid.New()
	sessionKey := nonZeroSessionKey()

	svc.maybeEvictPinAfterUpstreamErr(
		context.Background(),
		true,
		upstreamErr,
		"cluster:v0.57 model=gpt-5.5 provider=openai",
		installationID,
		sessionKey,
		sessionpin.DefaultRole,
	)

	require.Len(t, store.upserts, 1, "threshold strike must trigger an expired-pin upsert")
	expired := store.upserts[0]
	assert.Equal(t, sessionpin.DefaultRole, expired.Role)
	assert.Equal(t, installationID, expired.InstallationID)
	assert.Empty(t, expired.Provider, "expired pin must clear provider so loadPin discards it")
	assert.Empty(t, expired.Model, "expired pin must clear model so loadPin discards it")
	assert.True(t, expired.PinnedUntil.Before(time.Now()),
		"PinnedUntil must be in the past so loadPin's expiry check discards the row")
	assert.Equal(t, "upstream_error_strike_threshold", expired.Reason,
		"eviction reason is the audit trail that distinguishes this path from force-model / loop-break")
}

// TestMaybeEvictPin_SuccessResets verifies that a successful turn
// clears the counter so the strike count truly tracks CONSECUTIVE
// failures (not lifetime failures). Without this, a session could
// accumulate strikes across hours of healthy turns and trigger a
// stale eviction on a single bad request.
func TestMaybeEvictPin_SuccessResets(t *testing.T) {
	store := &evictionStubPinStore{}
	svc := newEvictionTestService(store)

	svc.maybeEvictPinAfterUpstreamErr(
		context.Background(),
		true,
		nil, // success
		"cluster:v0.57 model=gpt-5.5 provider=openai",
		uuid.New(),
		nonZeroSessionKey(),
		sessionpin.DefaultRole,
	)

	assert.Equal(t, 1, store.resetCalls, "successful turn on a sticky pin must clear the strike counter")
	assert.Equal(t, 0, store.incrementCalls)
	assert.Empty(t, store.upserts)
}

// TestMaybeEvictPin_RetryableStatusIgnored guards the IsRetryableStatus
// gate. 429 / 408 / 5xx are upstream-transient and handled by
// dispatchWithFallback; they must not drain the strike counter, since
// a rate-limited request says nothing about whether the model itself
// is wedged.
func TestMaybeEvictPin_RetryableStatusIgnored(t *testing.T) {
	for _, status := range []int{http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable} {
		store := &evictionStubPinStore{incrementNext: []int{99}}
		svc := newEvictionTestService(store)

		svc.maybeEvictPinAfterUpstreamErr(
			context.Background(),
			true,
			&providers.UpstreamErrorResponse{Status: status},
			"cluster:v0.57 model=gpt-5.5 provider=openai",
			uuid.New(),
			nonZeroSessionKey(),
			sessionpin.DefaultRole,
		)

		assert.Zero(t, store.incrementCalls, "status %d is retryable and must not bump the strike counter", status)
		assert.Empty(t, store.upserts, "status %d must not trigger eviction", status)
	}
}

// TestMaybeEvictPin_UserForcedSkipped pins the user-command override:
// a force-model'd session is the user explicitly choosing this
// provider/model, and auto-eviction would silently revert that
// command on the next turn. /unforce-model remains the escape hatch.
// Covers both the plain reason and the "+tier_clamp" suffix variant.
func TestMaybeEvictPin_UserForcedSkipped(t *testing.T) {
	for _, reason := range []string{translate.ReasonUserForceModel, translate.ReasonUserForceModel + "+tier_clamp"} {
		store := &evictionStubPinStore{incrementNext: []int{pinEvictionStrikeThreshold}}
		svc := newEvictionTestService(store)

		svc.maybeEvictPinAfterUpstreamErr(
			context.Background(),
			true,
			&providers.UpstreamErrorResponse{Status: http.StatusBadRequest},
			reason,
			uuid.New(),
			nonZeroSessionKey(),
			sessionpin.DefaultRole,
		)

		assert.Zero(t, store.incrementCalls, "user-forced pins (%q) must skip the counter increment", reason)
		assert.Zero(t, store.resetCalls)
		assert.Empty(t, store.upserts, "user-forced pins must never be auto-evicted (%q)", reason)
	}
}

// TestMaybeEvictPin_NoStickyHitSkipped verifies the cheap fast-path:
// when this turn freshly routed via the scorer (no sticky pin), there
// is no prior decision to second-guess. The Upsert that just wrote
// this turn's pin already resets the counter via the SQL CASE clause,
// so any reset call here would be redundant work.
func TestMaybeEvictPin_NoStickyHitSkipped(t *testing.T) {
	store := &evictionStubPinStore{incrementNext: []int{pinEvictionStrikeThreshold}}
	svc := newEvictionTestService(store)

	svc.maybeEvictPinAfterUpstreamErr(
		context.Background(),
		false, // !stickyHit
		&providers.UpstreamErrorResponse{Status: http.StatusBadRequest},
		"cluster:v0.57 model=gpt-5.5 provider=openai",
		uuid.New(),
		nonZeroSessionKey(),
		sessionpin.DefaultRole,
	)

	assert.Zero(t, store.incrementCalls)
	assert.Zero(t, store.resetCalls)
	assert.Empty(t, store.upserts)
}

// TestMaybeEvictPin_ZeroSessionKeySkipped guards against a corrupted
// pin row shared by every zero-keyed caller — mirrors the same guard
// in no_progress.go.
func TestMaybeEvictPin_ZeroSessionKeySkipped(t *testing.T) {
	store := &evictionStubPinStore{incrementNext: []int{pinEvictionStrikeThreshold}}
	svc := newEvictionTestService(store)

	svc.maybeEvictPinAfterUpstreamErr(
		context.Background(),
		true,
		&providers.UpstreamErrorResponse{Status: http.StatusBadRequest},
		"cluster:v0.57 model=gpt-5.5 provider=openai",
		uuid.New(),
		[sessionpin.SessionKeyLen]byte{}, // zero key
		sessionpin.DefaultRole,
	)

	assert.Zero(t, store.incrementCalls)
	assert.Empty(t, store.upserts)
}

// TestMaybeEvictPin_NilInstallationSkipped covers the unauthenticated
// path (no installation_id resolved). Selfhosted / fakes / dev
// scenarios hit this; the eviction must no-op rather than write a
// uuid.Nil-installed row to Postgres.
func TestMaybeEvictPin_NilInstallationSkipped(t *testing.T) {
	store := &evictionStubPinStore{incrementNext: []int{pinEvictionStrikeThreshold}}
	svc := newEvictionTestService(store)

	svc.maybeEvictPinAfterUpstreamErr(
		context.Background(),
		true,
		&providers.UpstreamErrorResponse{Status: http.StatusBadRequest},
		"cluster:v0.57 model=gpt-5.5 provider=openai",
		uuid.Nil,
		nonZeroSessionKey(),
		sessionpin.DefaultRole,
	)

	assert.Zero(t, store.incrementCalls)
	assert.Empty(t, store.upserts)
}

// TestMaybeEvictPin_NonUpstreamErrorIgnored guards the upstreamStatus
// gate: a generic transport / build / context-cancel error has no
// upstream status and is not a model-quality signal, so the counter
// must NOT advance.
func TestMaybeEvictPin_NonUpstreamErrorIgnored(t *testing.T) {
	store := &evictionStubPinStore{incrementNext: []int{pinEvictionStrikeThreshold}}
	svc := newEvictionTestService(store)

	svc.maybeEvictPinAfterUpstreamErr(
		context.Background(),
		true,
		errors.New("upstream call: dial tcp: connection refused"),
		"cluster:v0.57 model=gpt-5.5 provider=openai",
		uuid.New(),
		nonZeroSessionKey(),
		sessionpin.DefaultRole,
	)

	assert.Zero(t, store.incrementCalls)
	assert.Empty(t, store.upserts)
}
