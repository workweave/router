# router — CLAUDE

> **Mirror notice.** This file is kept verbatim in sync with
> [AGENTS.md](AGENTS.md). Different LLM tooling reads different filenames
> (Claude Code reads `CLAUDE.md`; Cursor and most generic agents read
> `AGENTS.md`), so we maintain both. **Update both files together** —
> divergence is a bug, not a feature.

This file instructs AI agents working within the `router/` subproject. It
covers the router-specific layering and design conventions you must respect
when adding or modifying code in `router/`.

The first read for any task in this directory is the [README](README.md) for
context, then this file for rules.

## Engineering principles

- **Patterns of Enterprise Application Architecture** (Fowler)
- **Designing Data-Intensive Applications** (Kleppmann)
- **Design Patterns** (GoF)
- **CLEAN architecture** (Martin) — especially dependency inversion
- **DRY** — don't repeat yourself
- **Designed for a small, expert-level team** — favor explicit composition, readable wiring; reject DI containers, reflection, and
  framework magic
- **Concise comments, sparingly added** — default to writing no comments.
  Only add one when the *why* is non-obvious: a hidden constraint, a subtle
  invariant, a workaround, behavior that would surprise a reader. Never
  rehash what the code does, never reference the current task/PR/caller,
  and never write multi-paragraph commentary. If removing the comment
  wouldn't confuse a future reader, don't write it.
- **Non-tautological tests** — every test must assert behavior that would
  break if the production code were deleted

## Layer model and import rules

The router uses three concentric layers. Imports must flow inward only.

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

- **Layering is the load-bearing rule.** Imports flow inward only.
  Inner-ring packages must not import any adapter or presentation package;
  adapters never import each other; only `cmd/router/main.go` constructs
  concrete things. Inner-ring packages may import each other (e.g.
  `proxy.Service.Route` returns a `router.Decision`; `proxy.Service`
  calls into `translate`, `sessionpin`, `planner`, `handover`, `cache`,
  `pricing`, `capability`, `turntype`, and `usage` to compose a turn).
- **Small utility third-party libraries are allowed at every layer.** The
  layering rule is about *where I/O and behavior live*, not about banning
  go.mod entries. Reach for a vetted small library (`golang-lru`, `uuid`,
  error helpers, etc.) before rolling your own. Reject heavyweight
  frameworks (DI containers, ORMs, metaprogramming kits) that bring
  code-organization opinions of their own.
- **Inner-ring packages are I/O-free.** `internal/router`, `internal/providers`,
  `internal/translate`, `internal/sse`, `internal/router/{cache,capability,
  handover,planner,pricing,sessionpin,turntype}`, and `internal/proxy/usage`
  define interfaces, value types, and pure functions — nothing more. Adding
  a method that does I/O (HTTP, DB, queue, file system, etc.) to any of
  these packages is a layering violation; put it on `auth.Service` /
  `proxy.Service` or in an adapter subpackage instead. Pure-Go utility
  libraries on those packages' go.mod are fine.
- **Adapters depend only on the inner ring.** `internal/postgres` may
  additionally import `internal/sqlc`. Adapters never import each other —
  `internal/postgres` does not know about `internal/api/admin`, etc.
  Note: provider adapters (`internal/providers/<name>/`) import
  `internal/proxy` for the `OnUpstreamMeta` callback hook so streaming
  responses can record usage/headers back to the proxy service — this is
  one of the few inward-pointing adapter→inner-ring imports and it is
  intentional.
- **`internal/api/*` and `internal/server`** depend on `internal/auth`
  (for the Service handle and middleware-context types) and on
  `internal/proxy` (for the routing/dispatch service handle). They may
  import `internal/observability` for logging, `internal/providers` for
  shared sentinel errors, and `internal/router/cluster` for the
  `ErrClusterUnavailable` sentinel and the `DeployedModelsSource` interface
  (the API handlers map the sentinel to HTTP 503). They must not import
  `internal/postgres`, any concrete `internal/providers/*` adapter, or
  `internal/translate` directly. Concrete instances reach the presentation
  layer only via constructor parameters from the composition root.
  (`internal/router/heuristic` and `internal/router/evalswitch` previously
  lived here; both were removed when the heuristic fallback was retired
  in favor of `cluster.ErrClusterUnavailable` → HTTP 503.)
- **`internal/config` and `internal/observability` are leaf utilities** —
  they must not import any other package under `internal/`. Third-party
  utility deps are fine; today they pull only stdlib + gin (for the
  request-scoped logger middleware). The `internal/observability/otel`
  subpackage *is* an adapter (it builds a real OTLP exporter) and can
  import other internal packages; the parent `internal/observability` stays
  a leaf.
- **Composition happens in `cmd/router/main.go`.** This is the only file
  that constructs concrete adapters and injects them. No other place wires
  things together. Keep `main.go` focused on wiring; the helpers it
  carries today are `buildClusterScorer` (per-version Scorer assembly +
  embedder warmup), `buildSemanticCache` (response-cache assembly),
  `buildOtelEmitter` (OTel span exporter), `runSessionPinSweep` (TTL
  sweep loop), `resolveHardPinModel` / `resolveDefaultBaselineModel` /
  `resolveAvailableModels` (boot-time model resolution), plus small env
  parsers. There is no longer a heuristic-fallback router — if cluster
  routing fails to boot, `main.go` panics.

If you find yourself wanting to import something that violates these rules,
the design is wrong — surface it as an interface in the appropriate
inner-ring package and implement it in an adapter subpackage instead.

## Adding code — step-by-step recipes

### Adding an HTTP endpoint

1. **Decide the timeout budget.** Cheap auth-only ops use `validateTimeout` /
   `healthTimeout` (1 s). Anything that calls a provider gets its own
   constant in [`server.go`](internal/server/server.go) — pick a budget and
   justify it in a comment.
2. **Decide whether it needs auth.** Routes that need a valid `rk_` bearer
   token go through `middleware.WithAuth(authSvc)`. Admin endpoints use
   `WithAdminOrAuth` (admin cookie OR bearer) or `WithAdminOnly` (admin
   cookie only). Unauthed routes (e.g. `/health`) attach no auth middleware.
3. **Decide whether it's part of the self-hoster dashboard surface.** The
   `/ui/*` static dashboard, `/admin/v1/auth/*`, and the `/admin/v1` mgmt
   group (metrics, keys, provider-keys, config, excluded-models) are
   mounted only when `mode == server.DeploymentModeSelfHosted`. New
   endpoints whose only consumer is the self-hosted dashboard go inside
   that block; endpoints that are part of the product surface (`/v1/*`,
   `/v1beta/*`, `/health`, `/validate`) stay outside it so they're
   available in `managed` mode too. **Do not** add a new `/admin/v1/*`
   route outside the selfhosted block — that would re-expose the
   redundant control plane on Weave-managed deployments.
4. **Pick (or create) the right `internal/api/<group>/` subpackage.**
   Operational endpoints live in `internal/api/admin/`; the Anthropic
   Messages surface lives in `internal/api/anthropic/` (which also hosts
   `/v1/route` for routing introspection and the various
   passthrough endpoints); the OpenAI Chat Completions surface lives in
   `internal/api/openai/`; the Gemini native `:generateContent` /
   `:streamGenerateContent` surface lives in `internal/api/gemini/`.
   New surfaces get their own subpackage.
5. **Use `observability.FromGin(c)` for the request-scoped logger.** If you
   need the authed installation, call `middleware.InstallationFrom(c)`
   (returns nil if `WithAuth` wasn't applied — your handler should be on
   an authed group). For BYOK secrets attached to the request, use
   `middleware.ExternalAPIKeysFrom(c)`.
6. **Pick the right service.** Identity-only operations go on
   `*auth.Service`. Operations that route/dispatch/translate go on
   `*proxy.Service`. Don't touch repositories, the router, providers,
   or the planner/handover/cache packages from the handler. The handler
   adapts HTTP ↔ service; the service does the work.
7. **Test with in-memory fakes and the gin testing harness**
   (`httptest.NewRequest`/`ResponseRecorder`). Don't spin up a real DB for
   handler tests — use the fakes from
   [`internal/auth/service_test.go`](internal/auth/service_test.go) and
   [`internal/proxy/service_test.go`](internal/proxy/service_test.go) as
   the model.

### Adding a method to a Service

Pick the right service first:

- **Identity / API-key concerns** → `*auth.Service` in
  [`internal/auth/service.go`](internal/auth/service.go).
- **Routing / dispatch / cross-format proxying / planner integration**
  → `*proxy.Service` in [`internal/proxy/service.go`](internal/proxy/service.go).

Then:

1. **Define the method on the chosen `*Service`.** No I/O directly here —
   push that into a repository or provider adapter. Inner-ring imports
   (`router`, `providers`, `translate`, `observability`, the various
   `internal/router/*` helper packages, `internal/proxy/usage`) and small
   utility libraries are fine.
2. **If it needs new repository methods, add them to the interfaces in
   [`installation.go`](internal/auth/installation.go) /
   [`api_key.go`](internal/auth/api_key.go) / sibling files.** The
   interface is the contract; the adapter must satisfy it. The
   `sessionpin.Store` interface lives in
   [`internal/router/sessionpin/store.go`](internal/router/sessionpin/store.go)
   and is implemented by `postgres.SessionPinRepository`.
3. **Implement the new repo method in
   [`internal/postgres/repository.go`](internal/postgres/repository.go)**
   (or a sibling file in `internal/postgres/`), adding the corresponding
   SQLC query in `db/queries/`. Run `make generate` to regenerate
   `internal/sqlc/`.
4. **Update the matching `service_test.go` fakes** to satisfy any expanded
   interface. Tests for the new Service method use the fakes; assert on
   real return values, not just that mocks were called.

### Adding a wire-format pair (translation)

When a new inbound format needs to talk to an existing upstream provider
whose wire format differs:

1. **Add the conversion functions to
   [`internal/translate/`](internal/translate).** Pure functions only — no
   I/O, no provider knowledge, no domain types. The package now covers
   all three directions: Anthropic ⇄ OpenAI and Gemini ⇄ {Anthropic,
   OpenAI} via the `RequestEnvelope` intermediate plus per-target
   `emit_*.go` files.
2. **If the response is streaming, adapt
   [`stream.go`](internal/translate/stream.go) /
   [`gemini_stream.go`](internal/translate/gemini_stream.go)** or add a
   sibling decorator. Decorators wrap `http.ResponseWriter` and translate
   on the fly so we never buffer entire responses. Use
   [`internal/sse`](internal/sse) for zero-alloc SSE event framing.
3. **Compose the new translation in `proxy.Service.Proxy*`.** The proxy
   service is the only caller of `translate`. Keep the providers
   (`internal/providers/<name>/`) ignorant of cross-format concerns.

### Adding a new `providers.Client` adapter

1. **Create `internal/providers/<name>/client.go`** with a `Client` struct
   and `NewClient(...)` constructor that takes whatever credentials it
   needs (typically an API key string and a base URL). For OpenAI-compatible
   upstreams (vLLM, Together, DeepInfra, customer endpoints), prefer adding
   a sibling `*BaseURL` constant in
   [`internal/providers/openaicompat`](internal/providers/openaicompat)
   over rolling a new adapter — the openaicompat client already covers
   OpenRouter and Fireworks under their own provider keys.
2. **Implement `Proxy` and `Passthrough`.** The adapter translates the
   prepared request body to the provider's wire format, sends it with a
   pooled `http.Client` (use `httputil.NewTransport` and
   `httputil.StreamBody` from `internal/providers/httputil/`), and streams
   the response back. Adapters call into `proxy.OnUpstreamMeta` when they
   observe usage/header data they want the proxy service to record. Do
   not let provider-specific types leak across the package boundary.
3. **Add a compile-time check:**
   `var _ providers.Client = (*Client)(nil)`
4. **Add a canonical name constant** to
   [`internal/providers/provider.go`](internal/providers/provider.go)
   (the `Provider*` block) and register the matching env-var name in
   `APIKeyEnvVars`. Today's wired keys are `"anthropic"`, `"openai"`,
   `"google"`, `"openrouter"`, and `"fireworks"`. The composition root
   reads `APIKeyEnvVars` so the admin `/config` view can't drift from
   the actual wiring.
5. **Wire it in `cmd/router/main.go`.** This is the only place that
   imports the provider package directly. The provider must be added to
   `providerMap` regardless of mode; `envKeyedProviders` (a parallel
   set) tracks which providers have a deployment-level key configured
   so the hard-pin resolver knows what's safe to pin to. Managed-mode
   deployments register every provider with an empty key and rely
   exclusively on BYOK / client-supplied auth.

### Adding a new `router.Router` implementation

1. **Create a sibling subpackage to `internal/router/cluster/`.** Today
   `cluster/` (AvengersPro, with a `Multiversion` wrapper for per-request
   bundle selection) is the only `Router` impl in production. New ones
   might be e.g. `internal/router/shadow/` for one that wraps two others.
2. **Implement `Route(ctx, router.Request) (router.Decision, error)`.**
3. **Add the compile-time check:** `var _ router.Router = (*X)(nil)`.
4. **Wire it in `cmd/router/main.go`** (replacing or wrapping the
   cluster scorer as needed).
5. **If your impl needs CGO or external libraries**, follow the
   build-tag pattern in `cluster/embedder_onnx.go` + `embedder_stub.go`
   so contributors without the library can still `go test` locally.
6. **Failure modes return errors, not silent fallbacks.** The cluster
   scorer's `ErrClusterUnavailable` → HTTP 503 pattern is the model:
   silent fallback to a default model masks regressions and lets
   quality silently degrade in eval and prod.

### Adding a column or query

1. **Migration first.** Add `db/migrations/NNNN_<name>.up.sql` and
   `.down.sql` in sequential numbering. Wrap in `BEGIN`/`COMMIT`. The down
   migration must be a precise rollback — no `IF EXISTS` guards.
2. **Add the query** to the appropriate `db/queries/<table>.sql` file. Use
   named parameters with type casts (`@param::varchar`). Use
   `sqlc.embed(t)` for JOINs.
3. **Run `make generate`** to regenerate `internal/sqlc/`. Commit the generated
   code alongside your changes.
4. **Update [`internal/postgres/repository.go`](internal/postgres/repository.go)**
   (and [`converters.go`](internal/postgres/converters.go) if a new column
   needs domain mapping). The domain types (`auth.Installation`,
   `auth.APIKey`, `sessionpin.Pin`) must not leak `pgtype` / `uuid`
   concerns — convert at the adapter boundary.

### Adding a doc under `docs/`

Every Markdown doc under `router/docs/` (active or archived) is indexed
in [`docs/README.md`](docs/README.md). When you add a new doc, the same
change must update the index — drift between the doc tree and the index
is a bug, not a feature.

1. **Top of the new file:** include the standard two-line header before
   the H1:

   ```
   Created: YYYY-MM-DD
   Last edited: YYYY-MM-DD
   ```

   The `Created` date is load-bearing — `docs/README.md` orders the
   table of contents by it. Don't backdate; if the doc takes multiple
   days to draft, leave `Created` on the day it first landed and bump
   `Last edited` as it changes.

2. **Append a row to [`docs/README.md`](docs/README.md)** in the
   correct section (Active or Archived), keeping each table sorted by
   `Created` ascending. Write a one- or two-sentence summary covering
   what the doc is for and (for archived docs) why it was archived plus
   a link to the active replacement.

3. **If you are archiving an active doc:** move the row from the active
   table to the archived table with a short reason, and mirror the
   entry in [`docs/plans/archive/README.md`](docs/plans/archive/README.md).
   Move the file with `git mv` so history follows.

4. **Renaming or deleting a doc:** update both this rule's index and
   any inbound links. `grep -rn 'old/path' router/` before merging.

### Adding a new helper

Don't, unless the same logic is needed in three or more places and there's
no plausible existing home. The canonical homes are:

- **Auth-related helpers** (token prefix, ID generation, hashing,
  encryption) live in [`internal/auth`](internal/auth) alongside the
  types they support.
- **Env parsing** lives in [`internal/config`](internal/config).
- **Logging / tracing** lives in
  [`internal/observability`](internal/observability) (with the OTel
  exporter in the `otel` subpackage).
- **SSE framing** lives in [`internal/sse`](internal/sse).

If the new helper doesn't fit any of these, justify the new package in a
code comment before creating it.

## Conventions

### Go style

- **No magic strings for provider names or model names.** Use the named
  constants from `internal/providers` (`providers.ProviderAnthropic`,
  `providers.ProviderOpenAI`, `providers.ProviderGoogle`,
  `providers.ProviderOpenRouter`, `providers.ProviderFireworks`)
  everywhere provider names appear as values — map keys, switch cases,
  `router.Decision.Provider` literals, log fields, test fixtures. For
  new model name constants, add them to the appropriate package before
  using them. Bare string literals for these values are a review-blocking
  issue.
- Keep files small. Split distinct logic into separate files, especially
  when shared between multiple places.
- Avoid unnecessary nesting — flatten conditionals with early returns and
  combined conditions rather than deep `if/else` trees.
- All exported symbols carry a godoc comment. The comment starts with the
  symbol name (Go convention: `Foo does X` or `// Foo is …`).
- Errors flow up. Don't swallow them; don't log-and-continue on the request
  path. `fireMarkUsed` in [service.go](internal/auth/service.go) is the
  one documented exception (best-effort, off the request path).
- Use `errors.Is` / `errors.As`, never `==` or `!=` on errors. For
  no-rows-found checks, always use `errors.Is(err, sql.ErrNoRows)`.
- Use `slog` (via `observability.Get` / `observability.FromGin`), not
  `fmt.Println` or `log.Print*`.
- Sentinel errors are typed (`var ErrFoo = errors.New(...)`) and live in
  the same package as the function that returns them. The HTTP layer maps
  them to status codes; do not export HTTP semantics from inner-ring
  packages.
- Constructor injection over package-level singletons. Inject the clock
  (`auth.Clock = func() time.Time`), the logger, the HTTP client, etc.

### Tests

- Tests live next to the code (`foo_test.go` next to `foo.go`). Prefer
  `<pkg>_test` external test packages so the public API is exercised;
  use the internal package only when a test needs to reach unexported
  state (the `*_internal_test.go` files in `internal/proxy` are the
  canonical examples).
- Real assertions only. Compare the value the code under test produced to
  a value the test author chose. Tautological assertions (`x == x`,
  "constructor returns instance", "mock was called with X") are rejected
  by review.
- Use `testify/assert` and `testify/require` for readability. Use
  `require.Eventually` for async assertions (see
  [service_test.go](internal/auth/service_test.go) `fireMarkUsed`
  assertion as the canonical example).
- In-memory fakes for repositories / routers / provider clients are cheap
  to write and far better than mocks for unit testing the Service.
- No DB-backed integration tests in `internal/` packages. If you need a
  real Postgres, the `docker compose` stack is the runtime fixture; write
  scripts under `scripts/` (when one exists) rather than `*_test.go`.

### SQL and migrations

- Always use named parameters (`@param::varchar`), never numbered (`$1`).
- Always include type casts so SQLC's inference is unambiguous.
- Query names use consistent prefixes: `Insert*`, `Upsert*`, `Get*`,
  `Update*`, `Delete*`.
- Every query should have an explanatory comment (SQLC turns it into a
  godoc on the generated function).
- For no-rows-found, single-row queries return an error — check with
  `errors.Is(err, sql.ErrNoRows)`.
- Always wrap migrations in `BEGIN; ... COMMIT;`.
- Never create migration files manually — use `make migrate-create NAME=<name>`.
- Down migrations must be precise rollbacks of the up migration. No
  `IF EXISTS` guards. Don't separately drop indexes when dropping tables.
- `organization_id` and `created_by` are opaque external identifiers —
  never add foreign keys to tables outside the router's own schema.
- Soft-delete via `deleted_at TIMESTAMP` on tables that need lifecycle.
  Hot-path queries filter `WHERE deleted_at IS NULL`.

### Logging

- Log message should explain in plain English what happened. Include
  the `err` itself and any relevant context (IDs, counts, status codes).
- Keep log statements on a single line with all arguments inline.
- Use `log.With("key", value)` to attach repeated context to a logger
  once, rather than repeating the same key-value pair on every call.
- Use snake_case for log attribute keys (`api_key_id`, not `apiKeyID`).
- Log at `Debug` for routine operations (auth checks, repo calls) and
  `Info` for major business events (server start, key issuance). Reserve
  `Error` for genuine failures that need on-call attention; an auth-401
  is `Debug`, not `Error`.
- Never log raw bearer tokens or hashes. The 8-char prefix and 4-char
  suffix (the `KeyPrefix`/`KeySuffix` columns on `auth.APIKey`) are safe;
  the full token is not.

## Things to NEVER do

- **Never import code from outside this subproject.** The router is a
  standalone Go module (`module workweave/router`) with no cross-project
  code dependencies. If you need a utility from elsewhere in the
  surrounding monorepo, copy it into the appropriate `internal/`
  package with its own godoc instead of importing it.
- **Never write raw SQL outside `db/queries/`** or call `pgx.Pool` directly
  from anywhere except `internal/postgres/`. SQLC is the only data mapper.
- **Never reach across layers.** A handler in `internal/api/` calling
  `*sqlc.Queries` directly is a layering violation; surface a Service
  method instead. A repo calling another repo is a layering violation;
  put the orchestration in `auth.Service` / `proxy.Service`.
- **Never add foreign keys to tables outside the router's own schema.**
  Such tables don't exist in this project. `organization_id` and
  `created_by` are opaque external strings, not FKs.
- **Never panic on the request path.** Reserve `panic` for startup-time
  fail-fast checks (`config.MustGet`, cluster-scorer boot failure,
  invalid `ROUTER_DEPLOYMENT_MODE`) where misconfiguration must abort
  the process, not turn into runtime errors.
- **Never introduce a DI container, reflection-based wiring, or a "service
  locator".** Composition is plain Go function calls in
  `cmd/router/main.go`.
- **Never log secrets, raw API keys, or full request bodies.** The first 8
  + last 4 chars stored on `auth.APIKey` are the safe form; the full
  token is not. BYOK secrets at rest go through `auth.Encryptor`
  (Tink AES-256-GCM); plaintext only lives in memory for the lifetime
  of a request.
- **Never edit generated files** under `internal/sqlc/`. Regenerate with
  `make generate` instead. SQLC's "DO NOT EDIT" header is load-bearing.

## Cluster routing (P0)

`internal/router/cluster` is the AvengersPro-derived primary router
(arxiv 2508.12631, DAI 2025). The full design is in
[`docs/plans/archive/CLUSTER_ROUTING_PLAN.md`](docs/plans/archive/CLUSTER_ROUTING_PLAN.md); this section is
the rules-for-AI subset.

**What's load-bearing:**

- The package compiles in **two layered modes via build tags**:
  - `embedder_onnx.go` vs `embedder_stub.go` — gated by `no_onnx`.
    Default builds compile the real hugot-backed embedder; passing
    `-tags=no_onnx` swaps in a stub `NewEmbedder` that always errors.
    Used by contributors without `libonnxruntime` installed.
  - `-tags ORT` — required by **hugot v0.7+** to enable the ONNX
    Runtime backend. Without it, `hugot.NewORTSession` returns
    "to enable ORT, run `go build -tags ORT`" and `cluster.NewEmbedder`
    fails. The Dockerfile already builds with `-tags ORT`. **Do not
    drop this tag from any production-bound build.**
  - To run the parity integration test, combine: `-tags "onnx_integration ORT"`.
- Local-dev build environment (Apple Silicon dev box):
  - `libtokenizers` static lib must be on the linker path. Pre-built
    releases at https://github.com/daulet/tokenizers/releases/. Extract
    `libtokenizers.darwin-arm64.tar.gz` somewhere user-writable and
    set `CGO_LDFLAGS=-L/path/to/dir`.
  - `libonnxruntime` shared lib via `brew install onnxruntime`. brew
    installs to `/opt/homebrew/lib`, but hugot defaults to
    `/usr/local/lib` lookup. Set `ROUTER_ONNX_LIBRARY_DIR=/opt/homebrew/lib`
    to override. (Linux containers using the Dockerfile don't need
    this — `/usr/lib/libonnxruntime.so` is the default and gets
    populated by the runtime stage.)
- **Versioned artifacts.** Every committed bundle lives at
  `internal/router/cluster/artifacts/v<X.Y>/` with four files:
  `centroids.bin`, `rankings.json`, `model_registry.json`, and
  `metadata.yaml`. The `artifacts/latest` pointer file (single line,
  e.g. `v0.37`) names the version the runtime serves by default; the
  `ROUTER_CLUSTER_VERSION` env var overrides it. Promotion is a
  one-line edit to `latest` plus a redeploy. The committed history
  spans v0.21 through the current `latest` — earlier versions were
  pruned once they fell out of eval comparison.
- The Go runtime builds **only the served default version** by default
  (`cmd/router/main.go`'s `buildClusterScorer`). Setting
  `ROUTER_CLUSTER_BUILD_ALL_VERSIONS=true` switches to building **one
  Scorer per committed bundle** so callers can pin per-request to a
  sibling version with `x-weave-cluster-version: v0.X` via
  `middleware.WithClusterVersionOverride`. This is the
  "compare-against-each-other" mechanism — staging/eval deployments set
  the flag so a single deploy carries every committed bundle and the
  eval harness flips between them per-request. Prod leaves the flag
  off: only the default bundle is loaded into memory and the header
  override is a no-op.
- **Centroids/rankings are write-once.** `train_cluster_router.py`
  always writes to `artifacts/v<X.Y>/` and never overwrites a previous
  version (auto-bumps from `latest` when `--version` is omitted). Pass
  `--from v0.36` to clone a previous version's `model_registry.json`
  before training a new one. **Never edit `centroids.bin` /
  `rankings.json` by hand.** `model_registry.json` is the only
  hand-editable file in a bundle (the training script reads it).
- `metadata.yaml` is informational at runtime — it carries the version
  changelog, training params, deployed models, and α-blend cost values.
  The Go runtime parses it for `/health`-style provenance; the eval
  harness reads it offline. Keep it accurate but it does not affect
  routing decisions.
- `assets/model.onnx` is **NOT in git.** We use Jina's own INT8
  export at `jinaai/jina-embeddings-v2-base-code`, file path
  `onnx/model_quantized.onnx`. The Dockerfile pulls anonymously
  during build (the Jina repo is public — self-hosters don't need
  any credentials); local dev pulls via `scripts/download_from_hf.py`.
  An `HF_TOKEN` build secret is *optional* (raises rate limits in
  CI) and `required=false` in the Dockerfile. The Go embedder reads
  from `/opt/router/assets/model.onnx` (override via
  `ROUTER_ONNX_ASSETS_DIR`). If the file is missing or <1 MiB,
  `cluster.NewEmbedder` errors at boot and `main.go` panics —
  the router refuses to start rather than silently degrading.
  `HF_MODEL_REVISION` is pinned to a Jina SHA by default; bump
  it deliberately if you want to pick up a new upstream export.
- The **cost values** used in the α-blend live in
  `train_cluster_router.py`'s `DEFAULT_COST_PER_1K_INPUT`. They are
  baked into `rankings.json` at training time, not looked up at
  request time (paper §3 — runtime scoring is a single argmax). When
  Anthropic changes prices, update the dict and rerun training.

**What to NOT do:**

- **Don't add per-request cost lookup or a runtime α knob.** α is
  baked at training time; changing it requires retraining. Per-request
  override (`x-weave-routing-alpha`) is P1, not P0 — wait for a
  customer to ask before shipping it.
- **Don't loosen the `MaxPromptChars = 1024` cap** without re-running
  the latency test. BERT inference is O(n²) attention; the cap is
  load-bearing.
- **Don't add fail-open fallbacks.** The cluster scorer returns
  `ErrClusterUnavailable` on every failure path (embed timeout, embed
  error, dim mismatch, prompt too short, empty argmax). The API
  handlers map that to HTTP 503. The previous `heuristic` fallback was
  removed because it silently degraded routing decisions — every
  request that should have hit the cluster scorer instead got
  `claude-haiku-4-5`, which masked real regressions in eval and
  production. New failure modes return the sentinel; do not add a
  default-model shortcut "for safety".
- **Don't change the centroid format without bumping the magic
  string.** `loadCentroids` uses the magic + version header to refuse
  loading mismatched binaries; if you change the layout, bump
  `centroidsMagic` from `CRT1` to `CRT2` (or whatever) so the next
  deploy refuses to load the old binary instead of silently misrouting.
- **Don't overwrite a previously committed artifact version.** Versions
  are frozen for comparison purposes — once `v0.37` is committed, train
  to `v0.38` rather than re-running `train_cluster_router.py` against
  `v0.37`. The training script auto-bumps to prevent this; only override
  with `--version v0.X` for in-place fixes you intend to land as a
  separate commit.
- **Don't bypass the version pointer.** `artifacts/latest` is the
  single source of truth for the default served version. Don't
  hardcode a version in `cmd/router/main.go`; let
  `cluster.ResolveVersion` read the pointer.


## Multi-provider routing (Anthropic / OpenAI / Google / OpenRouter / Fireworks)

The router serves five vendor pools from one composition root. A
single routing decision picks `(Provider, Model)` and the proxy
dispatches accordingly. Cross-format translation lives in
`internal/translate` so handlers never juggle wire formats.

**Architecture:**

- `cmd/router/main.go` registers each provider client. In **selfhosted**
  mode each provider's deployment-level key is read from its env var
  (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GOOGLE_API_KEY`,
  `OPENROUTER_API_KEY`, `FIREWORKS_API_KEY`); a missing key keeps the
  provider registered for BYOK / client-passthrough but excludes it
  from `envKeyedProviders` (the set the hard-pin resolver may pin to).
  In **managed** mode (`ROUTER_DEPLOYMENT_MODE=managed`) every provider
  is registered unconditionally with an empty key and the proxy service
  is flipped into BYOK-only mode — a request without BYOK or
  client-supplied credentials for the chosen provider 400s at the
  scorer rather than silently spending the platform's budget on
  customer traffic. The single source of truth for provider→env-var
  mapping is `providers.APIKeyEnvVars` in
  [`internal/providers/provider.go`](internal/providers/provider.go).
- `cluster.NewScorer` filters `model_registry.json`'s `deployed_models`
  list to entries whose `provider` is in `availableProviders`. argmax
  runs over the filtered list, so a deployment without an OpenRouter
  key cannot accidentally emit a `deepseek-*` decision.
- `model_registry.json` is a flat list of `{model, provider,
  bench_column, proxy?, proxy_note?}` entries. Direct columns are 1:1
  with OpenRouterBench; proxy entries (`proxy: true`) reuse another
  column's score until direct ranking data is available. The training
  script copies bench-column scores onto every deployed entry
  referencing that column rather than averaging columns together —
  that's how the scorer can rank `gpt-5` and the proxy `claude-opus-4-7`
  distinctly.
- `internal/translate` has all three directions (Anthropic ⇄ OpenAI ⇄
  Gemini) routed through a `RequestEnvelope` intermediate. Streaming
  decorators in `stream.go` / `gemini_stream.go` translate SSE events
  on the fly. `proxy.Service` is the only caller of `translate`.
- **`internal/providers/google` now ships a native Generative Language
  REST client** (`NativeClient` against
  `https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent`).
  The OpenAI-compat surface at `/v1beta/openai` does **not** preserve
  the opaque `thought_signature` field that multi-turn tool use against
  Gemini 3.x preview models requires, so the native client is mandatory
  for those flows. Auth is via the `x-goog-api-key` header. The old
  OpenAI-compat path remains in the file but is no longer wired into
  `main.go`.
- `internal/providers/openaicompat` is the generic OpenAI Chat
  Completions adapter, used today for OpenRouter
  (`https://openrouter.ai/api/v1`) and Fireworks
  (`https://api.fireworks.ai/inference/v1`). New OpenAI-compatible
  upstreams (vLLM, Together, DeepInfra, customer-hosted) should plug
  in here under a new provider-name constant rather than getting their
  own adapter package.
- `internal/providers/noop` is the placeholder client that returns
  `providers.ErrNotImplemented`. Use it when wiring a new
  `availableProviders` key whose adapter hasn't landed yet so the
  cluster scorer can already filter against it.

**What is load-bearing:**

- **The training script is the only writer of `rankings.json`.**
  Hand-editing breaks the cluster geometry guarantee
  (`scorer.go`'s sorted-candidate ordering must match what training
  produced). Re-run `train_cluster_router.py` after touching
  `model_registry.json` and commit the regenerated artifact.
- **The cluster scorer is availability-aware at boot, not request
  time.** Filter happens in `NewScorer`; runtime argmax is unchanged.
  An empty filtered set is a hard boot error so misconfigured
  deployments fail loud.
- **Anthropic-only fields (`thinking`, `cache_control`, `metadata`,
  Anthropic beta headers) are stripped at translation time and again
  defensively in the OpenAI / openaicompat adapters.** Keep both
  checks — the belt-and-suspenders is intentional because the field
  set drifts as Anthropic adds beta features.

**What to NOT do:**

- **Don't bypass the provider filter.** If you need to route to a
  provider whose key isn't registered (and isn't reachable via BYOK
  or client passthrough), register the provider — don't add a
  special-case path that ignores `availableProviders`.
- **Don't add bench-column averaging back to the training script.**
  The 1:1 mapping is the point. Two entries that share a column copy
  the same score; they don't average across columns.
- **Don't route Gemini 3.x preview tool-use through the
  OpenAI-compat surface.** It loses `thought_signature` and the second
  turn 400s. Use `google.NewNativeClient` (already the wired default).
- **Don't add per-installation provider preference yet.** Deploy-time
  env config + the cluster scorer's per-prompt argmax cover the v1
  use cases; per-installation routing is a follow-up PR with a DB
  migration on `model_router_installations`.

## Cache-aware turn routing (planner / handover / session pin / cache)

The proxy's per-turn flow is more than "scorer → dispatch". A pinned
session, a planner verdict, an optional handover summary, and a
semantic response cache all sit between the inbound request and the
upstream provider. The packages are intentionally small and
single-purpose so each can be unit-tested without the others.

**Packages:**

- [`internal/router/sessionpin`](internal/router/sessionpin) — `Pin`
  type and `Store` interface for sticky per-session routing. Keyed by
  `(api_key_id, session_key, role)` where `session_key` is a
  16-byte sha256 truncation derived from the inbound request
  (see [`internal/proxy/session_key.go`](internal/proxy/session_key.go)).
  Stage 1 emits `role="default"` only; the column exists so the
  turn-type detector can land role-conditioned pinning without a
  schema change. The Postgres adapter is in `internal/postgres/`;
  `runSessionPinSweep` in `main.go` runs a TTL sweep loop.
- [`internal/router/turntype`](internal/router/turntype) — classifies
  inbound requests into `MainLoop`, `ToolResult`, `SubAgentDispatch`,
  `Compaction`, `Probe`. Used by the proxy to short-circuit to the
  session pin on tool-result turns (whose embeddings are mostly
  noise), force Haiku on compaction turns, and bypass routing
  entirely on probe turns. Pure, no I/O.
- [`internal/router/capability`](internal/router/capability) — hand-
  maintained `Tier` table (Low / Mid / High) for each deployed model.
  Used by the planner to overturn a cost-driven "stay" when the fresh
  decision is in a strictly higher capability tier than the pin.
  `Validate()` is called at boot so any deployed model missing a tier
  entry fails the build loudly rather than silently bypassing the
  guard.
- [`internal/router/pricing`](internal/router/pricing) — per-model
  USD pricing plus per-model cache-read multipliers (Anthropic 0.10,
  OpenAI 0.50, Gemini 0.25, DeepSeek 0.10; `DefaultCacheReadMultiplier
  = 0.5` for unspecified entries). Pure data + lookup helpers; the
  OTel layer also reads this so the cost attributes on spans can't
  drift from what the planner used.
- [`internal/router/planner`](internal/router/planner) — Prism-style
  cache-aware EV policy. Per turn, decides STAY (preserve the pinned
  model's upstream prompt cache) vs SWITCH (take the cluster
  scorer's fresh decision and eat a one-time cache miss). The math
  compares expected per-turn savings over the remaining horizon
  against the eviction cost of warming a new cache; the tier-upgrade
  guard fires when STAY would clearly under-serve the prompt. Pure
  function of `(pin, fresh decision, estimated tokens, available
  models)`; no I/O.
- [`internal/router/handover`](internal/router/handover) — `Summarizer`
  interface plus envelope-rewrite helpers. When the planner decides
  to SWITCH, the proxy asks a small model to summarize the prior
  conversation and rewrites the message history to
  `[synthesizedSummary, latestUser]` before dispatching to the new
  model. This bounds the switch turn's input cost regardless of
  session length. The provider-backed implementation lives in
  `internal/proxy/handover.go`; the inner-ring package only defines
  the contract. On summarizer timeout or error the proxy falls back
  to `handover.TrimLastN`.
- [`internal/router/cache`](internal/router/cache) — cross-request
  semantic response cache. Short-circuits near-duplicate non-streaming
  requests by cosine similarity on the cluster scorer's prompt
  embedding; captured wire-format bytes are replayed without invoking
  the upstream. Per-(installation, inbound-format) isolation, since
  captured bytes are post-translation. Streaming bypasses the cache
  entirely. `buildSemanticCache` in `main.go` constructs the singleton.

**What's load-bearing:**

- **The planner is a pure function.** Its inputs are the pin row, the
  fresh `router.Decision`, an estimated token count, and the
  available-model set resolved at boot. No DB lookups, no provider
  calls. Tests cover the EV math without spinning anything up.
- **Capability tiers are hand-maintained.** Deriving them from price
  was rejected because it would silently move models on every pricing
  change. Every deployed model must have an entry in
  `capability.Table`; `Validate()` enforces this at boot.
- **Cache-read multipliers are per-provider, not global.** A single
  global multiplier makes cross-provider switches (opus → gpt-5)
  economically wrong. Read multipliers via
  `pricing.Pricing.EffectiveCacheReadMultiplier`, never the bare
  struct field.
- **The session-pin store interface is in the inner ring; the impl is
  in `internal/postgres`.** The proxy service is unit-tested with an
  in-memory fake (`internal/proxy/service_test.go`); the Postgres
  adapter is exercised end-to-end via the docker-compose stack.
- **`OnUpstreamMeta` callbacks** let provider adapters report streaming
  usage back to the proxy without coupling provider packages to
  proxy internals. The pricing / planner stack depends on per-turn
  token counts being recorded promptly; don't add a provider that
  forgets to call the callback.

**What to NOT do:**

- **Don't move provider-call logic into the planner.** The planner
  must remain pure so we can prove correctness of the EV math.
  Anything that needs a network call goes in `proxy.Service`.
- **Don't add a handover path that doesn't time out.** The summarizer
  contract says implementations MUST respect the context's deadline.
  Falling back to `TrimLastN` on timeout is the correct behavior, not
  a bug; do not "fix" it by waiting longer.
- **Don't cache streaming responses.** Streaming bypasses the cache
  on purpose — captured bytes would be post-translation SSE frames
  and the lookup latency budget is hostile to first-token-time. If
  you think we should change this, write a doc first.
- **Don't put pricing data in two places.** `pricing.Pricing` is the
  single source of truth. The OTel emitter and the planner both read
  the same map.

## Anthropic usage-bypass gate

[`internal/proxy/usage`](internal/proxy/usage) tracks the most recent
Anthropic unified rate-limit utilization (the same data the
`claude /usage` CLI reads off `anthropic-ratelimit-unified-{5h,weekly}-*`
response headers).

When `ROUTER_USAGE_BYPASS_ENABLED=true`, requests whose recorded 5h
and weekly utilization are both below `ROUTER_USAGE_BYPASS_THRESHOLD`
(default `0.95`) pass straight through to Anthropic with the requested
model — no cluster routing, no planner verdict. Once either window
crosses the threshold, the gate disengages for that credential and the
cluster scorer takes over. Observations expire after
`ROUTER_USAGE_OBSERVATION_TTL` (default 10 minutes); a torn-down key
or a long idle period falls back to "cold start = bypass" rather than
pinning the gate open on a stale near-100% reading.

The observer is pure in-memory state with no persistence; entries are
keyed by `usage.CredentialKey` (a salted hash of the upstream API key
bytes) so logs and metrics never see the raw token. A periodic sweep
bounds memory by evicting expired entries.

This gate exists because Anthropic-plan customers (Claude Code's
logged-in flow) want their unused quota spent on Anthropic, not
silently redirected to a cheaper substitute, until they're actually
approaching the cap.

## Deployment modes

`ROUTER_DEPLOYMENT_MODE` is read at boot in `cmd/router/main.go`:

- **`selfhosted`** (default): the full dashboard at `/ui/*`, the
  `/admin/v1/*` API (auth, metrics, keys, provider-keys, config,
  excluded-models), and dashboard cookie auth are all mounted.
  Provider keys are read from env vars; missing keys keep providers
  registered for client-passthrough but exclude them from hard-pin
  resolution.
- **`managed`**: dashboard and `/admin/v1/*` are not mounted at all
  — Weave-managed deployments have a separate control plane. Every
  provider is registered with an empty deployment key; the proxy
  service is in BYOK-only mode, so a request without BYOK or
  client-supplied auth for the chosen provider 400s rather than
  silently spending the platform budget. Setting the variable to any
  other value panics at boot.

When adding a new endpoint, put it inside the `selfhosted` block in
`server.Register` unless it's part of the product surface
(`/v1/*`, `/v1beta/*`, `/health`, `/validate`). Do not re-expose the
admin surface in managed mode.

## Eval harness (router/eval/)

Phase 1a's go/no-go gate. Sibling Poetry package to `router/scripts/`,
runs as a Modal app (`modal_app.py`); see [`eval/README.md`](eval/README.md).

**Per-request router selection:**

- `internal/server/middleware.WithClusterVersionOverride` reads the
  `x-weave-cluster-version: v0.X` header and stashes the version on
  the request context. `cluster.Multiversion.Route` reads it via
  `cluster.VersionFromContext` and dispatches to the matching `Scorer`.
  Customer traffic (no header) always serves the deployment's default
  version (`ROUTER_CLUSTER_VERSION` → `artifacts/latest`).
- `internal/server/middleware.WithEmbedOnlyUserMessageOverride` honors
  the `x-weave-embed-only-user-message: true|false` header, flipping
  the proxy between embedding user-role text only (default) and the
  concatenated turn stream. Used for orthogonal feature-extraction
  A/Bs against the artifact-version axis.
- The eval harness names cluster routers as `vX.Y-cluster` — any
  committed artifact directory under
  `internal/router/cluster/artifacts/` is reachable by name, no Python
  Literal updates needed. `eval/types.py::CLUSTER_ROUTER_PATTERN` is
  the regex; `parse_cluster_router` returns
  `(version, last_user_flag)` and `routing.py` translates that into
  the two staging headers.

**What to NOT do:**

- **Do NOT re-introduce a heuristic-vs-cluster A/B switch.** The
  heuristic was retired because its silent-fallback behavior masked
  cluster regressions. If you need to compare strategies, ship the
  alternate strategy as another `internal/router/X` package and
  promote on its own merits, not as a runtime fallback.
- **Do NOT add a runtime override that picks a specific model
  directly.** The whole point is to compare *router strategies*, not
  to do per-request model overrides — that's a different feature
  (P1, see `docs/plans/archive/CLUSTER_ROUTING_PLAN.md` open questions).

## Quick reference for common questions

- **"Where does feature X go?"** Pick by responsibility:
  - Identity / API-key concerns → method on `*auth.Service`.
  - Routing / dispatch / cross-format proxying / per-turn orchestration
    → method on `*proxy.Service`.
  - Wire-format conversion (no I/O) → function in `internal/translate/`.
  - Pure routing-policy math (EV, tier, pricing) → new helper in the
    matching `internal/router/<name>/` inner-ring subpackage, never
    in the proxy service.
  Add a new repo interface method (if it touches the DB) and an adapter
  implementation. The handler just adapts HTTP.
- **"Should this be in `auth`/`proxy`/`translate`/`config`/`observability`
  or in the package that uses it?"** In the package that uses it, unless
  three or more packages need the same logic. The shared homes are not
  catch-alls.
- **"How do I add a new OpenAI-compatible upstream?"** Add a `*BaseURL`
  constant to `internal/providers/openaicompat`, a provider-name
  constant + env-var entry to `internal/providers/provider.go`, and a
  registration block in `cmd/router/main.go`. No new adapter package.
- **"Should I commit `internal/sqlc/`?"** Yes. The Dockerfile and CI
  builds depend on the generated code being present. Run `make generate`
  before committing migration or query changes.
- **"How do I run a one-off query against the local DB?"**
  `docker compose exec postgres psql -U router -d router`. The migrate
  step has already applied the schema. (The router lives in the
  `router` schema; the pool's `AfterConnect` hook pins `search_path`
  so accidental writes to `public.*` are impossible.)
