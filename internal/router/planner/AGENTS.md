# internal/router/planner — AGENTS

> **Mirror notice.** Verbatim sync with [CLAUDE.md](CLAUDE.md). **Update both together** — divergence = bug.

Prism-style cache-aware EV policy. Decides STAY (preserve pinned model's upstream prompt cache) vs SWITCH (take cluster scorer's fresh decision + eat one-time cache miss) per turn. Read [root CLAUDE.md](../../../CLAUDE.md) first.

## Contract

```
Decide(pin, fresh router.Decision, estimated tokens, available models) → STAY | SWITCH
```

- **Pure function.** No DB lookups, no provider calls.
- Inputs are values: pin row, fresh decision, estimated token count, available-model set resolved at boot.

## Math, briefly

Compares expected per-turn savings over the remaining horizon against the eviction cost of warming a new cache. The **tier-upgrade guard** fires when STAY would clearly under-serve the prompt — uses [`../capability`](../capability)'s Low/Mid/High table to overturn a cost-driven "stay" when the fresh decision is in a strictly higher tier than the pin.

## Invariants

- **Pure.** Anything network-touching belongs in `proxy.Service`, not here.
- **Tests cover EV math without spinning anything up.** Use in-memory fixtures; no `httptest`, no Postgres.

## What NOT to do

- **Don't add a runtime override that mutates α-blend.** Cost weighting is baked into the cluster scorer at training time. Per-request cost knobs are a separate (P1) feature, not a planner responsibility.
- **Don't read pricing from anywhere but [`../pricing`](../pricing).** Single source of truth — the OTel emitter and planner must agree.
