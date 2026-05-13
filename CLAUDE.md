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
- **Concise comments** — comments should clarify non-obvious intent, not
  rehash what the code does
- **Non-tautological tests** — every test must assert behavior that would
  break if the production code were deleted

## Layer model and import rules

The router uses three concentric layers. Imports must flow inward only.

```
+-------------------------------------------------------------------+
|  cmd/router/main.go             (composition root — wires all)    |
|                                                                   |
|  +-------------------------------------------------------------+  |
|  |  internal/api/admin       (presentation: /health, /validate)|  |
|  |  internal/api/anthropic   (/v1/messages, passthrough)       |  |
|  |  internal/api/openai      (/v1/chat/completions)            |  |
|  |  internal/api/gemini      (/v1beta/models/:modelAction)     |  |
|  |  internal/server          (route registration)              |  |
|  |  internal/server/middleware (auth, timeout, eval overrides)  |  |
|  |  internal/postgres        (adapter: SQLC over pgx)          |  |
|  |  internal/sqlc            (generated; regenerate via `make generate`)|  |
|  |  internal/router/cluster    (Router impl: AvengersPro)      |  |
|  |  internal/providers/*     (Client impls)                    |  |
|  |                                                             |  |
|  |  +-------------------------------------------------------+  |  |
|  |  |  internal/auth      (identity domain: types,          |  |  |
|  |  |                      repos, Service.VerifyAPIKey,     |  |  |
|  |  |                      APIKeyCache, id/hashing)         |  |  |
|  |  |  internal/proxy     (routing/dispatch service:        |  |  |
|  |  |                      Route, ProxyMessages,            |  |  |
|  |  |                      ProxyOpenAIChatCompletion)       |  |  |
|  |  |  internal/router    (routing types + Router iface     |  |  |
|  |  |                      + ModelSpec/ModelRegistry)       |  |  |
|  |  |  internal/providers (Client iface + types)            |  |  |
|  |  |  internal/translate (OpenAI <-> Anthropic wire-       |  |  |
|  |  |                      format conversion; pure,         |  |  |
|  |  |                      no I/O)                          |  |  |
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
  `proxy.Service.Route` returns a `router.Decision`;
  `proxy.Service.ProxyOpenAIChatCompletion` calls into `translate` when the
  inbound and outbound wire formats differ).
- **Small utility third-party libraries are allowed at every layer.** The
  layering rule is about *where I/O and behavior live*, not about banning
  go.mod entries. Reach for a vetted small library (`golang-lru`, `uuid`,
  error helpers, etc.) before rolling your own. Reject heavyweight
  frameworks (DI containers, ORMs, metaprogramming kits) that bring
  code-organization opinions of their own.
- **`internal/router` and `internal/providers` are I/O-free.** They define
  interfaces and value types — nothing more. Adding a method that does I/O
  (HTTP, DB, queue, file system, etc.) to either of these packages is a
  layering violation; put it on `auth.Service` or in an adapter subpackage
  instead. Pure-Go utility libraries on those packages' go.mod are fine.
- **Adapters depend only on the inner ring.** `internal/postgres` may
  additionally import `internal/sqlc`. Adapters never import each other —
  `internal/postgres` does not know about `internal/api/admin`, etc.
- **`internal/api/*` and `internal/server`** depend on `internal/auth`
  (for the Service handle and middleware-context types) and on
  `internal/proxy` (for the routing/dispatch service handle). They may
  import `internal/observability` for logging, `internal/providers` for
  shared sentinel errors, and `internal/router/cluster` for the
  `ErrClusterUnavailable` sentinel (the API handlers map it to HTTP
  503). They must not import `internal/postgres`,
  any concrete `internal/providers/*` adapter, or `internal/translate`
  directly. Concrete instances reach the
  presentation layer only via constructor parameters from the composition
  root.
  (`internal/router/heuristic` and `internal/router/evalswitch` previously
  lived here; both were removed when the heuristic fallback was retired
  in favor of `cluster.ErrClusterUnavailable` → HTTP 503.)
- **`internal/config` and `internal/observability` are leaf utilities** —
  they must not import any other package under `internal/`. Third-party
  utility deps are fine; today they pull only stdlib + gin (for the
  request-scoped logger middleware).
- **Composition happens in `cmd/router/main.go`.** This is the only file
  that constructs concrete adapters and injects them. No other place wires
  things together. Keep `main.go` focused on wiring; the only helpers it
  carries today are `buildClusterScorer` (per-version Scorer assembly +
  embedder warmup) and `buildHeuristicFallback` (deterministic
  short/long-prompt router for the registered provider).

If you find yourself wanting to import something that violates these rules,
the design is wrong — surface it as an interface in the appropriate
inner-ring package and implement it in an adapter subpackage instead.

## Adding code — step-by-step recipes

### Adding an HTTP endpoint

1. **Decide the timeout budget.** Cheap auth-only ops use `validateTimeout` /
   `healthTimeout` (1 s). Anything that calls a provider gets its own
   constant in [`server.go`](internal/server/server.go) — pick a budget and
   justify it in a comment.
2. **Decide whether it needs auth.** If yes, register it under the `authed`
   group in `Register` (so `WithAuth` runs against `*auth.Service`). If no,
   register it on the engine directly.
3. **Decide whether it's part of the self-hoster dashboard surface.** The
   `/ui/*` static dashboard, `/admin/v1/auth/*`, and the `/admin/v1` mgmt
   group (metrics, keys, provider-keys, config) are mounted only when
   `mode == server.DeploymentModeSelfHosted`. New endpoints whose only
   consumer is the self-hosted dashboard go inside that block; endpoints
   that are part of the product surface (`/v1/*`, `/health`, `/validate`)
   stay outside it so they're available in `managed` mode too. **Do not**
   add a new `/admin/v1/*` route outside the selfhosted block — that
   would re-expose the redundant control plane on Weave-managed
   deployments.
4. **Pick (or create) the right `internal/api/<group>/` subpackage.**
   Operational endpoints live in `internal/api/admin/`; the Anthropic
   Messages surface lives in `internal/api/anthropic/`; the OpenAI Chat
   Completions surface lives in `internal/api/openai/`; the Gemini
   native surface lives in `internal/api/gemini/`. New surfaces get
   their own subpackage.
5. **Use `observability.FromGin(c)` for the request-scoped logger.** If you
   need the authed installation, call `middleware.InstallationFrom(c)`
   (returns nil if `WithAuth` wasn't applied — your handler should be on
   the authed group).
6. **Pick the right service.** Identity-only operations go on
   `*auth.Service`. Operations that route/dispatch/translate go on
   `*proxy.Service`. Don't touch repositories, the router, or providers
   from the handler. The handler adapts HTTP ↔ service; the service does
   the work.
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
- **Routing / dispatch / cross-format proxying** → `*proxy.Service` in
  [`internal/proxy/service.go`](internal/proxy/service.go).

Then:

1. **Define the method on the chosen `*Service`.** No I/O directly here —
   push that into a repository or provider adapter. Inner-ring imports
   (`router`, `providers`, `translate`, `observability`) and small utility
   libraries are fine.
2. **If it needs new repository methods, add them to the interfaces in
   [`installation.go`](internal/auth/installation.go) /
   [`api_key.go`](internal/auth/api_key.go).** The interface is the
   contract; the adapter must satisfy it.
3. **Implement the new repo method in
   [`internal/postgres/repository.go`](internal/postgres/repository.go),
   adding the corresponding SQLC query in `db/queries/`.** Run `make generate`
   to regenerate `internal/sqlc/`.
4. **Update the matching `service_test.go` fakes** to satisfy any expanded
   interface. Tests for the new Service method use the fakes; assert on
   real return values, not just that mocks were called.

### Adding a wire-format pair (translation)

When a new inbound format needs to talk to an existing upstream provider
whose wire format differs:

1. **Add the conversion functions to
   [`internal/translate/`](internal/translate).** Pure functions only — no
   I/O, no provider knowledge, no domain types.
2. **If the response is streaming, adapt
   [`SSETranslator`](internal/translate/stream.go)** or add a sibling
   decorator. Decorators wrap `http.ResponseWriter` and translate on the
   fly so we never buffer entire responses.
3. **Compose the new translation in `proxy.Service.Proxy*`.** The proxy
   service is the only caller of `translate`. Keep the providers
   (`internal/providers/<name>/`) ignorant of cross-format concerns.

### Adding a new `providers.Client` adapter

1. **Create `internal/providers/<name>/client.go`** with a `Client` struct
   and `NewClient(...)` constructor that takes whatever credentials it
   needs (typically an API key string).
2. **Implement `Proxy` and `Passthrough`.** The adapter is responsible for
   translating the prepared request body to the provider's wire format,
   sending it with a pooled `http.Client` (use `httputil.NewTransport` and
   `httputil.StreamBody` from `internal/providers/httputil/`), and streaming
   the response back. Do not let provider-specific types leak across the
   package boundary.
3. **Add a compile-time check:**
   `var _ providers.Client = (*Client)(nil)`
4. **Add the new client to the `map[string]providers.Client` registry in
   `cmd/router/main.go`** keyed by the same name the routing strategy
   emits in `decision.Provider`. `"anthropic"`, `"openai"`, and `"google"`
   are the existing wired keys (each registered only when its API key env
   var is set).
5. **Wire it in `cmd/router/main.go`.** This is the only place that
   imports the provider package directly.

### Adding a new `router.Router` implementation

1. **Create a sibling subpackage to `internal/router/cluster/`.** Today
   `cluster/` (AvengersPro) is the only `Router` impl; new ones might be
   e.g. `internal/router/shadow/` for one that wraps two others (see
   `docs/plans/archive/CLUSTER_ROUTING_PLAN.md` "Shadow / promotion gates").
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
   `auth.APIKey`) must not leak `pgtype` / `uuid` concerns — convert at the
   adapter boundary.

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

- **Auth-related helpers** (token prefix, ID generation, hashing) live in
  [`internal/auth`](internal/auth) alongside the types they support.
- **Env parsing** lives in [`internal/config`](internal/config).
- **Logging / tracing** lives in
  [`internal/observability`](internal/observability).

If the new helper doesn't fit any of these, justify the new package in a
code comment before creating it.

## Conventions

### Go style

- **No magic strings for provider names or model names.** Use the named
  constants from `internal/providers` (`providers.ProviderAnthropic`,
  `providers.ProviderOpenAI`, `providers.ProviderGoogle`,
  `providers.ProviderOpenRouter`) everywhere provider names appear as
  values — map keys, switch cases, `router.Decision.Provider` literals,
  log fields, test fixtures. For new model name constants, add them to the
  appropriate package before using them. Bare string literals for these
  values are a review-blocking issue.
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

- Tests live next to the code (`foo_test.go` next to `foo.go`), in
  `<pkg>_test` external test packages so they exercise only the exported
  API.
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
  standalone project with no cross-project code dependencies. If you need
  a utility from elsewhere in the surrounding monorepo, copy it into the
  appropriate `internal/` package with its own godoc instead of importing
  it.
- **Never write raw SQL outside `db/queries/`** or call `pgx.Pool` directly
  from anywhere except `internal/postgres/`. SQLC is the only data mapper.
- **Never reach across layers.** A handler in `internal/api/` calling
  `*sqlc.Queries` directly is a layering violation; surface a Service
  method instead. A repo calling another repo is a layering violation;
  put the orchestration in `auth.Service`.
- **Never add foreign keys to tables outside the router's own schema.**
  Such tables don't exist in this project. `organization_id` and
  `created_by` are opaque external strings, not FKs.
- **Never panic on the request path.** Reserve `panic` for startup-time
  fail-fast checks (`config.MustGet`) where misconfiguration must abort
  the process, not turn into runtime errors.
- **Never introduce a DI container, reflection-based wiring, or a "service
  locator".** Composition is plain Go function calls in
  `cmd/router/main.go`.
- **Never log secrets, raw API keys, or full request bodies.** The first 8
  + last 4 chars stored on `auth.APIKey` are the safe form; the full
  token is not.
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
  e.g. `v0.2`) names the version the runtime serves by default; the
  `ROUTER_CLUSTER_VERSION` env var overrides it. Promotion is a
  one-line edit to `latest` plus a redeploy.
- The Go runtime builds **one Scorer per committed bundle** at boot
  (`cluster.NewMultiversion` in `cmd/router/main.go`); customer traffic
  hits the default version; any caller can pin per-request to a sibling
  version with `x-weave-cluster-version: v0.X` via
  `middleware.WithClusterVersionOverride`. This is the
  "compare-against-each-other" mechanism — a single staging deployment
  carries every committed bundle and the eval harness flips between
  them per-request.
- **Centroids/rankings are write-once.** `train_cluster_router.py`
  always writes to `artifacts/v<X.Y>/` and never overwrites a previous
  version (auto-bumps from `latest` when `--version` is omitted). Pass
  `--from v0.2` to clone a previous version's `model_registry.json`
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
  are frozen for comparison purposes — once `v0.2` is committed, train
  to `v0.3` rather than re-running `train_cluster_router.py` against
  `v0.2`. The training script auto-bumps to prevent this; only override
  with `--version v0.X` for in-place fixes you intend to land as a
  separate commit.
- **Don't bypass the version pointer.** `artifacts/latest` is the
  single source of truth for the default served version. Don't
  hardcode a version in `cmd/router/main.go`; let
  `cluster.ResolveVersion` read the pointer.


## Multi-provider routing (Anthropic / OpenAI / Google)

The router serves all three vendors from one composition root. A
single routing decision picks `(Provider, Model)` and the proxy
dispatches accordingly. Cross-format translation lives in
`internal/translate` so handlers never juggle wire formats.

**Architecture:**

- `cmd/router/main.go` registers each provider client only when its
  API key is set: `ANTHROPIC_API_KEY` (mandatory), `OPENAI_API_KEY`
  and `GOOGLE_API_KEY` (optional). The set of registered
  provider names is passed to the cluster scorer as `availableProviders`.
- `cluster.NewScorer` filters `model_registry.json`'s `deployed_models`
  list to entries whose `provider` is in `availableProviders`. argmax
  runs over the filtered list, so a deployment without a Google key
  cannot accidentally emit a `gemini-*` decision.
- `model_registry.json` is a flat list of `{model, provider,
  bench_column, proxy?, proxy_note?}` entries. Direct columns are 1:1
  with OpenRouterBench; proxy entries (`proxy: true`) reuse another
  column's score until D3 traffic provides direct ranking data. The
  training script copies bench-column scores onto every deployed
  entry referencing that column rather than averaging columns
  together — that's how the scorer can rank `gpt-5` and the proxy
  `claude-opus-4-7` distinctly.
- `internal/translate` has both directions:
  - `OpenAIToAnthropicRequest` + `SSETranslator` — used when an
    OpenAI Chat Completions client lands on an Anthropic decision.
  - `AnthropicToOpenAIRequest` + `AnthropicSSETranslator` — used
    when an Anthropic Messages client (e.g. Claude Code) lands on
    an OpenAI or Google decision. Both translators are pure data
    transforms; `proxy.Service` is the only caller.
- `internal/providers/google` is the Gemini adapter. It hits Google's
  OpenAI-compatible endpoint (`https://generativelanguage.googleapis.com/v1beta/openai`)
  so the existing OpenAI translator + the `openai` adapter's stripping
  logic apply unchanged. **Do not** add a native Gemini REST client
  unless a feature (grounding, native multimodal) requires the
  non-compat surface.

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
**What to NOT do:**

- **Don't bypass the provider filter.** If you need to route to a
  provider whose key isn't registered, register the provider — don't
  add a special-case path that ignores `availableProviders`.
- **Don't add bench-column averaging back to the training script.**
  The 1:1 mapping is the point. Two entries that share a column copy
  the same score; they don't average across columns.
- **Don't leak Anthropic-only fields (`thinking`, `cache_control`,
  `metadata`) when targeting OpenAI/Google.** `AnthropicToOpenAIRequest`
  strips them; the OpenAI/Google adapters strip
  `reasoning_effort`/`thinking` defensively. Keep both checks — the
  belt-and-suspenders is intentional because the field set drifts as
  Anthropic adds beta features.
- **Don't add per-installation provider preference yet.** Deploy-time
  env config + the cluster scorer's per-prompt argmax cover the v1
  use cases; per-installation routing is a follow-up PR with a DB
  migration on `model_router_installations`.

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
  - Routing / dispatch / cross-format proxying → method on `*proxy.Service`.
  - Wire-format conversion (no I/O) → function in `internal/translate/`.
  Add a new repo interface method (if it touches the DB) and an adapter
  implementation. The handler just adapts HTTP.
- **"Should this be in `auth`/`proxy`/`translate`/`config`/`observability`
  or in the package that uses it?"** In the package that uses it, unless
  three or more packages need the same logic. The shared homes are not
  catch-alls.
- **"How do I add streaming?"** Don't, until v1 + provider adapters land.
  When you do, the gin streaming pattern (`c.Stream`) goes in the
  handler, and `providers.Client` grows a `Stream(ctx, req) (chan Event,
  error)` method or similar — design it then, not now.
- **"Should I commit `internal/sqlc/`?"** Yes. The Dockerfile and CI
  builds depend on the generated code being present. Run `make generate`
  before committing migration or query changes.
- **"How do I run a one-off query against the local DB?"**
  `docker compose exec postgres psql -U router -d router`. The migrate
  step has already applied the schema.
