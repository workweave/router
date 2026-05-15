// Package handover defines the contract for bounded-cost context handover
// when the planner switches models mid-session. The provider-backed
// implementation lives in internal/proxy; this package only declares the
// interface and envelope-rewrite helpers so inner-ring code can describe
// the operation without an I/O dependency.
//
// Why: switching mid-session forces a prompt-cache miss on the full prior
// context. Summarizing and replacing history with [summary, latestUser]
// bounds switch-turn input cost regardless of session length.
package handover

import (
	"context"

	"workweave/router/internal/translate"
)

// Summarizer produces a prose summary of the prior conversation for
// seeding a new model's context after a router switch.
//
// Implementations SHOULD respect the context deadline. On timeout or
// error, callers fall back to TrimLastN.
type Summarizer interface {
	Summarize(ctx context.Context, env *translate.RequestEnvelope) (summary string, err error)
}

// RewriteEnvelope mutates env in-place: keeps system blocks, replaces
// messages with [assistantSummary, latestUser]. Returns the number of
// original messages elided.
//
// Pure: no I/O. Returns 0 when env is nil or the message array is missing/empty.
func RewriteEnvelope(env *translate.RequestEnvelope, summary string) int {
	if env == nil {
		return 0
	}
	return env.RewriteForHandover(summary)
}

// TrimLastN is the graceful-degradation path when summarization fails or
// times out. Keeps the most recent n non-system messages plus system
// blocks; n <= 0 is treated as 3. Returns the number of messages elided.
//
// Pure: no I/O. Returns 0 when env is nil.
func TrimLastN(env *translate.RequestEnvelope, n int) int {
	if env == nil {
		return 0
	}
	return env.TrimLastNMessages(n)
}
