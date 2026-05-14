# router — AGENTS

> **Mirror notice.** Verbatim sync with [CLAUDE.md](CLAUDE.md). Cursor + generic agents read `AGENTS.md`; Claude Code reads `CLAUDE.md`. **Update both together** — divergence = bug.

Instructs AI agents in `router/` subproject. Covers router-specific layering + design conventions. First read for any task: [README](README.md), then this file.

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
- **`internal/api/*` and `internal/server`** depend on `internal/auth` (Service handle + middleware-context types) and `internal/proxy` (routing/dispatch service handle). May import `internal/observability` for logging, `internal/providers` for shared sentinel errors, `internal/router/cluster` for `ErrClusterUnavailable` sentinel + `DeployedModelsSource` interface (API handlers map sentinel → HTTP 503). Must not import `internal/postgres`, any concrete `internal/providers/*` adapter, or `internal/translate` directly. Concrete instances reach presentation only via constructor params from composition root. (`internal/router/heuristic` and `internal/router/evalswitch` previously lived here; both removed when heuristic fallback retired in favor of `cluster.ErrClusterUnavailable` → HTTP 503.)
- **`internal/config` and `internal/observability` are leaf utilities** — must not import any other package under `internal/`. Third-party utility deps fine; today pull only stdlib + gin (request-scoped logger middleware). `internal/observability/otel` subpackage *is* an adapter (builds real OTLP exporter) and can import other internal packages; parent `internal/observability` stays a leaf.
- **Composition happens in `cmd/router/main.go`.** Only file that constructs concrete adapters + injects them. No other place wires things. Keep `main.go` focused on wiring; helpers today: `buildClusterScorer` (per-version Scorer assembly + embedder warmup), `buildSemanticCache` (response-cache assembly), `buildOtelEmitter` (OTel span exporter), `runSessionPinSweep` (TTL sweep loop), `resolveHardPinModel` / `resolveDefaultBaselineModel` / `resolveAvailableModels` (boot-time model resolution), small env parsers. No more heuristic-fallback router — if cluster routing fails to boot, `main.go` panics.

If wanting to import something that violates these rules, design is wrong — surface as interface in appropriate inner-ring package and implement in adapter subpackage.

## Adding code — step-by-step recipes

### Adding an HTTP endpoint

1. **Decide timeout budget.** Cheap auth-only ops use `validateTimeout` / `healthTimeout` (1 s). Provider calls get own constant in [`server.go`](internal/server/server.go) — pick budget + justify in comment.
2. **Decide auth.** Routes needing valid `rk_` bearer go through `middleware.WithAuth(authSvc)`. Admin endpoints use `WithAdminOrAuth` (admin cookie OR bearer) or `WithAdminOnly` (admin cookie only). Unauthed routes (e.g. `/health`) attach no auth middleware.
3. **Decide if self-hoster dashboard surface.** `/ui/*` static dashboard, `/admin/v1/auth/*`, `/admin/v1` mgmt group (metrics, keys, provider-keys, config, excluded-models) mount only when `mode == server.DeploymentModeSelfHosted`. New endpoints whose only consumer is self-hosted dashboard go inside that block; product-surface endpoints (`/v1/*`, `/v1beta/*`, `/health`, `/validate`) stay outside so available in `managed` mode too. **Do not** add new `/admin/v1/*` route outside selfhosted block — would re-expose redundant control plane on Weave-managed deploys.
4. **Pick (or create) right `internal/api/<group>/` subpackage.** Operational endpoints → `internal/api/admin/`; Anthropic Messages surface → `internal/api/anthropic/` (also hosts `/v1/route` for routing introspection + passthrough endpoints); OpenAI Chat Completions → `internal/api/openai/`; Gemini native `:generateContent` / `:streamGenerateContent` → `internal/api/gemini/`. New surfaces get own subpackage.
5. **Use `observability.FromGin(c)` for request-scoped logger.** If need authed installation: `middleware.InstallationFrom(c)` (nil if `WithAuth` not applied — handler should be on authed group). For BYOK secrets attached to request: `middleware.ExternalAPIKeysFrom(c)`.
6. **Pick right service.** Identity-only ops → `*auth.Service`. Routing/dispatch/translate → `*proxy.Service`. Don't touch repositories, router, providers, planner/handover/cache packages from handler. Handler adapts HTTP ↔ service; service does the work.
7. **Test with in-memory fakes + gin testing harness** (`httptest.NewRequest`/`ResponseRecorder`). No real DB for handler tests — use fakes from [`internal/auth/service_test.go`](internal/auth/service_test.go) and [`internal/proxy/service_test.go`](internal/proxy/service_test.go) as model.

### Adding a method to a Service

Pick service first:

- **Identity / API-key** → `*auth.Service` in [`internal/auth/service.go`](internal/auth/service.go).
- **Routing / dispatch / cross-format proxying / planner integration** → `*proxy.Service` in [`internal/proxy/service.go`](internal/proxy/service.go).

Then:

1. **Define method on chosen `*Service`.** No I/O directly here — push into repo or provider adapter. Inner-ring imports (`router`, `providers`, `translate`, `observability`, `internal/router/*` helper packages, `internal/proxy/usage`) + small utility libs fine.
2. **If need new repo methods, add to interfaces in [`installation.go`](internal/auth/installation.go) / [`api_key.go`](internal/auth/api_key.go) / sibling files.** Interface = contract; adapter must satisfy. `sessionpin.Store` interface in [`internal/router/sessionpin/store.go`](internal/router/sessionpin/store.go), implemented by `postgres.SessionPinRepository`.
3. **Implement new repo method in [`internal/postgres/repository.go`](internal/postgres/repository.go)** (or sibling in `internal/postgres/`), adding SQLC query in `db/queries/`. Run `make generate` to regenerate `internal/sqlc/`.
4. **Update matching `service_test.go` fakes** to satisfy expanded interface. Tests for new Service method use fakes; assert on real return values, not just that mocks were called.

### Adding a wire-format pair (translation)

When new inbound format needs to talk to existing upstream provider with different wire format:

1. **Add conversion functions to [`internal/translate/`](internal/translate).** Pure functions only — no I/O, no provider knowledge, no domain types. Package covers all three directions: Anthropic ⇄ OpenAI and Gemini ⇄ {Anthropic, OpenAI} via `RequestEnvelope` intermediate + per-target `emit_*.go` files.
2. **If response streaming, adapt [`stream.go`](internal/translate/stream.go) / [`gemini_stream.go`](internal/translate/gemini_stream.go)** or add sibling decorator. Decorators wrap `http.ResponseWriter` and translate on the fly so we never buffer entire responses. Use [`internal/sse`](internal/sse) for zero-alloc SSE framing.
3. **Compose new translation in `proxy.Service.Proxy*`.** Proxy service is only caller of `translate`. Keep providers (`internal/providers/<name>/`) ignorant of cross-format concerns.

### Adding a new `providers.Client` adapter

1. **Create `internal/providers/<name>/client.go`** with `Client` struct + `NewClient(...)` constructor taking credentials (typically API key string + base URL). For OpenAI-compatible upstreams (vLLM, Together, DeepInfra, customer endpoints), prefer adding sibling `*BaseURL` constant in [`internal/providers/openaicompat`](internal/providers/openaicompat) over new adapter — openaicompat client already covers OpenRouter + Fireworks under own provider keys.
2. **Implement `Proxy` and `Passthrough`.** Adapter translates prepared request body to provider's wire format, sends with pooled `http.Client` (use `httputil.NewTransport` and `httputil.StreamBody` from `internal/providers/httputil/`), streams response back. Adapters call into `proxy.OnUpstreamMeta` when they observe usage/header data to record. Do not leak provider-specific types across package boundary.
3. **Add compile-time check:** `var _ providers.Client = (*Client)(nil)`
4. **Add canonical name constant** to [`internal/providers/provider.go`](internal/providers/provider.go) (the `Provider*` block) + register matching env-var name in `APIKeyEnvVars`. Today's wired keys: `"anthropic"`, `"openai"`, `"google"`, `"openrouter"`, `"fireworks"`. Composition root reads `APIKeyEnvVars` so admin `/config` view can't drift from actual wiring.
5. **Wire in `cmd/router/main.go`.** Only place that imports provider package directly. Provider must be added to `providerMap` regardless of mode; `envKeyedProviders` (parallel set) tracks which providers have deployment-level key configured so hard-pin resolver knows what's safe to pin to. Managed-mode deploys register every provider with empty key + rely exclusively on BYOK / client-supplied auth.

### Adding a new `router.Router` implementation

1. **Create sibling subpackage to `internal/router/cluster/`.** Today `cluster/` (AvengersPro, with `Multiversion` wrapper for per-request bundle selection) is only `Router` impl in prod. New ones might be e.g. `internal/router/shadow/` wrapping two others.
2. **Implement `Route(ctx, router.Request) (router.Decision, error)`.**
3. **Add compile-time check:** `var _ router.Router = (*X)(nil)`.
4. **Wire in `cmd/router/main.go`** (replacing or wrapping cluster scorer as needed).
5. **If impl needs CGO or external libs**, follow build-tag pattern in `cluster/embedder_onnx.go` + `embedder_stub.go` so contributors without library can still `go test` locally.
6. **Failure modes return errors, not silent fallbacks.** Cluster scorer's `ErrClusterUnavailable` → HTTP 503 pattern is the model: silent fallback to default model masks regressions + lets quality silently degrade in eval + prod.

### Adding a column or query

1. **Migration first.** Add `db/migrations/NNNN_<name>.up.sql` + `.down.sql` in sequential numbering. Wrap in `BEGIN`/`COMMIT`. Down migration must be precise rollback — no `IF EXISTS` guards.
2. **Add query** to appropriate `db/queries/<table>.sql`. Use named params with type casts (`@param::varchar`). Use `sqlc.embed(t)` for JOINs.
3. **Run `make generate`** to regenerate `internal/sqlc/`. Commit generated code alongside changes.
4. **Update [`internal/postgres/repository.go`](internal/postgres/repository.go)** (and [`converters.go`](internal/postgres/converters.go) if new column needs domain mapping). Domain types (`auth.Installation`, `auth.APIKey`, `sessionpin.Pin`) must not leak `pgtype` / `uuid` concerns — convert at adapter boundary.

### Adding a doc under `docs/`

Every Markdown doc under `router/docs/` (active or archived) indexed in [`docs/README.md`](docs/README.md). When adding new doc, same change must update index — drift between doc tree and index = bug.

1. **Top of new file:** include standard two-line header before H1:

   ```
   Created: YYYY-MM-DD
   Last edited: YYYY-MM-DD
   ```

   `Created` date is load-bearing — `docs/README.md` orders TOC by it. Don't backdate; if doc takes multiple days, leave `Created` on day it first landed and bump `Last edited` as it changes.

2. **Append row to [`docs/README.md`](docs/README.md)** in correct section (Active or Archived), keeping each table sorted by `Created` ascending. Write one- or two-sentence summary covering what doc is for and (for archived) why archived plus link to active replacement.

3. **If archiving active doc:** move row from active table to archived with short reason, mirror entry in [`docs/plans/archive/README.md`](docs/plans/archive/README.md). Move file with `git mv` so history follows.

4. **Renaming or deleting doc:** update both this rule's index and inbound links. `grep -rn 'old/path' router/` before merging.

### Adding a new helper

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

### SQL and migrations

- Always named params (`@param::varchar`), never numbered (`$1`).
- Always include type casts so SQLC inference unambiguous.
- Query names use consistent prefixes: `Insert*`, `Upsert*`, `Get*`, `Update*`, `Delete*`.
- Every query gets explanatory comment (SQLC turns into godoc on generated function).
- No-rows single-row queries return error — check `errors.Is(err, sql.ErrNoRows)`.
- Always wrap migrations in `BEGIN; ... COMMIT;`.
- Never create migration files manually — use `make migrate-create NAME=<name>`.
- Down migrations must be precise rollbacks. No `IF EXISTS` guards. Don't separately drop indexes when dropping tables.
- `organization_id` + `created_by` = opaque external identifiers — never add foreign keys to tables outside router's own schema.
- Soft-delete via `deleted_at TIMESTAMP` on tables needing lifecycle. Hot-path queries filter `WHERE deleted_at IS NULL`.

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

## Cluster routing (P0)

`internal/router/cluster` = AvengersPro-derived primary router (arxiv 2508.12631, DAI 2025). Full design in [`docs/plans/archive/CLUSTER_ROUTING_PLAN.md`](docs/plans/archive/CLUSTER_ROUTING_PLAN.md); this section is rules-for-AI subset.

**What's load-bearing:**

- Package compiles in **two layered modes via build tags**:
  - `embedder_onnx.go` vs `embedder_stub.go` — gated by `no_onnx`. Default builds compile real hugot-backed embedder; `-tags=no_onnx` swaps in stub `NewEmbedder` that always errors. Used by contributors without `libonnxruntime`.
  - `-tags ORT` — required by **hugot v0.7+** to enable ONNX Runtime backend. Without it, `hugot.NewORTSession` returns "to enable ORT, run `go build -tags ORT`" and `cluster.NewEmbedder` fails. Dockerfile builds with `-tags ORT`. **Do not drop this tag from any production-bound build.**
  - To run parity integration test, combine: `-tags "onnx_integration ORT"`.
- Local-dev build env (Apple Silicon):
  - `libtokenizers` static lib must be on linker path. Pre-built releases at https://github.com/daulet/tokenizers/releases/. Extract `libtokenizers.darwin-arm64.tar.gz` somewhere user-writable + set `CGO_LDFLAGS=-L/path/to/dir`.
  - `libonnxruntime` shared lib via `brew install onnxruntime`. brew installs to `/opt/homebrew/lib`, hugot defaults to `/usr/local/lib` lookup. Set `ROUTER_ONNX_LIBRARY_DIR=/opt/homebrew/lib` to override. (Linux containers using Dockerfile don't need this — `/usr/lib/libonnxruntime.so` is default, populated by runtime stage.)
- **Versioned artifacts.** Every committed bundle lives at `internal/router/cluster/artifacts/v<X.Y>/` with four files: `centroids.bin`, `rankings.json`, `model_registry.json`, `metadata.yaml`. `artifacts/latest` pointer file (single line, e.g. `v0.37`) names version runtime serves by default; `ROUTER_CLUSTER_VERSION` env overrides. Promotion = one-line edit to `latest` + redeploy. Committed history spans v0.21 through current `latest` — earlier pruned once they fell out of eval comparison.
- Go runtime builds **only served default version** by default (`cmd/router/main.go`'s `buildClusterScorer`). Setting `ROUTER_CLUSTER_BUILD_ALL_VERSIONS=true` switches to building **one Scorer per committed bundle** so callers can pin per-request to sibling version with `x-weave-cluster-version: v0.X` via `middleware.WithClusterVersionOverride`. "Compare-against-each-other" mechanism — staging/eval deploys set flag so single deploy carries every committed bundle + eval harness flips between them per-request. Prod leaves flag off: only default bundle loaded into memory, header override is no-op.
- **Centroids/rankings are write-once.** `train_cluster_router.py` always writes to `artifacts/v<X.Y>/` and never overwrites previous version (auto-bumps from `latest` when `--version` omitted). Pass `--from v0.36` to clone previous version's `model_registry.json` before training new one. **Never edit `centroids.bin` / `rankings.json` by hand.** `model_registry.json` is only hand-editable file in a bundle (training script reads it).
- `metadata.yaml` is informational at runtime — carries version changelog, training params, deployed models, α-blend cost values. Go runtime parses for `/health`-style provenance; eval harness reads offline. Keep accurate but does not affect routing decisions.
- `assets/model.onnx` is **NOT in git.** Use Jina's own INT8 export at `jinaai/jina-embeddings-v2-base-code`, file path `onnx/model_quantized.onnx`. Dockerfile pulls anonymously during build (Jina repo public — self-hosters don't need creds); local dev pulls via `scripts/download_from_hf.py`. `HF_TOKEN` build secret is *optional* (raises rate limits in CI) + `required=false` in Dockerfile. Go embedder reads from `/opt/router/assets/model.onnx` (override via `ROUTER_ONNX_ASSETS_DIR`). If missing or <1 MiB, `cluster.NewEmbedder` errors at boot + `main.go` panics — router refuses to start rather than silently degrading. `HF_MODEL_REVISION` pinned to Jina SHA by default; bump deliberately to pick up new upstream export.
- **Cost values** used in α-blend live in `train_cluster_router.py`'s `DEFAULT_COST_PER_1K_INPUT`. Baked into `rankings.json` at training time, not looked up at request time (paper §3 — runtime scoring is single argmax). When Anthropic changes prices, update dict + rerun training.

**What to NOT do:**

- **Don't add per-request cost lookup or runtime α knob.** α baked at training time; changing requires retraining. Per-request override (`x-weave-routing-alpha`) is P1, not P0 — wait for customer ask before shipping.
- **Don't loosen `MaxPromptChars = 1024` cap** without re-running latency test. BERT inference is O(n²) attention; cap is load-bearing.
- **Don't add fail-open fallbacks.** Cluster scorer returns `ErrClusterUnavailable` on every failure path (embed timeout, embed error, dim mismatch, prompt too short, empty argmax). API handlers map to HTTP 503. Previous `heuristic` fallback removed because it silently degraded routing — every request that should have hit cluster scorer instead got `claude-haiku-4-5`, masking real regressions in eval + prod. New failure modes return sentinel; no default-model shortcut "for safety".
- **Don't change centroid format without bumping magic string.** `loadCentroids` uses magic + version header to refuse mismatched binaries; if layout changes, bump `centroidsMagic` from `CRT1` to `CRT2` so next deploy refuses old binary instead of silently misrouting.
- **Don't overwrite previously committed artifact version.** Versions frozen for comparison — once `v0.37` committed, train to `v0.38` rather than re-running `train_cluster_router.py` against `v0.37`. Training script auto-bumps; only override with `--version v0.X` for in-place fixes intended to land as separate commit.
- **Don't bypass version pointer.** `artifacts/latest` is single source of truth for default served version. Don't hardcode version in `cmd/router/main.go`; let `cluster.ResolveVersion` read pointer.


## Multi-provider routing (Anthropic / OpenAI / Google / OpenRouter / Fireworks)

Router serves five vendor pools from one composition root. Single routing decision picks `(Provider, Model)` + proxy dispatches accordingly. Cross-format translation lives in `internal/translate` so handlers never juggle wire formats.

**Architecture:**

- `cmd/router/main.go` registers each provider client. In **selfhosted** mode each provider's deployment-level key read from env var (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GOOGLE_API_KEY`, `OPENROUTER_API_KEY`, `FIREWORKS_API_KEY`); missing key keeps provider registered for BYOK / client-passthrough but excludes from `envKeyedProviders` (set hard-pin resolver may pin to). In **managed** mode (`ROUTER_DEPLOYMENT_MODE=managed`) every provider registered unconditionally with empty key + proxy service flipped into BYOK-only mode — request without BYOK or client-supplied creds for chosen provider 400s at scorer rather than silently spending platform budget on customer traffic. Single source of truth for provider→env-var mapping = `providers.APIKeyEnvVars` in [`internal/providers/provider.go`](internal/providers/provider.go).
- `cluster.NewScorer` filters `model_registry.json`'s `deployed_models` list to entries whose `provider` is in `availableProviders`. argmax runs over filtered list, so deploy without OpenRouter key cannot accidentally emit `deepseek-*` decision.
- `model_registry.json` = flat list of `{model, provider, bench_column, proxy?, proxy_note?}` entries. Direct columns are 1:1 with OpenRouterBench; proxy entries (`proxy: true`) reuse another column's score until direct ranking data available. Training script copies bench-column scores onto every deployed entry referencing that column rather than averaging columns — that's how scorer can rank `gpt-5` and proxy `claude-opus-4-7` distinctly.
- `internal/translate` has all three directions (Anthropic ⇄ OpenAI ⇄ Gemini) routed through `RequestEnvelope` intermediate. Streaming decorators in `stream.go` / `gemini_stream.go` translate SSE events on fly. `proxy.Service` is only caller of `translate`.
- **`internal/providers/google` ships native Generative Language REST client** (`NativeClient` against `https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent`). OpenAI-compat surface at `/v1beta/openai` does **not** preserve opaque `thought_signature` field that multi-turn tool use against Gemini 3.x preview models requires, so native client is mandatory for those flows. Auth via `x-goog-api-key` header. Old OpenAI-compat path remains in file but no longer wired into `main.go`.
- `internal/providers/openaicompat` = generic OpenAI Chat Completions adapter, used today for OpenRouter (`https://openrouter.ai/api/v1`) + Fireworks (`https://api.fireworks.ai/inference/v1`). New OpenAI-compatible upstreams (vLLM, Together, DeepInfra, customer-hosted) should plug in here under new provider-name constant rather than getting own adapter package.
- `internal/providers/noop` = placeholder client returning `providers.ErrNotImplemented`. Use when wiring new `availableProviders` key whose adapter hasn't landed yet so cluster scorer can already filter against it.

**What is load-bearing:**

- **Training script is only writer of `rankings.json`.** Hand-editing breaks cluster geometry guarantee (`scorer.go`'s sorted-candidate ordering must match what training produced). Re-run `train_cluster_router.py` after touching `model_registry.json` + commit regenerated artifact.
- **Cluster scorer is availability-aware at boot, not request time.** Filter happens in `NewScorer`; runtime argmax unchanged. Empty filtered set = hard boot error so misconfigured deploys fail loud.
- **Anthropic-only fields (`thinking`, `cache_control`, `metadata`, Anthropic beta headers) stripped at translation time + again defensively in OpenAI / openaicompat adapters.** Keep both checks — belt-and-suspenders intentional because field set drifts as Anthropic adds beta features.

**What to NOT do:**

- **Don't bypass provider filter.** If need to route to provider whose key isn't registered (and not reachable via BYOK or client passthrough), register provider — don't add special-case path that ignores `availableProviders`.
- **Don't add bench-column averaging back to training script.** 1:1 mapping is the point. Two entries that share column copy same score; they don't average across columns.
- **Don't route Gemini 3.x preview tool-use through OpenAI-compat surface.** Loses `thought_signature` and second turn 400s. Use `google.NewNativeClient` (already wired default).
- **Don't add per-installation provider preference yet.** Deploy-time env config + cluster scorer's per-prompt argmax cover v1 use cases; per-installation routing = follow-up PR with DB migration on `model_router_installations`.

## Cache-aware turn routing (planner / handover / session pin / cache)

Proxy's per-turn flow is more than "scorer → dispatch". Pinned session, planner verdict, optional handover summary, semantic response cache all sit between inbound request and upstream provider. Packages intentionally small + single-purpose so each unit-testable without others.

**Packages:**

- [`internal/router/sessionpin`](internal/router/sessionpin) — `Pin` type + `Store` interface for sticky per-session routing. Keyed by `(api_key_id, session_key, role)` where `session_key` = 16-byte sha256 truncation derived from inbound request (see [`internal/proxy/session_key.go`](internal/proxy/session_key.go)). Stage 1 emits `role="default"` only; column exists so turn-type detector can land role-conditioned pinning without schema change. Postgres adapter in `internal/postgres/`; `runSessionPinSweep` in `main.go` runs TTL sweep loop.
- [`internal/router/turntype`](internal/router/turntype) — classifies inbound requests into `MainLoop`, `ToolResult`, `SubAgentDispatch`, `Compaction`, `Probe`. Used by proxy to short-circuit to session pin on tool-result turns (whose embeddings are mostly noise), force Haiku on compaction turns, bypass routing entirely on probe turns. Pure, no I/O.
- [`internal/router/capability`](internal/router/capability) — hand-maintained `Tier` table (Low / Mid / High) for each deployed model. Used by planner to overturn cost-driven "stay" when fresh decision is in strictly higher capability tier than pin. `Validate()` called at boot so any deployed model missing tier entry fails build loudly rather than silently bypassing guard.
- [`internal/router/pricing`](internal/router/pricing) — per-model USD pricing + per-model cache-read multipliers (Anthropic 0.10, OpenAI 0.50, Gemini 0.25, DeepSeek 0.10; `DefaultCacheReadMultiplier = 0.5` for unspecified). Pure data + lookup helpers; OTel layer also reads this so cost attributes on spans can't drift from what planner used.
- [`internal/router/planner`](internal/router/planner) — Prism-style cache-aware EV policy. Per turn, decides STAY (preserve pinned model's upstream prompt cache) vs SWITCH (take cluster scorer's fresh decision + eat one-time cache miss). Math compares expected per-turn savings over remaining horizon against eviction cost of warming new cache; tier-upgrade guard fires when STAY would clearly under-serve prompt. Pure function of `(pin, fresh decision, estimated tokens, available models)`; no I/O.
- [`internal/router/handover`](internal/router/handover) — `Summarizer` interface + envelope-rewrite helpers. When planner decides SWITCH, proxy asks small model to summarize prior conversation + rewrites message history to `[synthesizedSummary, latestUser]` before dispatching to new model. Bounds switch turn's input cost regardless of session length. Provider-backed implementation in `internal/proxy/handover.go`; inner-ring package only defines contract. On summarizer timeout or error, proxy falls back to `handover.TrimLastN`.
- [`internal/router/cache`](internal/router/cache) — cross-request semantic response cache. Short-circuits near-duplicate non-streaming requests by cosine similarity on cluster scorer's prompt embedding; captured wire-format bytes replayed without invoking upstream. Per-(installation, inbound-format) isolation, since captured bytes are post-translation. Streaming bypasses cache entirely. `buildSemanticCache` in `main.go` constructs singleton.

**What's load-bearing:**

- **Planner is pure function.** Inputs = pin row, fresh `router.Decision`, estimated token count, available-model set resolved at boot. No DB lookups, no provider calls. Tests cover EV math without spinning anything up.
- **Capability tiers hand-maintained.** Deriving from price rejected because would silently move models on every pricing change. Every deployed model must have entry in `capability.Table`; `Validate()` enforces at boot.
- **Cache-read multipliers per-provider, not global.** Single global multiplier makes cross-provider switches (opus → gpt-5) economically wrong. Read multipliers via `pricing.Pricing.EffectiveCacheReadMultiplier`, never bare struct field.
- **Session-pin store interface in inner ring; impl in `internal/postgres`.** Proxy service unit-tested with in-memory fake (`internal/proxy/service_test.go`); Postgres adapter exercised end-to-end via docker-compose stack.
- **`OnUpstreamMeta` callbacks** let provider adapters report streaming usage back to proxy without coupling provider packages to proxy internals. Pricing / planner stack depends on per-turn token counts being recorded promptly; don't add provider that forgets to call callback.

**What to NOT do:**

- **Don't move provider-call logic into planner.** Planner must remain pure so we can prove correctness of EV math. Anything needing network call goes in `proxy.Service`.
- **Don't add handover path that doesn't time out.** Summarizer contract says implementations MUST respect context deadline. Falling back to `TrimLastN` on timeout is correct, not a bug; do not "fix" by waiting longer.
- **Don't cache streaming responses.** Streaming bypasses cache on purpose — captured bytes would be post-translation SSE frames + lookup latency budget is hostile to first-token-time. If you think we should change, write a doc first.
- **Don't put pricing data in two places.** `pricing.Pricing` is single source of truth. OTel emitter + planner both read same map.

## Anthropic usage-bypass gate

[`internal/proxy/usage`](internal/proxy/usage) tracks most recent Anthropic unified rate-limit utilization (same data `claude /usage` CLI reads off `anthropic-ratelimit-unified-{5h,weekly}-*` response headers).

When `ROUTER_USAGE_BYPASS_ENABLED=true`, requests whose recorded 5h + weekly utilization are both below `ROUTER_USAGE_BYPASS_THRESHOLD` (default `0.95`) pass straight through to Anthropic with requested model — no cluster routing, no planner verdict. Once either window crosses threshold, gate disengages for that credential + cluster scorer takes over. Observations expire after `ROUTER_USAGE_OBSERVATION_TTL` (default 10 minutes); torn-down key or long idle period falls back to "cold start = bypass" rather than pinning gate open on stale near-100% reading.

Observer is pure in-memory state with no persistence; entries keyed by `usage.CredentialKey` (salted hash of upstream API key bytes) so logs + metrics never see raw token. Periodic sweep bounds memory by evicting expired entries.

Gate exists because Anthropic-plan customers (Claude Code's logged-in flow) want unused quota spent on Anthropic, not silently redirected to cheaper substitute, until actually approaching cap.

## Deployment modes

`ROUTER_DEPLOYMENT_MODE` read at boot in `cmd/router/main.go`:

- **`selfhosted`** (default): full dashboard at `/ui/*`, `/admin/v1/*` API (auth, metrics, keys, provider-keys, config, excluded-models), dashboard cookie auth all mounted. Provider keys read from env vars; missing keys keep providers registered for client-passthrough but exclude from hard-pin resolution.
- **`managed`**: dashboard + `/admin/v1/*` not mounted at all — Weave-managed deploys have separate control plane. Every provider registered with empty deployment key; proxy service in BYOK-only mode, so request without BYOK or client-supplied auth for chosen provider 400s rather than silently spending platform budget. Setting variable to any other value panics at boot.

When adding new endpoint, put inside `selfhosted` block in `server.Register` unless part of product surface (`/v1/*`, `/v1beta/*`, `/health`, `/validate`). Do not re-expose admin surface in managed mode.

## Eval harness (router/eval/)

Phase 1a's go/no-go gate. Sibling Poetry package to `router/scripts/`, runs as Modal app (`modal_app.py`); see [`eval/README.md`](eval/README.md).

**Per-request router selection:**

- `internal/server/middleware.WithClusterVersionOverride` reads `x-weave-cluster-version: v0.X` header + stashes version on request context. `cluster.Multiversion.Route` reads via `cluster.VersionFromContext` + dispatches to matching `Scorer`. Customer traffic (no header) always serves deployment's default version (`ROUTER_CLUSTER_VERSION` → `artifacts/latest`).
- `internal/server/middleware.WithEmbedOnlyUserMessageOverride` honors `x-weave-embed-only-user-message: true|false` header, flipping proxy between embedding user-role text only (default) + concatenated turn stream. Used for orthogonal feature-extraction A/Bs against artifact-version axis.
- Eval harness names cluster routers as `vX.Y-cluster` — any committed artifact directory under `internal/router/cluster/artifacts/` reachable by name, no Python Literal updates needed. `eval/types.py::CLUSTER_ROUTER_PATTERN` is regex; `parse_cluster_router` returns `(version, last_user_flag)` + `routing.py` translates into two staging headers.

**What to NOT do:**

- **Do NOT re-introduce heuristic-vs-cluster A/B switch.** Heuristic retired because silent-fallback behavior masked cluster regressions. If need to compare strategies, ship alternate strategy as another `internal/router/X` package + promote on own merits, not as runtime fallback.
- **Do NOT add runtime override that picks specific model directly.** Whole point is comparing *router strategies*, not per-request model overrides — different feature (P1, see `docs/plans/archive/CLUSTER_ROUTING_PLAN.md` open questions).

## Quick reference for common questions

- **"Where does feature X go?"** Pick by responsibility:
  - Identity / API-key → method on `*auth.Service`.
  - Routing / dispatch / cross-format proxying / per-turn orchestration → method on `*proxy.Service`.
  - Wire-format conversion (no I/O) → function in `internal/translate/`.
  - Pure routing-policy math (EV, tier, pricing) → new helper in matching `internal/router/<name>/` inner-ring subpackage, never proxy service.
  Add new repo interface method (if touches DB) + adapter impl. Handler just adapts HTTP.
- **"Should this be in `auth`/`proxy`/`translate`/`config`/`observability` or in package that uses it?"** In package that uses it, unless 3+ packages need same logic. Shared homes not catch-alls.
- **"How do I add new OpenAI-compatible upstream?"** Add `*BaseURL` constant to `internal/providers/openaicompat`, provider-name constant + env-var entry to `internal/providers/provider.go`, registration block in `cmd/router/main.go`. No new adapter package.
- **"Should I commit `internal/sqlc/`?"** Yes. Dockerfile + CI builds depend on generated code being present. Run `make generate` before committing migration or query changes.
- **"How do I run one-off query against local DB?"** `docker compose exec postgres psql -U router -d router`. Migrate step has already applied schema. (Router lives in `router` schema; pool's `AfterConnect` hook pins `search_path` so accidental writes to `public.*` are impossible.)
