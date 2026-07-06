# internal/router/rl — AGENTS

> **Mirror notice.** Verbatim sync with [CLAUDE.md](CLAUDE.md). **Update both together** — divergence = bug.

Opt-in `router.Router` that delegates model selection to a trained RL/DPO policy served by the out-of-process **router-rl-sidecar** Cloud Run service (`ml_dev/rl_router/router_policy_server.py`). Read [root CLAUDE.md](../../../CLAUDE.md) and [internal/router/CLAUDE.md](../CLAUDE.md) first.

## What it does (and does not do)

- **Does:** build the eligible candidate set from this router's own `catalog` (deployed models ∩ request's enabled providers, minus exclusions, minus `ToolUseLow` on tool turns), ask the policy to pick one, and map the choice back to a `router.Decision`.
- **Does NOT:** dispatch, translate, embed, or talk to OpenRouter. Dispatch stays in `proxy.Service` over Weave's own providers; the sidecar only answers "which eligible model?". The prompt embedding the policy needs is computed inside the sidecar (against a Weave Google/Vertex key), never here.

## Load-bearing invariants

- **Opt-in only.** Reached solely via `x-weave-router-strategy: rl` (see `proxy.Service.routeFor`); the deployment default stays the cluster scorer.
- **Fail closed.** Every failure path (sidecar unreachable/errored, no eligible candidate, unrecognized selection) returns `ErrPolicyUnavailable`, which the API layer maps to HTTP 503 — mirroring `cluster.ErrClusterUnavailable`. **Never** add a silent fallback to a default model or to the cluster scorer; that masks regressions exactly like the retired heuristic fallback.
- **Adapters don't import adapters.** This package defines its own `ErrPolicyUnavailable` rather than importing `cluster`. Keep it that way.
- **Roster mapping is best-effort.** `mapping.go` mirrors `ml_dev/agent_environment/roster.py`'s `OPENROUTER_MODEL_ALIASES`. The sidecar intersects candidates against its trained roster, so a model it doesn't know is simply dropped — but keep the two dotted-form Claude exceptions accurate.

## Wiring

`cmd/router/main.go` constructs `rl.New(rl.NewHTTPDecider(ROUTER_RL_SIDECAR_URL, …), availableModels)` and injects it via `proxy.Service.WithRLRouter`. Unset URL → `WithRLRouter(nil)` → the strategy header 503s.

## Tests

`Decider` is an interface so `rl_test.go` injects a fake and asserts candidate construction, roster↔catalog mapping, and the fail-closed paths — no network.
