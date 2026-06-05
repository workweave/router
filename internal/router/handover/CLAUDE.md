# internal/router/handover — CLAUDE

> **Mirror notice.** Verbatim sync with [AGENTS.md](AGENTS.md). **Update both together** — divergence = bug.

`Summarizer` interface + envelope-rewrite helpers. Read [root CLAUDE.md](../../../CLAUDE.md) first.

## What it does

When the planner decides SWITCH, proxy asks a small model to summarize prior conversation + rewrites the message history to `[synthesizedSummary, latestUser]` before dispatching to the new model. Bounds the switch turn's input cost regardless of session length.

## Layout

- Inner-ring package here defines the **contract only** (`Summarizer` interface, envelope helpers, `TrimLastN` bounded-trim primitive — no longer the failure path).
- Provider-backed implementation lives in [`../../proxy/handover.go`](../../proxy/handover.go).

## Invariants

- **`Summarizer` implementations MUST respect the context deadline.** On summarizer timeout or error, proxy keeps the full prior history unchanged (no trim) — a pricier switch turn beats silently dropping context. Do not "fix" by waiting longer, and do not reintroduce a silent trim fallback.
- **No I/O in this package.** All I/O lives in the proxy-side implementation.
