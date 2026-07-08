package proxy

import (
	"errors"
	"sync"
	"time"

	"github.com/sony/gobreaker/v2"
)

const (
	// breakerFailureThreshold trips a binding's breaker after this many
	// consecutive failed outcomes.
	breakerFailureThreshold = 5
	// breakerOpenTimeout is how long a tripped breaker stays open before
	// allowing a half-open probe.
	breakerOpenTimeout = 30 * time.Second
	// breakerHalfOpenProbes bounds concurrent half-open probes; the breaker
	// closes once this many probes succeed in a row.
	breakerHalfOpenProbes = 2
)

// errBreakerFailure is passed to gobreaker's done callback to record a
// failed outcome. gobreaker's default IsSuccessful treats any non-nil
// error as a failure and never inspects its value, so the identity/message
// here is irrelevant — recordOutcome already carries the real success bit.
var errBreakerFailure = errors.New("proxy: breaker recorded failure")

// breakerRegistry holds one TwoStepCircuitBreaker per (provider, model)
// binding, created lazily on first use, protected by mu. State is learned
// passively from live dispatch outcomes reported via recordOutcome — there
// is no health-probe loop.
//
// A nil *breakerRegistry is valid: allow always permits and records
// nothing, preserving pre-breaker behavior for &Service{} (most unit
// tests). NewService wires a real registry.
type breakerRegistry struct {
	mu       sync.Mutex
	breakers map[string]*gobreaker.TwoStepCircuitBreaker[any]
}

func newBreakerRegistry() *breakerRegistry {
	return &breakerRegistry{breakers: make(map[string]*gobreaker.TwoStepCircuitBreaker[any])}
}

// breakerKey identifies the (provider, model) binding a breaker tracks.
func breakerKey(provider, model string) string {
	return provider + "|" + model
}

// allow reports whether a dispatch attempt against key may proceed
// (breakerOpen=false), and returns the callback the caller must invoke
// exactly once with the attempt's outcome. recordOutcome is always safe to
// call: on a nil registry or a denied (breakerOpen=true) request it is a
// no-op, since no outcome should feed back into a breaker that never
// admitted the attempt.
func (r *breakerRegistry) allow(key string) (recordOutcome func(success bool), breakerOpen bool) {
	noop := func(bool) {}
	if r == nil {
		return noop, false
	}
	done, err := r.breakerFor(key).Allow()
	if err != nil {
		// TwoStepCircuitBreaker.Allow only errors when the breaker denies
		// entry (open, or half-open with its probe budget spent) — both
		// cases the caller should treat as "breaker open".
		return noop, true
	}
	return func(success bool) {
		if success {
			done(nil)
			return
		}
		done(errBreakerFailure)
	}, false
}

func (r *breakerRegistry) breakerFor(key string) *gobreaker.TwoStepCircuitBreaker[any] {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.breakers[key]; ok {
		return b
	}
	b := gobreaker.NewTwoStepCircuitBreaker[any](gobreaker.Settings{
		Name: key,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= breakerFailureThreshold
		},
		Timeout:     breakerOpenTimeout,
		MaxRequests: breakerHalfOpenProbes,
	})
	r.breakers[key] = b
	return b
}
