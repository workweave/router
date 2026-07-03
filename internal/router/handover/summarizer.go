// Package handover declares the interface and envelope-rewrite helpers for
// bounded-cost context handover when the planner switches models mid-session
// (provider-backed implementation lives in internal/proxy).
//
// Switching mid-session forces a prompt-cache miss on the full prior context;
// replacing history with [summary, latestUser] bounds switch-turn input cost
// regardless of session length.
package handover

import (
	"context"

	"workweave/router/internal/translate"
)

// Usage captures the upstream token counts of a Summarize call so callers can
// bill the summary turn separately from the main inference debit. Zero
// values mean usage wasn't reported.
type Usage struct {
	InputTokens   int
	OutputTokens  int
	CacheCreation int
	CacheRead     int
	// Model and Provider identify the upstream the summarizer dispatched
	// to so the ledger row can record them. Empty means "not reported".
	Model    string
	Provider string
}

// Summarizer produces a prose summary of the prior conversation for seeding
// a new model's context after a router switch.
//
// Implementations SHOULD respect the context deadline; on timeout or error,
// callers keep the full prior history unchanged instead of dropping it.
//
// Provider identifies the upstream this summarizer dispatches to (e.g.
// "anthropic"), so the orchestrator can plumb matching BYOK creds through
// and keep tenant data from crossing the deployment key boundary.
type Summarizer interface {
	Summarize(ctx context.Context, env *translate.RequestEnvelope) (summary string, usage Usage, err error)
	Provider() string
}

// RewriteEnvelope mutates env in-place: keeps system blocks, replaces
// messages with [assistantSummary, latestUser]. Returns the number of
// messages elided (0 if env is nil or has no messages).
func RewriteEnvelope(env *translate.RequestEnvelope, summary string) int {
	if env == nil {
		return 0
	}
	return env.RewriteForHandover(summary)
}

// TrimLastN keeps the most recent n non-system messages plus system blocks
// (n <= 0 treated as 3) and returns the number elided; 0 if env is nil.
//
// No longer used on the handover failure path — summarizer failure now keeps
// full history instead of trimming. Retained for a future context-window
// overflow guard.
func TrimLastN(env *translate.RequestEnvelope, n int) int {
	if env == nil {
		return 0
	}
	return env.TrimLastNMessages(n)
}
