# router — AGENTS

> **Mirror notice.** Verbatim sync with [CLAUDE.md](CLAUDE.md). Claude Code reads `CLAUDE.md`; Cursor + generic agents read `AGENTS.md`. **Update both together** — divergence = bug.

Root guide for AI agents in the `router/` subproject. Covers cross-cutting design + the layer model. **First read for any task:** [README](README.md), then this file. Then read the `CLAUDE.md` inside the package you're editing — each subpackage has its own with focused recipes + invariants.

## Engineering principles

- **Patterns of Enterprise Application Architecture** (Fowler)
- **Designing Data-Intensive Applications** (Kleppmann)
- **Design Patterns** (GoF)
- **CLEAN architecture** (Martin) — especially dependency inversion
- **DRY**
- **Small expert team** — explicit composition, readable wiring; reject DI containers, reflection, framework magic
- **Concise comments, sparingly** — default to none. Only when *why* is non-obvious (hidden constraint, subtle invariant, workaround, surprising behavior). Never rehash code, never reference current task/PR/caller, no multi-paragraph. If removing wouldn't confuse, don't write.
- **Non-tautological tests** — every test must assert behavior that breaks if prod code deleted.

## Layer model and import rules

Three concentric layers. Imports flow inward only.

```
+-------------------------------------------------------------------+
|  cmd/router/main.go             (composition root — wires all)    |
|                                                                   |
|  +-------------------------------------------------------------+  |
|  |  internal/api/admin       (presentation: /health, /validate,|  |
|  |                            /admin/v1/*)                     |  |
|  |  internal/api/anthropic   (/v1/messages, passthrough,       |  |
|  |                            /v1/route)                       |  |
|  |  internal/api/openai      (/v1/chat/completions)            |  |
|  |  internal/api/gemini      (/v1beta/models/:modelAction)     |  |
|  |  internal/server          (route registration)              |  |
|  |  internal/server/middleware (auth, timeout, cluster/embed   |  |
|  |                              overrides, OTel timing)        |  |
|  |  internal/postgres        (adapter: SQLC over pgx; also     |  |
|  |                            session-pin store impl)          |  |
|  |  internal/sqlc            (generated; regenerate via        |  |
|  |                            `make generate`)                 |  |
|  |  internal/router/cluster  (Router impl: AvengersPro,        |  |
|  |                            Multiversion)                    |  |
|  |  internal/providers/*     (Client impls: anthropic, openai, |  |
|  |                            google native, openaicompat,     |  |
|  |                            noop, httputil)                  |  |
|  |  internal/observability/otel (span emitter; adapter)        |  |
|  |                                                             |  |
|  |  +-------------------------------------------------------+  |  |
|  |  |  internal/auth      (identity domain: types,          |  |  |
|  |  |                      repos, Service.VerifyAPIKey,     |  |  |
|  |  |                      APIKeyCache, id/hashing, Tink    |  |  |
|  |  |                      encryptor)                       |  |  |
|  |  |  internal/proxy     (routing/dispatch service:        |  |  |
|  |  |                      Route, ProxyMessages,            |  |  |
|  |  |                      ProxyOpenAIChatCompletion,       |  |  |
|  |  |                      ProxyGemini; turn loop,          |  |  |
|  |  |                      handover adapter, cache writer,  |  |  |
|  |  |                      session-key derivation)          |  |  |
|  |  |  internal/proxy/usage (Anthropic unified-limit        |  |  |
|  |  |                        observer for usage-bypass)     |  |  |
|  |  |  internal/router    (Router iface + Request/Decision  |  |  |
|  |  |                      + ModelSpec/ModelRegistry)       |  |  |
|  |  |  internal/router/cache      (semantic response cache) |  |  |
|  |  |  internal/router/capability (Low/Mid/High tier table) |  |  |
|  |  |  internal/router/handover   (Summarizer iface +       |  |  |
|  |  |                              envelope rewrite)        |  |  |
|  |  |  internal/router/planner    (cache-aware EV policy)   |  |  |
|  |  |  internal/router/pricing    (per-model USD + cache    |  |  |
|  |  |                              read multipliers)        |  |  |
|  |  |  internal/router/sessionpin (Pin types + Store iface) |  |  |
|  |  |  internal/router/turntype   (turn-type detector)      |  |  |
|  |  |  internal/providers (Client iface + types + canonical |  |  |
|  |  |                      Provider* name constants)        |  |  |
|  |  |  internal/translate (cross-format wire-format         |  |  |
|  |  |                      conversion: OpenAI <-> Anthropic |  |  |
|  |  |                      <-> Gemini; pure, no I/O)        |  |  |
|  |  |  internal/sse       (zero-alloc SSE framing helpers)  |  |  |
|  |  +-------------------------------------------------------+  |  |
|  +-------------------------------------------------------------+  |
|                                                                   |
|  internal/config         (env helpers: MustGet, GetOr)            |
|  internal/observability  (slog logger + gin middleware)           |
+-------------------------------------------------------------------+
```

### Hard rules

- **Layering is load-bearing.** Imports flow inward only. Inner-ring packages must not import adapter or presentation packages; adapters never import each other; only `cmd/router/main.go` constructs concrete things. Inner-ring packages may import each other (e.g. `proxy.Service.Route` returns `router.Decision`; `proxy.Service` calls `translate`, `sessionpin`, `planner`, `handover`, `cache`, `pricing`, `capability`, `turntype`, `usage` to compose a turn).
- **Small utility third-party libs allowed at every layer.** Layering = about *where I/O and behavior live*, not banning go.mod entries. Reach for vetted small lib (`golang-lru`, `uuid`, error helpers) before rolling own. Reject heavyweight frameworks (DI containers, ORMs, metaprogramming kits).
- **Inner-ring packages are I/O-free.** `internal/router`, `internal/providers`, `internal/translate`, `internal/sse`, `internal/router/{cache,capability,handover,planner,pricing,sessionpin,turntype}`, `internal/proxy/usage` define interfaces, value types, pure functions only. Adding I/O method (HTTP, DB, queue, FS) = layering violation; put on `auth.Service` / `proxy.Service` or adapter subpackage. Pure-Go utility libs fine.
- **Adapters depend only on inner ring.** `internal/postgres` may also import `internal/sqlc`. Adapters never import each other — `internal/postgres` doesn't know `internal/api/admin` etc. Note: provider adapters (`internal/providers/<name>/`) import `internal/proxy` for `OnUpstreamMeta` callback so streaming responses record usage/headers back to proxy — one of few inward-pointing adapter→inner-ring imports, intentional.
- **`internal/api/*` and `internal/server`** depend on `internal/auth` (Service handle + middleware-context types) and `internal/proxy` (routing/dispatch service handle). May import `internal/observability` for logging, `internal/providers` for shared sentinel errors, `internal/router/cluster` for `ErrClusterUnavailable` sentinel + `DeployedModelsSource` interface (API handlers map sentinel → HTTP 503). Must not import `internal/postgres`, any concrete `internal/providers/*` adapter, or `internal/translate` directly. Concrete instances reach presentation only via constructor params from composition root.
- **`internal/config` and `internal/observability` are leaf utilities** — must not import any other package under `internal/`. Third-party utility deps fine; today pull only stdlib + gin (request-scoped logger middleware). `internal/observability/otel` subpackage *is* an adapter (builds real OTLP exporter) and can import other internal packages; parent `internal/observability` stays a leaf.
- **Composition happens in `cmd/router/main.go`.** Only file that constructs concrete adapters + injects them. No other place wires things. See [`cmd/CLAUDE.md`](cmd/CLAUDE.md).

If wanting to import something that violates these rules, design is wrong — surface as interface in appropriate inner-ring package and implement in adapter subpackage.

## Where to put new code

Pick by responsibility, then read that package's `CLAUDE.md`:

| Responsibility | Package | Guide |
|---|---|---|
| HTTP endpoint (handler + route) | `internal/api/<group>/` | [internal/api/CLAUDE.md](internal/api/CLAUDE.md) |
| Identity / API-key logic | `internal/auth` (method on `*Service`) | [internal/auth/CLAUDE.md](internal/auth/CLAUDE.md) |
| Routing / dispatch / per-turn orchestration | `internal/proxy` (method on `*Service`) | [internal/proxy/CLAUDE.md](internal/proxy/CLAUDE.md) |
| Cross-format wire conversion (no I/O) | `internal/translate` | [internal/translate/CLAUDE.md](internal/translate/CLAUDE.md) |
| New upstream provider | `internal/providers/<name>/` | [internal/providers/CLAUDE.md](internal/providers/CLAUDE.md) |
| New `Router` implementation | `internal/router/<name>/` | [internal/router/CLAUDE.md](internal/router/CLAUDE.md) |
| Cluster scorer / artifacts | `internal/router/cluster/` | [internal/router/cluster/CLAUDE.md](internal/router/cluster/CLAUDE.md) |
| Cache-aware turn routing internals | `internal/router/{planner,handover,cache,sessionpin,pricing,capability,turntype}/` | each has its own CLAUDE.md |
| Anthropic usage-bypass gate | `internal/proxy/usage` | [internal/proxy/usage/CLAUDE.md](internal/proxy/usage/CLAUDE.md) |
| New column / SQL query | `db/queries/` + `internal/postgres/` | [db/CLAUDE.md](db/CLAUDE.md), [internal/postgres/CLAUDE.md](internal/postgres/CLAUDE.md) |
| Doc under `docs/` | `docs/` | [docs/CLAUDE.md](docs/CLAUDE.md) |

**Default rule:** put logic in the package that uses it. Only promote to a shared home (`auth`, `proxy`, `translate`, `config`, `observability`, `sse`) when 3+ packages need the same logic.

## Adding a new helper

Don't, unless same logic needed in 3+ places and no plausible existing home. Canonical homes:

- **Auth helpers** (token prefix, ID gen, hashing, encryption) → [`internal/auth`](internal/auth) alongside types they support.
- **Env parsing** → [`internal/config`](internal/config).
- **Logging / tracing** → [`internal/observability`](internal/observability) (OTel exporter in `otel` subpackage).
- **SSE framing** → [`internal/sse`](internal/sse).

If new helper doesn't fit, justify new package in code comment before creating.

## Conventions

### Go style

- **No magic strings for provider/model names.** Use named constants from `internal/providers` (`providers.ProviderAnthropic`, `providers.ProviderOpenAI`, `providers.ProviderGoogle`, `providers.ProviderOpenRouter`, `providers.ProviderFireworks`) everywhere provider names appear as values — map keys, switch cases, `router.Decision.Provider` literals, log fields, test fixtures. For new model name constants, add to appropriate package before use. Bare string literals = review-blocking.
- Keep files small. Split distinct logic into separate files, especially when shared between multiple places.
- Avoid unnecessary nesting — flatten conditionals with early returns + combined conditions.
- All exported symbols carry godoc starting with symbol name (`Foo does X` or `// Foo is …`).
- Errors flow up. Don't swallow; don't log-and-continue on request path. `fireMarkUsed` in [service.go](internal/auth/service.go) is the one documented exception (best-effort, off request path).
- Use `errors.Is` / `errors.As`, never `==` or `!=` on errors. For no-rows checks: `errors.Is(err, sql.ErrNoRows)`.
- Use `slog` (via `observability.Get` / `observability.FromGin`), not `fmt.Println` or `log.Print*`.
- Sentinel errors typed (`var ErrFoo = errors.New(...)`) + live in same package as function that returns them. HTTP layer maps to status codes; do not export HTTP semantics from inner-ring packages.
- Constructor injection over package-level singletons. Inject clock (`auth.Clock = func() time.Time`), logger, HTTP client, etc.

### Tests

- Tests live next to code (`foo_test.go` next to `foo.go`). Prefer `<pkg>_test` external test packages so public API exercised; use internal package only when test needs unexported state (`*_internal_test.go` files in `internal/proxy` are canonical).
- Real assertions only. Compare value code-under-test produced to value test author chose. Tautological assertions (`x == x`, "constructor returns instance", "mock called with X") rejected.
- Use `testify/assert` + `testify/require`. Use `require.Eventually` for async (see [service_test.go](internal/auth/service_test.go) `fireMarkUsed` assertion).
- In-memory fakes for repos/routers/provider clients are cheap + far better than mocks for unit testing Service.
- No DB-backed integration tests in `internal/`. If need real Postgres, `docker compose` stack is runtime fixture; write scripts under `scripts/` rather than `*_test.go`.

### Logging

- Log message explains in plain English what happened. Include `err` + relevant context (IDs, counts, status codes).
- Keep log statements on single line with all args inline.
- Use `log.With("key", value)` to attach repeated context once, rather than repeating same key-value on every call.
- snake_case for log attribute keys (`api_key_id`, not `apiKeyID`).
- Log `Debug` for routine ops (auth checks, repo calls); `Info` for major business events (server start, key issuance). Reserve `Error` for genuine failures needing on-call attention; auth-401 is `Debug`, not `Error`.
- Never log raw bearer tokens or hashes. 8-char prefix + 4-char suffix (`KeyPrefix`/`KeySuffix` columns on `auth.APIKey`) are safe; full token is not.

## Things to NEVER do

- **Never import code from outside this subproject.** Router is standalone Go module (`module workweave/router`) with no cross-project deps. If need utility from elsewhere in monorepo, copy into appropriate `internal/` package with own godoc.
- **Never write raw SQL outside `db/queries/`** or call `pgx.Pool` directly from anywhere except `internal/postgres/`. SQLC is only data mapper.
- **Never reach across layers.** Handler in `internal/api/` calling `*sqlc.Queries` directly = layering violation; surface Service method instead. Repo calling another repo = layering violation; put orchestration in `auth.Service` / `proxy.Service`.
- **Never add FKs to tables outside router's own schema.** Such tables don't exist in this project. `organization_id` + `created_by` = opaque external strings, not FKs.
- **Never panic on request path.** Reserve `panic` for startup-time fail-fast (`config.MustGet`, cluster-scorer boot failure, invalid `ROUTER_DEPLOYMENT_MODE`) where misconfiguration must abort process.
- **Never introduce DI container, reflection-based wiring, or service locator.** Composition = plain Go function calls in `cmd/router/main.go`.
- **Never log secrets, raw API keys, or full request bodies.** First 8 + last 4 chars on `auth.APIKey` are safe form. BYOK secrets at rest go through `auth.Encryptor` (Tink AES-256-GCM); plaintext only in memory for request lifetime.
- **Never edit generated files** under `internal/sqlc/`. Regenerate with `make generate`. SQLC's "DO NOT EDIT" header is load-bearing.

## Deployment modes

`ROUTER_DEPLOYMENT_MODE` read at boot in `cmd/router/main.go`:

- **`selfhosted`** (default): full dashboard at `/ui/*`, `/admin/v1/*` API (auth, metrics, keys, provider-keys, config, excluded-models), dashboard cookie auth all mounted. Provider keys read from env vars; missing keys keep providers registered for client-passthrough but exclude from hard-pin resolution.
- **`managed`**: dashboard + `/admin/v1/*` not mounted at all — Weave-managed deploys have separate control plane. Every provider registered with empty deployment key; proxy service in BYOK-only mode, so request without BYOK or client-supplied auth for chosen provider 400s rather than silently spending platform budget. Setting variable to any other value panics at boot.

When adding new endpoint, put inside `selfhosted` block in `server.Register` unless part of product surface (`/v1/*`, `/v1beta/*`, `/health`, `/validate`). Do not re-expose admin surface in managed mode.

## Eval harness (sibling `router-internal/eval/`)

The eval harness is a sibling Poetry package, **not in this repo** — lives at `router-internal/eval/` in the WorkWeave monorepo and runs as a Modal app. It exercises the router via staging headers; see that package's README.

**Per-request router selection (server side):**

- [`internal/server/middleware`](internal/server/middleware).`WithClusterVersionOverride` reads `x-weave-cluster-version: v0.X` header + stashes version on request context. `cluster.Multiversion.Route` reads via `cluster.VersionFromContext` + dispatches to matching `Scorer`. Customer traffic (no header) always serves deployment's default version (`ROUTER_CLUSTER_VERSION` → `artifacts/latest`).
- `WithEmbedOnlyUserMessageOverride` honors `x-weave-embed-only-user-message: true|false` header, flipping proxy between embedding user-role text only (default) + concatenated turn stream.

**What to NOT do:**

- **Do NOT re-introduce heuristic-vs-cluster A/B switch.** Heuristic retired because silent-fallback behavior masked cluster regressions. If need to compare strategies, ship alternate strategy as another `internal/router/X` package + promote on own merits, not as runtime fallback.
- **Do NOT add runtime override that picks specific model directly.** Whole point is comparing *router strategies*, not per-request model overrides.

## Quick reference

- **"Should I commit `internal/sqlc/`?"** Yes. Dockerfile + CI builds depend on generated code being present. Run `make generate` before committing migration or query changes.
- **"How do I run one-off query against local DB?"** `docker compose exec postgres psql -U router -d router`. Migrate step has already applied schema. (Router lives in `router` schema; pool's `AfterConnect` hook pins `search_path` so accidental writes to `public.*` are impossible.)
- **"How do I add new OpenAI-compatible upstream?"** Add `*BaseURL` constant to `internal/providers/openaicompat`, provider-name constant + env-var entry to `internal/providers/provider.go`, registration block in `cmd/router/main.go`. No new adapter package. See [internal/providers/CLAUDE.md](internal/providers/CLAUDE.md).
