# internal/api — CLAUDE

> **Mirror notice.** Verbatim sync with [AGENTS.md](AGENTS.md). **Update both together** — divergence = bug.

Presentation layer. Handlers adapt HTTP ↔ Service. Read [root CLAUDE.md](../../CLAUDE.md) first.

## Subpackages

- `admin/` — operational endpoints: `/health`, `/validate`, `/admin/v1/*`
- `anthropic/` — Anthropic Messages surface (`/v1/messages`, passthrough, `/v1/route`)
- `openai/` — OpenAI Chat Completions (`/v1/chat/completions`)
- `gemini/` — Gemini native (`/v1beta/models/:modelAction`)

## Import rules

- May import `internal/auth` (Service handle + middleware-context types) and `internal/proxy` (routing/dispatch service handle).
- May import `internal/observability` for logging, `internal/providers` for shared sentinel errors, `internal/router/cluster` for `ErrClusterUnavailable` sentinel + `DeployedModelsSource` interface.
- **Must not import** `internal/postgres`, any concrete `internal/providers/*` adapter, or `internal/translate` directly.
- Concrete instances reach presentation only via constructor params from composition root.

## Adding an HTTP endpoint

1. **Decide timeout budget.** Cheap auth-only ops use `validateTimeout` / `healthTimeout` (1 s). Provider calls get own constant in [`../server/server.go`](../server/server.go) — pick budget + justify in comment.
2. **Decide auth.** Routes needing valid `rk_` bearer go through `middleware.WithAuth(authSvc)`. Admin endpoints use `WithAdminOrAuth` (admin cookie OR bearer) or `WithAdminOnly` (admin cookie only). Unauthed routes (e.g. `/health`) attach no auth middleware.
3. **Decide if self-hoster dashboard surface.** `/ui/*` static dashboard, `/admin/v1/auth/*`, `/admin/v1` mgmt group (metrics, keys, provider-keys, config, excluded-models) mount only when `mode == server.DeploymentModeSelfHosted`. New endpoints whose only consumer is self-hosted dashboard go inside that block; product-surface endpoints (`/v1/*`, `/v1beta/*`, `/health`, `/validate`) stay outside so they're available in `managed` mode too. **Do not** add new `/admin/v1/*` route outside the selfhosted block — would re-expose redundant control plane on Weave-managed deploys.
4. **Pick (or create) the right subpackage.** Operational → `admin/`; Anthropic Messages → `anthropic/`; OpenAI → `openai/`; Gemini → `gemini/`. New surfaces get their own subpackage.
5. **Use `observability.FromGin(c)` for request-scoped logger.** For authed installation: `middleware.InstallationFrom(c)` (nil if `WithAuth` not applied — handler should be on authed group). For BYOK secrets: `middleware.ExternalAPIKeysFrom(c)`.
6. **Pick the right service.** Identity-only ops → `*auth.Service`. Routing/dispatch/translate → `*proxy.Service`. Don't touch repositories, router, providers, planner/handover/cache packages from a handler. Handler adapts HTTP ↔ service; service does the work.
7. **Test with in-memory fakes + gin testing harness** (`httptest.NewRequest`/`ResponseRecorder`). No real DB for handler tests — use fakes from [`../auth/service_test.go`](../auth/service_test.go) and [`../proxy/service_test.go`](../proxy/service_test.go) as model.

## History

`internal/router/heuristic` and `internal/router/evalswitch` previously lived in the API ring; both removed when heuristic fallback retired in favor of `cluster.ErrClusterUnavailable` → HTTP 503.
