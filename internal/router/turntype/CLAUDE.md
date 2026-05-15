# internal/router/turntype — CLAUDE

> **Mirror notice.** Verbatim sync with [AGENTS.md](AGENTS.md). **Update both together** — divergence = bug.

Inbound turn-type classifier. Read [root CLAUDE.md](../../../CLAUDE.md) first.

## What it does

Classifies inbound requests into:

- `MainLoop`
- `ToolResult` — proxy short-circuits to the session pin (these turns' embeddings are mostly noise)
- `SubAgentDispatch`
- `Compaction` — proxy forces Haiku
- `Probe` — proxy bypasses routing entirely

Used by [`../../proxy`](../../proxy) to keep the turn loop cheap + correct.

## Invariants

- **Pure, no I/O.** Static classifier over `router.Request` shape.
- **No upstream dependency in the inner ring.** Don't import providers, postgres, or proxy.
