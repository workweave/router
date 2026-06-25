# internal/proxy/usage — CLAUDE

> **Mirror notice.** Verbatim sync with [AGENTS.md](AGENTS.md). **Update both together** — divergence = bug.

Per-credential subscription rate-limit headroom observer. Inner-ring, I/O-free. Read [root CLAUDE.md](../../../CLAUDE.md) first.

## What it does

Tracks the most recent rate-limit utilization each subscription backend reports on every response, keyed by a salted hash of the credential:

- **Claude** (`api.anthropic.com`, OAuth) — `anthropic-ratelimit-unified-{5h,weekly}-*` (the `claude /usage` data), via `ParseAnthropicUnifiedHeaders`.
- **Codex** (`chatgpt.com/backend-api/codex`) — `x-codex-{primary,secondary}-*`, via `ParseCodexHeaders`.

`Snapshot.CostFactor(epsilon, gamma)` turns the binding (more-used) window's utilization into a multiplier on a covered model's catalog cost: ~epsilon when the window has slack, rising to 1.0 (full price) as it binds. Quota is perishable (resets each window), so unused headroom has zero salvage value — spend it, back off only as the cap nears.

## Consumers (both in package `proxy`)

- **Subscription-aware routing** ([`../subsidy.go`](../subsidy.go), `ROUTER_SUBSCRIPTION_AWARE_ROUTING`, default on): discounts covered models' cost term by `CostFactor` so they win routing while the plan has slack, fading to full price as it binds.
- **Per-installation usage-bypass gate** ([`../usage_bypass.go`](../usage_bypass.go)): when an installation has `usage_bypass_enabled` and the caller's observed Claude utilization is below the installation's `usage_bypass_threshold` (or nothing has been observed yet — cold start), `ProxyMessages` passes the request straight through to the requested model on the customer's own subscription — no routing, no substitution, no billing debit. Once utilization crosses the threshold the gate disengages and normal routing (incl. the subsidy discount) takes over. Strict opt-in: off until the customer enables it in the dashboard.

## Why it exists

Subscription customers (Claude Code / Codex logged-in flows) want unused, perishable plan quota spent on their own subscription — not silently redirected to a cheaper substitute, and not billed by us — until they actually approach their cap.

## Invariants

- **Pure in-memory state, no persistence.**
- Entries keyed by `usage.CredentialKey` (HMAC-SHA256 prefix of the token under a process-scoped salt) so logs + metrics never see the raw token.
- A reading stays authoritative for the life of its binding quota window (`freshFor`), not a flat short TTL — a near-cap reading must not age out and re-read as optimistic slack while the window is still exhausted.
- Per-window merge on `Record`: a response reporting only one window must not erase the other window's last-known utilization.
- Periodic `Sweep` (driven by the composition root on a ticker) bounds memory; the package spawns no goroutines of its own.
- I/O-free per the inner-ring rule — just structs, maps, a mutex, and an injected clock.
