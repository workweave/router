# cmd — AGENTS

> **Mirror notice.** Verbatim sync with [CLAUDE.md](CLAUDE.md). **Update both together** — divergence = bug.

Composition root. Only place that constructs concrete adapters + wires them together. Read [root CLAUDE.md](../CLAUDE.md) first.

## Rules

- **`cmd/router/main.go` is the only file that imports concrete `internal/providers/*` adapters and `internal/postgres`.** No other place wires things.
- Keep `main.go` focused on wiring. Today's helpers:
  - `buildClusterScorer` — per-version Scorer assembly + embedder warmup
  - `buildSemanticCache` — response-cache assembly
  - `buildOtelEmitter` — OTel span exporter
  - `runSessionPinSweep` — TTL sweep loop
  - `resolveHardPinModel` / `resolveDefaultBaselineModel` / `resolveAvailableModels` — boot-time model resolution
  - small env parsers
- **No more heuristic-fallback router.** If cluster routing fails to boot, `main.go` panics. Misconfiguration must abort the process rather than silently degrade.
- **Never introduce DI container, reflection-based wiring, or service locator.** Composition = plain Go function calls.
- **`panic` is reserved for startup-time fail-fast** (`config.MustGet`, cluster-scorer boot failure, invalid `ROUTER_DEPLOYMENT_MODE`). Never panic on request path.

## Deployment-mode dispatch

`ROUTER_DEPLOYMENT_MODE` is read at boot:

- `selfhosted` (default): mounts dashboard at `/ui/*`, `/admin/v1/*` API, dashboard cookie auth. Provider keys read from env vars.
- `managed`: dashboard + `/admin/v1/*` not mounted. Every provider registered with empty deployment key; proxy in BYOK-only mode.
- Any other value → panic at boot.

Provider registration:

- Every provider goes into `providerMap` regardless of mode.
- `envKeyedProviders` (parallel set) tracks which providers have a deployment-level key configured so the hard-pin resolver knows what's safe to pin to.
- Managed-mode deploys register every provider with empty key + rely exclusively on BYOK / client-supplied auth.

Single source of truth for provider→env-var mapping = `providers.APIKeyEnvVars` in [`../internal/providers/provider.go`](../internal/providers/provider.go). Admin `/config` view reads it so it can't drift from actual wiring.
