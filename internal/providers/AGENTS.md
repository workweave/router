# internal/providers — AGENTS

> **Mirror notice.** Verbatim sync with [CLAUDE.md](CLAUDE.md). **Update both together** — divergence = bug.

Provider `Client` interface + canonical `Provider*` name constants + concrete adapters per upstream. Read [root CLAUDE.md](../../CLAUDE.md) first.

## Layout

- `provider.go` — `Client` interface, `Provider*` constants, `APIKeyEnvVars` map (single source of truth for provider→env-var mapping).
- `anthropic/`, `openai/`, `google/`, `openaicompat/` — concrete adapters.
- `noop/` — placeholder client returning `providers.ErrNotImplemented`. Use when wiring a new `availableProviders` key whose adapter hasn't landed yet so the cluster scorer can already filter against it.
- `httputil/` — shared transport + streaming helpers.

## Inward-pointing import (intentional)

Provider adapters (`internal/providers/<name>/`) import `internal/proxy` for the `OnUpstreamMeta` callback so streaming responses record usage/headers back to proxy. This is one of the few inward-pointing adapter→inner-ring imports and is intentional.

## Adding a new `providers.Client` adapter

1. **Create `internal/providers/<name>/client.go`** with a `Client` struct + `NewClient(...)` constructor taking credentials (typically API key string + base URL). For OpenAI-compatible upstreams (vLLM, Together, DeepInfra, customer endpoints), **prefer adding a sibling `*BaseURL` constant in [`openaicompat`](openaicompat) over a new adapter** — the openaicompat client already covers OpenRouter + Fireworks under their own provider keys.
2. **Implement `Proxy` and `Passthrough`.** The adapter translates the prepared request body to the provider's wire format, sends it with a pooled `http.Client` (use `httputil.NewTransport` and `httputil.StreamBody`), streams the response back. Adapters call `proxy.OnUpstreamMeta` when they observe usage/header data. Do not leak provider-specific types across the package boundary.
3. **Add compile-time check:** `var _ providers.Client = (*Client)(nil)`.
4. **Add a canonical name constant** to [`provider.go`](provider.go) (the `Provider*` block) + register the matching env-var name in `APIKeyEnvVars`. Today's wired keys: `"anthropic"`, `"openai"`, `"google"`, `"openrouter"`, `"fireworks"`. The composition root reads `APIKeyEnvVars`, so the admin `/config` view can't drift from actual wiring.
5. **Wire in `../../cmd/router/main.go`.** Only place that imports the provider package directly. The provider must be added to `providerMap` regardless of mode; `envKeyedProviders` (parallel set) tracks which providers have a deployment-level key configured.

## Multi-provider routing

Router serves five vendor pools (Anthropic / OpenAI / Google / OpenRouter / Fireworks) from one composition root. A single routing decision picks `(Provider, Model)` + proxy dispatches accordingly.

- `cluster.NewScorer` filters `model_registry.json`'s `deployed_models` list to entries whose `provider` is in `availableProviders`. argmax runs over the filtered list, so a deploy without an OpenRouter key cannot accidentally emit a `deepseek-*` decision.
- `model_registry.json` = flat list of `{model, provider, bench_column, proxy?, proxy_note?}` entries. Direct columns are 1:1 with OpenRouterBench; proxy entries (`proxy: true`) reuse another column's score until direct ranking data is available. The training script copies bench-column scores onto every deployed entry referencing that column rather than averaging columns — that's how the scorer can rank `gpt-5` and proxy `claude-opus-4-7` distinctly.
- **`google/` ships a native Generative Language REST client** (`NativeClient` against `https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent`). The OpenAI-compat surface at `/v1beta/openai` does **not** preserve the opaque `thought_signature` field that multi-turn tool use against Gemini 3.x preview models requires, so the native client is mandatory for those flows. Auth via `x-goog-api-key` header.
- `openaicompat/` is the generic OpenAI Chat Completions adapter, used today for OpenRouter (`https://openrouter.ai/api/v1`) + Fireworks (`https://api.fireworks.ai/inference/v1`).

## What is load-bearing

- **The training script is the only writer of `rankings.json`.** Hand-editing breaks the cluster geometry guarantee (`scorer.go`'s sorted-candidate ordering must match what training produced). Re-run `train_cluster_router.py` after touching `model_registry.json` + commit the regenerated artifact.
- **Cluster scorer is availability-aware at boot, not request time.** Filter happens in `NewScorer`; runtime argmax unchanged. Empty filtered set = hard boot error so misconfigured deploys fail loud.

## What to NOT do

- **Don't bypass the provider filter.** If you need to route to a provider whose key isn't registered (and isn't reachable via BYOK or client passthrough), register the provider — don't add a special-case path that ignores `availableProviders`.
- **Don't add bench-column averaging back to the training script.** 1:1 mapping is the point. Two entries that share a column copy the same score; they don't average across columns.
- **Don't route Gemini 3.x preview tool-use through OpenAI-compat surface.** Loses `thought_signature`; second turn 400s. Use `google.NewNativeClient` (already wired by default).
- **Don't add per-installation provider preference yet.** Deploy-time env config + cluster scorer's per-prompt argmax cover v1. Per-installation routing = follow-up PR with a DB migration on `model_router_installations`.
- **Don't leak provider-specific types** (`anthropic.MessageRequest`, OpenAI streaming chunk types, etc.) across the package boundary.
