---
name: add-provider
description: Add a new upstream provider or model to the router — catalog entry, optional provider wiring, and the required tests. Use when a contributor wants to add a provider (Together, DeepInfra, Cerebras, a self-hosted vLLM/OpenAI-compatible endpoint, etc.) or make a new model routable. This is THE recipe for provider/model PRs; follow every step.
---

# Adding a provider or model to router

Most "add my model" contributions are an **OpenAI-compatible upstream**, which needs **no new adapter package** — just a base-URL constant, a provider constant + env var, a registration block copied from an existing provider, and a catalog entry. Follow the steps in order. The definition of done is at the bottom; a PR that skips a step gets sent back here.

Read first: [`internal/providers/CLAUDE.md`](../../../internal/providers/CLAUDE.md) and [`internal/router/catalog/CLAUDE.md`](../../../internal/router/catalog/CLAUDE.md).

## Decide the shape

- **New model on a provider already wired** (anthropic, openai, google, openrouter, fireworks, deepinfra, bedrock, makora) → you only need **Step 4 (catalog)**. Skip 1–3.
- **New OpenAI-compatible upstream** (vLLM, Together, DeepInfra-likes, customer endpoint) → do **Steps 1–4**. No new adapter package.
- **New non-OpenAI wire format** (a brand-new Anthropic/Gemini-shaped API) → this needs a real adapter package and is maintainer territory; open a [provider request issue](../../../.github/ISSUE_TEMPLATE/provider_request.yml) first instead of guessing.

## Step 1 — Base URL constant

Add a `*BaseURL` constant alongside the others in
[`internal/providers/openaicompat/client.go`](../../../internal/providers/openaicompat/client.go)
(see `DefaultBaseURL`, `FireworksBaseURL`, `DeepInfraBaseURL`, `MakoraBaseURL`):

```go
TogetherBaseURL = "https://api.together.xyz/v1"
```

## Step 2 — Provider constant + env var

In [`internal/providers/provider.go`](../../../internal/providers/provider.go):

1. Add a `Provider*` constant to the block (`ProviderTogether = "together"`).
2. Add the matching entry to `APIKeyEnvVars` (`ProviderTogether: "TOGETHER_API_KEY"`).
3. Add a cache-TTL entry to the TTL map if the provider streams (mirror the 5-minute entries).

`APIKeyEnvVars` is the single source of truth the admin `/config` view reads — never wire a key without adding it here, or config drifts from reality.

## Step 3 — Register in the composition root

`cmd/router/main.go` is the **only** file that constructs concrete providers. **Copy the nearest existing block** (the DeepInfra block is the canonical template for an OpenAI-compatible upstream that needs model-ID mapping) and adapt the names:

```go
{
    togetherBaseURL := config.GetOr("TOGETHER_BASE_URL", openaiCompatProvider.TogetherBaseURL)
    togetherKey := ""
    if !byokOnly {
        togetherKey = config.GetOr("TOGETHER_API_KEY", "")
    }
    providerMap[providers.ProviderTogether] = openaiCompatProvider.NewClientWithModelIDMap(
        togetherKey, togetherBaseURL, upstreamIDsForProvider(providers.ProviderTogether))
    switch {
    case byokOnly:
        logger.Info("Together provider enabled (BYOK only)", "base_url", togetherBaseURL)
    case togetherKey != "":
        envKeyedProviders[providers.ProviderTogether] = struct{}{}
        logger.Info("Together provider enabled", "base_url", togetherBaseURL)
    default:
        logger.Info("Together provider registered (BYOK only — set TOGETHER_API_KEY for deployment-level use)", "base_url", togetherBaseURL)
    }
}
```

Rules that this preserves (don't break them):
- The provider goes into `providerMap` **regardless of mode** (so BYOK/passthrough works without a deployment key).
- It joins `envKeyedProviders` **only** when a real deployment key is present (hard-pin resolution depends on this).
- Use `NewClientWithModelIDMap` + `upstreamIDsForProvider(...)` if the upstream's model IDs differ from the router's slugs; plain `NewClient` otherwise.
- Add the `*_BASE_URL` and `*_API_KEY` vars to [`.env.example`](../../../.env.example) and [`docs/CONFIGURATION.md`](../../../docs/CONFIGURATION.md).

## Step 4 — Catalog entry (the source of truth for the model)

Per-model data lives in [`internal/router/catalog/catalog.go`](../../../internal/router/catalog/catalog.go) — `Tier`, `ProviderBindings` (`Provider` + `UpstreamID` + `Pricing`), and capability flags. Add or extend the `Model`:

- Set `Tier` (`TierLow` / `TierMid` / `TierHigh`) honestly.
- Add a `ProviderBinding` with the `providers.Provider*` constant, the upstream's exact model ID as `UpstreamID`, and correct `Pricing` (input/output $/MTok). Wrong prices break cost-aware routing.
- The binding order is load-bearing: the first binding whose provider is available in the deploy wins. Put the preferred/cheapest provider first.

## What you can't do as a contributor (maintainer step)

Making a model show up in the **cluster routing decision** requires a `bench_column` in
`internal/router/cluster/artifacts/v<X.Y>/model_registry.json`, which only the training
pipeline writes (hand-editing breaks the cluster geometry guarantee). Wiring the provider +
catalog makes the model usable via **hard-pin and BYOK passthrough** immediately; cluster
eligibility is a follow-up a maintainer runs through the model-onboarding pipeline. Note this
in your PR so reviewers know what's left.

## Step 5 — Tests

- Add a table case to the openaicompat client tests if you touched routing/ID-mapping behavior.
- Add a catalog test asserting the new model's tier + pricing + binding (see `catalog_test.go`).
- Tests must be **non-tautological** — assert a value the code produced, not `x == x`.

## Definition of done

- [ ] No new adapter package (unless a genuinely new wire format — see "Decide the shape").
- [ ] No magic strings — provider name used via the `providers.Provider*` constant everywhere.
- [ ] `.env.example` + `docs/CONFIGURATION.md` document the new env vars.
- [ ] `make check` is green (generate + build + test).
- [ ] PR notes whether cluster-routing eligibility (bench column) is still pending.
