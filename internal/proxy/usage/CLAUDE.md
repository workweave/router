# internal/proxy/usage — CLAUDE

> **Mirror notice.** Verbatim sync with [AGENTS.md](AGENTS.md). **Update both together** — divergence = bug.

Anthropic usage-bypass gate. Inner-ring, I/O-free. Read [root CLAUDE.md](../../../CLAUDE.md) first.

## What it does

Tracks the most recent Anthropic unified rate-limit utilization — the same data `claude /usage` CLI reads off `anthropic-ratelimit-unified-{5h,weekly}-*` response headers.

When `ROUTER_USAGE_BYPASS_ENABLED=true`, requests whose recorded 5h + weekly utilization are both below `ROUTER_USAGE_BYPASS_THRESHOLD` (default `0.95`) pass straight through to Anthropic with the requested model — no cluster routing, no planner verdict. Once either window crosses threshold, the gate disengages for that credential + the cluster scorer takes over. Observations expire after `ROUTER_USAGE_OBSERVATION_TTL` (default 10 minutes); a torn-down key or long idle period falls back to "cold start = bypass" rather than pinning the gate open on a stale near-100% reading.

## Why it exists

Anthropic-plan customers (Claude Code's logged-in flow) want unused quota spent on Anthropic, not silently redirected to a cheaper substitute, until actually approaching cap.

## Invariants

- **Pure in-memory state, no persistence.**
- Entries keyed by `usage.CredentialKey` (salted hash of upstream API key bytes) so logs + metrics never see the raw token.
- Periodic sweep bounds memory by evicting expired entries.
- I/O-free per the inner-ring rule — observer is just structs + maps + a mutex.
