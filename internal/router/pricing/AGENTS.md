# internal/router/pricing — AGENTS

> **Mirror notice.** Verbatim sync with [CLAUDE.md](CLAUDE.md). **Update both together** — divergence = bug.

Per-model USD pricing + per-model cache-read multipliers. Read [root CLAUDE.md](../../../CLAUDE.md) first.

## What's here

- Per-model input / output / cache-write USD rates.
- Per-model cache-read multipliers: Anthropic 0.10, OpenAI 0.50, Gemini 0.25, DeepSeek 0.10.
- `DefaultCacheReadMultiplier = 0.5` for unspecified models.
- Pure data + lookup helpers.

## Invariants

- **Single source of truth.** The OTel emitter and the planner both read the same map. Don't introduce a parallel price table elsewhere.
- **Per-provider cache-read multipliers, not global.** A single global multiplier makes cross-provider switches (opus → gpt-5) economically wrong.
- **Always go through `pricing.Pricing.EffectiveCacheReadMultiplier`** — never read the bare struct field.
- **No I/O.** Static data + accessors.

## Updating prices

When Anthropic / OpenAI / Google / OpenRouter / Fireworks change prices, update this map. For the α-blend cost values used by the cluster scorer, also update `train_cluster_router.py`'s `DEFAULT_COST_PER_1K_INPUT` + rerun training — those are baked at training time, not looked up at request time.
