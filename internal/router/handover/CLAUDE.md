# internal/router/handover — CLAUDE

> **Mirror notice.** Verbatim sync with [AGENTS.md](AGENTS.md). **Update both together** — divergence = bug.

`Summarizer` interface + envelope-rewrite helpers. Read [root CLAUDE.md](../../../CLAUDE.md) first.

## What it does

When the planner decides SWITCH, proxy asks a small model to summarize prior conversation + rewrites the message history to `[synthesizedSummary, latestUser]` before dispatching to the new model. Bounds the switch turn's input cost regardless of session length.

## Layout

- Inner-ring package here defines the **contract only** (`Summarizer` interface, `TrimLastN` fallback, envelope helpers).
- Provider-backed implementation lives in [`../../proxy/handover.go`](../../proxy/handover.go).

## Invariants

- **`Summarizer` implementations MUST respect the context deadline.** On summarizer timeout or error, proxy falls back to `handover.TrimLastN` — that is the correct behavior, not a bug. Do not "fix" by waiting longer.
- **No I/O in this package.** All I/O lives in the proxy-side implementation.
