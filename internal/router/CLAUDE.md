# internal/router — CLAUDE

> **Mirror notice.** Verbatim sync with [AGENTS.md](AGENTS.md). **Update both together** — divergence = bug.

Inner-ring `Router` interface + `Request`/`Decision`/`ModelSpec`/`ModelRegistry` value types. Plus subpackages for cache-aware turn-routing primitives. Read [root CLAUDE.md](../../CLAUDE.md) first.

## Subpackages

| Package | Role | Guide |
|---|---|---|
| [`catalog/`](catalog) | **Single source of truth** for per-model data (tier, ordered provider bindings, pricing, cost math). Add a model here. | [catalog/CLAUDE.md](catalog/CLAUDE.md) |
| [`cluster/`](cluster) | AvengersPro-derived primary `Router` impl (P0) | [cluster/CLAUDE.md](cluster/CLAUDE.md) |
| [`policy/`](policy) | Shared candidate resolution, sidecar adapter, and conformance primitives for external policy routers | — |
| [`planner/`](planner) | Cache-aware EV policy (STAY vs SWITCH) | [planner/CLAUDE.md](planner/CLAUDE.md) |
| [`handover/`](handover) | `Summarizer` interface + envelope rewrite | [handover/CLAUDE.md](handover/CLAUDE.md) |
| [`cache/`](cache) | Cross-request semantic response cache | [cache/CLAUDE.md](cache/CLAUDE.md) |
| [`sessionpin/`](sessionpin) | Sticky per-session routing pin | [sessionpin/CLAUDE.md](sessionpin/CLAUDE.md) |
| [`turntype/`](turntype) | Inbound turn-type classifier | [turntype/CLAUDE.md](turntype/CLAUDE.md) |

All inner-ring — except `cluster/`, which is the adapter-tier exception: it does real filesystem I/O to load model artifacts (embedder weights, cluster bundles) at boot. See the [root layer diagram](../../CLAUDE.md), which places `internal/router/cluster` in the adapter tier alongside `internal/postgres` and `internal/providers/*`. Every other subpackage above is I/O-free.

## Adding a new `router.Router` implementation

1. **External policy:** construct `policy.SidecarRouter` with a roster mapper, managed provider policy, strategy ID, and unavailable sentinel. Register it with `proxy.WithPolicyStrategy`; candidate filtering, result validation, metadata, outcome, and feedback are shared.
2. **In-process implementation:** create a sibling subpackage to `cluster/` and implement `Route(ctx, router.Request) (router.Decision, error)`.
3. **Add compile-time check:** `var _ router.Router = (*X)(nil)`.
4. **Wire in `../../cmd/router/main.go`** (replacing, wrapping, or registering it as needed).
5. **If the impl needs CGO or external libs**, follow the build-tag pattern in `cluster/embedder_onnx.go` + `embedder_stub.go` so contributors without the library can still `go test` locally.
6. **Failure modes return errors, not silent fallbacks.** The cluster scorer's `ErrClusterUnavailable` → HTTP 503 pattern is the model: silent fallback to a default model masks regressions + lets quality silently degrade in eval + prod.

## Invariants

- **No I/O in this ring.** Interfaces, value types, pure functions only. Adding an I/O method (HTTP, DB, queue, FS) on anything here is a layering violation — surface it on `auth.Service` / `proxy.Service` or an adapter subpackage instead.
- **Pure-Go utility libs are fine** (`golang-lru`, `uuid`, error helpers).
