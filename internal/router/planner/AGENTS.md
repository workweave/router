# internal/router/planner — AGENTS

> **Mirror notice.** Verbatim sync with [CLAUDE.md](CLAUDE.md). **Update both together** — divergence = bug.

Prism-style cache-aware EV policy. Decides STAY (preserve pinned model's upstream prompt cache) vs SWITCH (take cluster scorer's fresh decision + eat one-time cache miss) per turn. Read [root CLAUDE.md](../../../CLAUDE.md) first.

## Contract

```
Decide(pin, fresh router.Decision, estimated tokens, available models) → STAY | SWITCH
```

- **Pure function.** No DB lookups, no provider calls. No clock — the proxy computes cache warmth (it owns the clock) and passes it in as `Inputs.PinCacheCold`.
- Inputs are values: pin row, fresh decision, estimated token count, available-model set resolved at boot, and a cache-cold flag.

## Math, briefly

Compares expected per-turn savings over the remaining horizon against the eviction cost of warming a new cache. The **tier-upgrade guard** fires when STAY would clearly under-serve the prompt — uses [`../catalog`](../catalog)'s Low/Mid/High tier to overturn a cost-driven "stay" when the fresh decision is in a strictly higher tier than the pin.

**Cache-warmth gate.** The cache-read multipliers and eviction cost only apply while the pin's upstream cache is warm. When `Inputs.PinCacheCold` is set (the pinned provider's cache TTL has lapsed — short and best-effort on the OSS compat providers vs Anthropic's 1h window; see [`../../providers`](../../providers).`CacheTTLFor`), both sides are priced uncached so raw economics + the tier guard decide, instead of a phantom cache gluing the session to a stale pin. The zero value means "assume warm", preserving the original behavior.

## Invariants

- **Pure.** Anything network-touching belongs in `proxy.Service`, not here.
- **Tests cover EV math without spinning anything up.** Use in-memory fixtures; no `httptest`, no Postgres.

## What NOT to do

- **Don't add a runtime override that mutates α-blend.** Cost weighting is baked into the cluster scorer at training time. Per-request cost knobs are a separate (P1) feature, not a planner responsibility.
- **Don't read pricing from anywhere but [`../catalog`](../catalog).** Single source of truth — the OTel emitter and planner must agree.
