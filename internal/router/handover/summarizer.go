// Package handover defines the contract for bounded-cost context handover
// when the planner switches models mid-session. The adapter that calls a
// real provider lives in internal/proxy; this package only declares the
// interface and the envelope-rewrite helpers so inner-ring code can
// describe the operation without taking an I/O dependency.
//
// Why this exists: switching models mid-session forces the new model to
// take a one-time prompt-cache miss on the whole prior context. By
// summarizing the conversation and replacing the message history with
// [synthesizedSummary, latestUser], the switch turn's input cost is
// bounded regardless of how long the session has been running.
package handover

import (
	"context"

	"workweave/router/internal/translate"
)

// Summarizer produces a short prose summary of the prior conversation
// suitable to seed a new model's context after a router switch. Adapters
// implement this against a real LLM (default: claude-haiku-4-5).
//
// The summarizer SHOULD respect the context's deadline. On timeout or
// any error, callers fall back to TrimLastN — implementations return an
// error rather than blocking past the deadline.
type Summarizer interface {
	Summarize(ctx context.Context, env *translate.RequestEnvelope) (summary string, err error)
}

// RewriteEnvelope mutates env in-place: keeps the system block(s) and
// replaces all messages with [assistantSummary, latestUser]. The summary
// is wrapped with translate.HandoverSummaryTag so a reader can tell
// synthesized context from real assistant output.
//
// Returns the number of original messages that were elided so the
// caller can log it. The synthesized summary entry is not counted as
// elided.
//
// Pure: no I/O. Returns 0 and does nothing when env is nil, when the
// envelope's message array is missing, or when the message array is
// empty.
func RewriteEnvelope(env *translate.RequestEnvelope, summary string) int {
	if env == nil {
		return 0
	}
	return env.RewriteForHandover(summary)
}

// TrimLastN is the graceful-degradation path used when summarization
// fails or times out. Keeps the most recent n non-system messages plus
// the system block(s); n <= 0 is treated as 3. Returns the number of
// messages elided.
//
// Pure: no I/O. Returns 0 when env is nil.
func TrimLastN(env *translate.RequestEnvelope, n int) int {
	if env == nil {
		return 0
	}
	return env.TrimLastNMessages(n)
}
