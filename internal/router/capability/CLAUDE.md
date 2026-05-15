# internal/router/capability — CLAUDE

> **Mirror notice.** Verbatim sync with [AGENTS.md](AGENTS.md). **Update both together** — divergence = bug.

Hand-maintained `Tier` table (Low / Mid / High) for each deployed model. Read [root CLAUDE.md](../../../CLAUDE.md) first.

## What it does

Used by [`../planner`](../planner) to overturn a cost-driven "stay" when the fresh decision is in a strictly higher capability tier than the pin (tier-upgrade guard).

## Invariants

- **Hand-maintained.** Deriving tiers from price was rejected — would silently move models on every pricing change.
- **Every deployed model must have an entry in `capability.Table`.** `Validate()` is called at boot so any deployed model missing a tier entry fails the build loudly rather than silently bypassing the guard.
- **No I/O.** Static table + accessors.

## When to update

Add a row whenever a new model lands in any deployed `model_registry.json`. Removing a model from registries means the row is dead — remove it too.
