# internal/router/catalog — AGENTS

> **Mirror notice.** Verbatim sync with [CLAUDE.md](CLAUDE.md). **Update both together** — divergence = bug.

Single source of truth for per-model data: capability tier, ordered list of provider bindings, per-binding pricing + upstream model ID. Read [root CLAUDE.md](../../../CLAUDE.md) first.

## What's here

- `Model` — one struct per logical model. Fields: `ID`, `Tier`, `ContextWindow`, `Providers []ProviderBinding`.
- `ProviderBinding` — one `(Provider, UpstreamID, Price)` tuple. A model's bindings are ordered: the first whose `Provider` name is in the deploy's available set wins.
- `Pricing` — per-binding input / output / cache-read pricing.
- `Tier` — Low / Mid / High.
- `ContextWindow` — model's total input+output token budget in tokens. 0 falls back to `DefaultContextWindow` (128K).
- Lookup helpers: `ByID`, `ResolveBinding`, `PriceFor(provider, id)`, `PrimaryPriceFor(id)`, `TierFor`, `IsAtOrBelow`, `AllowedAtOrBelow`, `AllPrimaryPricing`, `ValidateDeployed`, `ContextWindowFor`.
- Cost math: `EffectiveInputCost`, `EffectiveOutputCost` — the OTel emitter, telemetry write path, and billing debit hook all funnel through these.

## Adding a model

1. Append one `Model{}` struct literal to `Models` in `catalog.go`.
2. If the model is a routing target, list it in the cluster bundle's `model_registry.json` (this catalog says how to price/dispatch a model; the registry says which version routes to it).
3. Run `go run ./cmd/genprices` to regenerate `install/install.sh` + `install/cc-statusline.sh`.

That's it. Nothing else needs editing — the planner, scorer, OTel emitter, install scripts, and provider modelIDMaps all flow from this file.

## Multi-provider semantics

Today most models carry a single binding. The catalog's data shape is multi-binding so SOC 2 direct-provider rows can append an OpenRouter fallback without touching call sites: managed-prod deploys (no `OPENROUTER_API_KEY`) get the primary binding; self-hosters with only OpenRouter get the trailing one.

The cluster scorer resolves each routable model's binding at boot via `ResolveBinding(id, availableProviders)`. The chosen binding's `Provider` becomes the `router.Decision.Provider`; the planner then uses `catalog.PriceFor(provider, id)` so STAY-vs-SWITCH EV math is correct when a model is served by different providers at different prices.

## Invariants

- **No I/O.** Pure data + accessors. Adding HTTP, DB, or FS calls = layering violation.
- **`Models` is the only writer.** `ByID` / `PrimaryPriceFor` / `TierFor` are read-only views over it.
- **Every binding's `Provider` is one of the `providers.Provider*` constants.** Tested in `catalog_test.go`.
- **Every binding has positive input + output prices.** Tested.
- **No duplicate `Model.ID`s.** Tested.
- **`Providers` is never empty.** Tested.

## What to NOT do

- **Don't read pricing from a parallel table.** The OTel emitter, planner, billing hook, and install-script generator all funnel through this package. A second price table guarantees drift.
- **Don't add a runtime mutation API.** The catalog is compile-time data; per-deploy filtering happens through `ResolveBinding(id, available)`, not by mutating `Models`.
- **Don't fold non-routable model metadata (e.g. `ModelSpec` wire-format capabilities) here yet.** Those live in `internal/router/model.go`. If that file grows past its current scope, surface a separate `Capabilities` field on `Model` rather than entangling them.
