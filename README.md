# router

A standalone Go service for authenticating and routing LLM completions
to the most appropriate provider. The service proxies Anthropic Messages
and OpenAI Chat Completions requests, selecting a model via the cluster
scorer (AvengersPro-derived) or a deterministic heuristic fallback.

## Getting started

The router can run two ways: fully containerized via **Docker Compose**,
or locally with **Go + Make**. Both produce a working router at
`http://localhost:8082`.

> **Provider API key required.** The router proxies requests to upstream
> LLM providers. At least one of `ANTHROPIC_API_KEY`,
> `OPENAI_PROVIDER_API_KEY`, or `GOOGLE_PROVIDER_API_KEY` must be set,
> or the server refuses to boot. See [Configuration](#configuration) for
> the full list.

### Option A: Docker Compose (recommended for first run)

No local Go toolchain needed — everything runs in containers.

```bash
cd router

# 1. Export your provider API key (pick at least one).
export ANTHROPIC_API_KEY=sk-ant-...

# 2. Start Postgres, run migrations, and boot the server.
docker compose up --build -d

# 3. Point Claude Code at the local router (writes ~/.claude/settings.json
#    with ANTHROPIC_BASE_URL=http://localhost:8082 and the routed-model
#    status line). Idempotent — re-run any time.
make install-cc

# 4. Verify.
curl http://localhost:8082/health
claude   # status line should show "WEAVE ROUTER — <model>"
```

`make install-cc` wraps `./install/install.sh --local`, which uses
`ROUTER_DEV_MODE=true` so no API key seed is needed for local development.
For the seeded-key path (or other tools like Cursor) see [Connecting
clients](#connecting-clients) below.

The stack runs three services:

1. `postgres` — postgres:15-alpine, host port `5433`
2. `migrate` — one-shot golang-migrate that applies `db/migrations/` and
   exits 0
3. `server` — built from the `Dockerfile`, host port `8082`

Tear down:

```bash
make uninstall-cc         # remove the Claude Code → router config
docker compose down       # keep the Postgres volume
docker compose down -v    # drop the volume (fresh next time)
```

### Option B: Local development (Go + Make)

Prerequisites: Go 1.25+,
[golang-migrate](https://github.com/golang-migrate/migrate),
[CompileDaemon](https://github.com/githubnemo/CompileDaemon) (for hot
reload).

#### 1. Start Postgres

Use the bundled compose file to start Postgres only:

```bash
cd router
make db
```

Then create `.env.local` with the matching connection string and your
provider key:

```
DATABASE_URL=postgresql://router:router@localhost:5433/router?sslmode=disable
ANTHROPIC_API_KEY=sk-ant-...
```

Or point `DATABASE_URL` at any Postgres you already have running (the
default in `.env.development` targets `localhost:5432/router_dev`).

#### 2. Bootstrap the database

```bash
make setup
```

This runs three steps in order:

| Step | Target | What it does |
| ---- | ------ | ------------ |
| 1 | `make initdb` | Creates the database and `router` schema (idempotent) |
| 2 | `make migrate-up` | Applies all pending migrations via golang-migrate |
| 3 | `make seed` | Creates an installation + API key and prints usage instructions |

Save the printed token — it is shown only once.

#### 3. Start the server

```bash
make dev
```

Verify:

```bash
curl http://localhost:8082/health
curl -H "Authorization: Bearer <token>" http://localhost:8082/validate
```

### Connecting clients

#### Claude Code (recommended: `make install-cc`)

For the bundled docker-compose router, the one-shot installer is the
shortest path:

```bash
make install-cc          # writes ~/.claude/settings.json + status line
claude                   # routes through the local router
make uninstall-cc        # revert
```

Under the hood it runs `./install/install.sh --local`, which sets
`ANTHROPIC_BASE_URL=http://localhost:8082` and installs a status line
script that surfaces the routed model and per-turn savings. Re-running is
idempotent and preserves any other keys in your `settings.json`. For
project-scope installs, hosted/self-hosted URLs, or seeded-key flows, see
[`install/README.md`](install/README.md).

If you'd rather wire it up by hand (e.g. against a `make setup`-seeded
key):

```bash
export ANTHROPIC_BASE_URL=http://localhost:8082
export ANTHROPIC_API_KEY=<token>
claude
```

#### Cursor

1. Open Cursor Settings → Models → Override OpenAI Base URL
   Set to: `http://localhost:8082/v1`
2. Add an API key: `<token>` (from `make seed`, or any string when
   `ROUTER_DEV_MODE=true`)

### Other Makefile targets

Run `make help` to see all available targets:

```bash
make generate    # regenerate SQLC (no live DB required)
make build       # typecheck the whole module
make test        # run all tests
make check       # full CI-equivalent check (generate + build + test)
make install-cc  # point Claude Code at the local router
```

## What's running today


| Endpoint                    | Method | Auth          | Purpose                                                                                                          |
| --------------------------- | ------ | ------------- | ---------------------------------------------------------------------------------------------------------------- |
| `/health`                   | GET    | none          | Cheap liveness probe (~1 ms). Used by Cloud Run / Compose healthchecks.                                          |
| `/validate`                 | GET    | bearer        | Bearer-key validity check. Returns the matched installation's id and name on success; opaque 401 on any failure. |
| `/v1/messages`              | POST   | bearer or dev | Anthropic Messages proxy. Routes to a model via the cluster scorer, dispatches to the upstream provider.         |
| `/v1/chat/completions`      | POST   | bearer or dev | OpenAI Chat Completions proxy. Same routing logic as `/v1/messages`.                                             |
| `/v1/messages/count_tokens` | POST   | bearer or dev | Anthropic passthrough — forwarded as-is with service credentials.                                                |
| `/v1/models`                | GET    | bearer or dev | Anthropic passthrough — model availability list.                                                                 |
| `/v1/models/:model`         | GET    | bearer or dev | Anthropic passthrough — single-model lookup.                                                                     |


"bearer or dev" means auth is required unless `ROUTER_DEV_MODE=true`,
which bypasses router-side bearer auth on `/v1/*` so a developer can
point a client (e.g. Claude Code) at the router without seeding a key.
The admin surface (`/validate`) stays protected regardless.

## Architecture

### Router service

```mermaid
flowchart TB
    Client["HTTP client"] -->|"Authorization: Bearer rk_..."| Engine["Gin engine"]

    subgraph server_pkg ["internal/server (route registration + middleware)"]
        Register["Register"]
        TimeoutMW["middleware.WithTimeout"]
        AuthMW["middleware.WithAuth"]
        EvalOverrideMW["middleware.WithEvalRoutingOverride"]
        EmbedOverrideMW["middleware.WithEmbedLastUserMessageOverride"]
        ClusterVersionMW["middleware.WithClusterVersionOverride"]
    end

    subgraph admin_pkg ["internal/api/admin"]
        Health["HealthHandler"]
        Validate["ValidateHandler"]
    end

    subgraph anthapi_pkg ["internal/api/anthropic"]
        MessagesH["MessagesHandler"]
        PassthroughH["PassthroughHandler"]
    end

    subgraph oaiapi_pkg ["internal/api/openai"]
        ChatCompH["ChatCompletionHandler"]
    end

    Engine --> Register
    Register --> TimeoutMW
    TimeoutMW --> Health
    TimeoutMW --> AuthMW
    AuthMW --> Validate
    AuthMW --> EvalOverrideMW
    EvalOverrideMW --> EmbedOverrideMW
    EmbedOverrideMW --> ClusterVersionMW
    ClusterVersionMW --> MessagesH
    ClusterVersionMW --> ChatCompH
    AuthMW --> PassthroughH

    subgraph auth_pkg ["internal/auth (identity domain)"]
        AuthService["Service
        VerifyAPIKey"]
        KeyCache["APIKeyCache
        (LRU, in-process)"]
        InstallRepoIface[("InstallationRepository")]
        APIKeyRepoIface[("APIKeyRepository")]
    end

    AuthMW --> AuthService
    AuthService --> KeyCache
    AuthService --> InstallRepoIface
    AuthService --> APIKeyRepoIface

    subgraph proxy_pkg ["internal/proxy (routing/dispatch)"]
        ProxyService["Service
        Route / Dispatch / ProxyMessages /
        ProxyOpenAIChatCompletion / Passthrough*"]
    end

    MessagesH --> ProxyService
    ChatCompH --> ProxyService
    PassthroughH --> ProxyService

    subgraph router_pkg ["internal/router"]
        RouterIface[("Router interface
        + Request/Decision types
        + ModelSpec/ModelRegistry")]
    end
    ProxyService --> RouterIface

    subgraph providers_pkg ["internal/providers"]
        ClientIface[("Client interface
        + Request/Response types")]
    end
    ProxyService --> ClientIface

    subgraph translate_pkg ["internal/translate (wire-format conversion)"]
        TranslateFns["OpenAIToAnthropicRequest /
        AnthropicToOpenAIResponse /
        SSETranslator (decorator)"]
    end
    ProxyService --> TranslateFns

    subgraph postgres_pkg ["internal/postgres (adapter)"]
        InstallRepo["installationRepo"]
        APIKeyRepo["apiKeyRepo"]
    end
    InstallRepoIface -.implemented by.-> InstallRepo
    APIKeyRepoIface -.implemented by.-> APIKeyRepo
    InstallRepo --> SQLC["internal/sqlc (generated)"]
    APIKeyRepo --> SQLC
    SQLC -->|pgx/v5| Postgres[("Postgres
    model_router_installations
    model_router_api_keys")]

    subgraph evalswitch_pkg ["internal/router/evalswitch"]
        EvalSwitch["Router: per-request
        primary/fallback switch"]
    end

    subgraph cluster_pkg ["internal/router/cluster"]
        Multiversion["Multiversion: dispatches by
        x-weave-cluster-version header,
        defaults to artifacts/latest (v0.6)"]
        ScorerV01["Scorer (v0.1)"]
        ScorerV02["Scorer (v0.2)"]
        ScorerV03["Scorer (v0.3)"]
        ScorerV04["Scorer (v0.4)"]
        ScorerV05["Scorer (v0.5)"]
        ScorerV06["Scorer (v0.6 default)"]
        Multiversion --> ScorerV01
        Multiversion --> ScorerV02
        Multiversion --> ScorerV03
        Multiversion --> ScorerV04
        Multiversion --> ScorerV05
        Multiversion --> ScorerV06
        SharedEmbedder["Embedder
        (Jina v2 INT8 ONNX,
        shared across versions)"]
        ScorerV01 -.uses.-> SharedEmbedder
        ScorerV02 -.uses.-> SharedEmbedder
        ScorerV03 -.uses.-> SharedEmbedder
        ScorerV04 -.uses.-> SharedEmbedder
        ScorerV05 -.uses.-> SharedEmbedder
        ScorerV06 -.uses.-> SharedEmbedder
    end

    subgraph heuristic_pkg ["internal/router/heuristic"]
        Rules["Rules: token-threshold
        fallback (single provider)"]
    end

    RouterIface -.implemented by.-> EvalSwitch
    EvalSwitch -->|primary| Multiversion
    EvalSwitch -->|fallback| Rules
    Multiversion -->|"fail-open"| Rules

    subgraph anthropic_provider ["internal/providers/anthropic"]
        AnthClient["Client: Messages API"]
    end

    subgraph openai_provider ["internal/providers/openai"]
        OAIClient["Client: Chat Completions API"]
    end

    subgraph google_provider ["internal/providers/google"]
        GoogleClient["Client: Gemini via
        OpenAI-compatible endpoint"]
    end

    ClientIface -.implemented by.-> AnthClient
    ClientIface -.implemented by.-> OAIClient
    ClientIface -.implemented by.-> GoogleClient
    AnthClient -->|HTTPS| AnthropicAPI[("Anthropic API")]
    OAIClient -->|HTTPS| OpenAIAPI[("OpenAI API")]
    GoogleClient -->|HTTPS| GoogleAPI[("Google Generative
    Language API")]

    Composition["cmd/router/main.go
    (composition root)"]
    Composition -.constructs + injects.-> AuthService
    Composition -.constructs + injects.-> ProxyService
    Composition -.constructs.-> InstallRepo
    Composition -.constructs.-> APIKeyRepo
    Composition -.constructs.-> EvalSwitch
    Composition -.constructs.-> Multiversion
    Composition -.constructs.-> Rules
    Composition -.constructs.-> AnthClient
    Composition -.constructs.-> OAIClient
    Composition -.constructs.-> GoogleClient
```



### Offline tooling (Python)

```mermaid
flowchart TB
    subgraph training ["scripts/ — Cluster training pipeline"]
        DownloadBench["download_bench.sh
        Pull OpenRouterBench"]
        SweepK["sweep_cluster_k.py
        Pick K clusters"]
        Train["train_cluster_router.py
        Build centroids + rankings"]
        DumpFixture["dump_cluster_test_vector.py
        Build Go parity fixture"]
        DownloadBench --> SweepK --> Train --> DumpFixture
    end

    subgraph model_download ["scripts/ — Model download"]
        DownloadHF["download_from_hf.py
        Jina v2 INT8 ONNX from HF"]
    end

    subgraph artifacts ["Committed artifacts (go:embed in cluster/)"]
        Centroids["centroids.bin"]
        Rankings["rankings.json"]
        ModelReg["model_registry.json"]
        Fixture["testdata/fixture.json"]
    end

    subgraph runtime_assets ["Runtime assets (not in git)"]
        ModelONNX["assets/model.onnx"]
    end

    Train --> Centroids
    Train --> Rankings
    Train --> ModelReg
    DumpFixture --> Fixture
    DownloadHF --> ModelONNX

    Centroids --> ClusterPkg["internal/router/cluster/"]
    Rankings --> ClusterPkg
    ModelReg --> ClusterPkg
    Fixture --> ClusterTests["cluster/testdata/"]
    ModelONNX --> ClusterPkg

    subgraph eval_harness ["eval/ — Phase 1a eval harness (Modal)"]
        ModalApp["modal_app.py
        Orchestrates eval runs"]
        BenchLoaders["benchmarks/
        14 dataset loaders"]
        JudgePkg["judges/
        ensemble / gemini / gpt5"]
        Inference["inference.py + routing.py
        Run completions via router"]
        ScoringPkg["rubric.py + reference.py
        + pareto.py"]
        ModalApp --> BenchLoaders
        ModalApp --> JudgePkg
        ModalApp --> Inference --> ScoringPkg
    end

    Inference -->|"HTTP completions"| StagingDeploy["Staging router
    (evalswitch A/B)"]
    ScoringPkg --> EvalResults["docs/eval/EVAL_RESULTS.md"]
```



Strict dependency direction (outer → inner):


| Layer             | Package                                                        | Imports                                             | Purpose                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| ----------------- | -------------------------------------------------------------- | --------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Inner — auth      | `[internal/auth](internal/auth)`                               | stdlib + small utility libs (e.g. `golang-lru/v2`)  | `Installation`, `APIKey`, repo interfaces, `Service.VerifyAPIKey`, the small `id`/`hashing` helpers used for token issuance, and the in-process `APIKeyCache`                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| Inner — proxy     | `[internal/proxy](internal/proxy)`                             | `router`, `providers`, `translate`, `observability` | `Service` for routing decisions and provider dispatch (`Route`, `Dispatch`, `ProxyMessages`, `ProxyOpenAIChatCompletion`, `Passthrough*`). Composes `translate` when inbound and outbound wire formats differ.                                                                                                                                                                                                                                                                                                                                                                                             |
| Inner — router    | `[internal/router](internal/router)`                           | stdlib + small utility libs; no I/O                 | `Router` interface, `Request`/`Decision` types, `ModelSpec`/`ModelRegistry` for capability-aware model definitions                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| Inner — providers | `[internal/providers](internal/providers)`                     | stdlib + small utility libs; no I/O                 | `Client` interface and `Request`/`Response` types, plus shared helpers (`CopyUpstreamHeaders`)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| Inner — translate | `[internal/translate](internal/translate)`                     | stdlib only; no I/O                                 | OpenAI ↔ Anthropic wire-format conversion: `OpenAIToAnthropicRequest`, `AnthropicToOpenAIResponse`, `SSETranslator` decorator for streaming                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| Adapter           | `[internal/postgres](internal/postgres)`                       | `auth`, `internal/sqlc`, `pgx/v5`, `uuid`           | Implements `auth.{Installation,APIKey}Repository` over SQLC-generated queries                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| Adapter           | `[internal/router/heuristic](internal/router/heuristic)`       | `router`                                            | Implements `router.Router` with a deterministic token-threshold rule. Always wired as the fail-open fallback.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| Adapter           | `[internal/router/cluster](internal/router/cluster)`           | `router`, `observability`, `hugot`/`onnxruntime_go` | Implements `router.Router` with an AvengersPro-derived cluster scorer: in-process Jina v2 INT8 ONNX embedder + K-means + α-blended ranking matrix. Versioned bundles live at `artifacts/v<X.Y>/` (`//go:embed all:artifacts`); `Multiversion` builds one `Scorer` per committed bundle and dispatches per-request via the trusted `x-weave-cluster-version` header (defaults to the version named in `artifacts/latest`). Falls back to `heuristic.Rules` on any embed/score error. Wired by default; bypass with `ROUTER_DISABLE_CLUSTER=true`. See [`docs/plans/archive/CLUSTER_ROUTING_PLAN.md`](docs/plans/archive/CLUSTER_ROUTING_PLAN.md). |
| Adapter           | `[internal/router/evalswitch](internal/router/evalswitch)`     | `router`                                            | Wraps a primary + fallback `router.Router`, dispatching per-request based on a context key set by `middleware.WithEvalRoutingOverride`. Enables A/B comparison of routing strategies on a single staging deployment.                                                                                                                                                                                                                                                                                                                                                                                       |
| Adapter           | `[internal/providers/anthropic](internal/providers/anthropic)` | `providers`, `router`                               | Implements `providers.Client` over the Anthropic Messages API                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| Adapter           | `[internal/providers/openai](internal/providers/openai)`       | `providers`, `router`                               | Implements `providers.Client` over the OpenAI Chat Completions API                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| Adapter           | `[internal/providers/google](internal/providers/google)`       | `providers`, `router`                               | Implements `providers.Client` over Google's Gemini API via its OpenAI-compatible endpoint, so the OpenAI translator + adapter stripping logic apply unchanged                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| Adapter           | `[internal/providers/noop](internal/providers/noop)`           | `providers`                                         | Implements `providers.Client`; used as a test double in `proxy.Service`'s unit tests. Not wired in `cmd/router/main.go`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| Presentation      | `[internal/api/admin](internal/api/admin)`                     | `gin`                                               | Operational handlers (`HealthHandler`, `ValidateHandler`)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| Presentation      | `[internal/api/anthropic](internal/api/anthropic)`             | `proxy`, `providers`, `gin`                         | Anthropic Messages handlers (`MessagesHandler`, `PassthroughHandler`)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| Presentation      | `[internal/api/openai](internal/api/openai)`                   | `proxy`, `providers`, `gin`                         | OpenAI Chat Completions handler                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| Presentation      | `[internal/server](internal/server)`                           | `auth`, `proxy`, `api/*`, `server/middleware`       | Wires the gin engine and route registration                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| Presentation      | `[internal/server/middleware](internal/server/middleware)`     | `auth`, `observability`, `evalswitch`, `gin`        | Auth, timeout, eval-routing-override, embed-last-user-message-override, and cluster-version-override middleware (Chain of Responsibility)                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| Composition       | `[cmd/router](cmd/router)`                                     | everything                                          | Builds concrete instances and starts the listener                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| Helper            | `[internal/config](internal/config)`                           | stdlib                                              | `MustGet` / `GetOr` over `os.Getenv`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| Helper            | `[internal/observability](internal/observability)`             | stdlib + gin                                        | `slog`-based logger plus per-request gin middleware                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| Helper            | `[internal/observability/otel](internal/observability/otel)`   | stdlib + OTLP protobuf                              | Async OTLP/HTTP span export: per-request `Buffer`, worker-pool `Emitter`, `UsageExtractor` (response-sniffing decorator), model `Pricing` table. Leaf utility — no imports from `internal/` except `observability` parent.                                                                                                                                                                                                                                                                                                                                                                                 |


The CLEAN dependency rule: inner-ring packages (`auth`, `proxy`, `router`,
`providers`, `translate`) don't depend on adapters or presentation;
adapters depend only on the inner ring; the composition root depends on
everything. See [AGENTS.md](AGENTS.md) for the full set of import rules.

## Configuration

All configuration is via environment variables. The router follows the
[12-factor app](https://12factor.net/config) pattern — no config files,
no CLI flags.

### Environment variables

#### Required


| Variable       | Purpose                                                                    |
| -------------- | -------------------------------------------------------------------------- |
| `DATABASE_URL` | Postgres connection string. Alternatively set the `POSTGRES_*` vars below. |


#### Providers


| Variable                   | Default                                                   | Purpose                                                              |
| -------------------------- | --------------------------------------------------------- | -------------------------------------------------------------------- |
| `ANTHROPIC_API_KEY`        | *(none)*                                                  | Enables the Anthropic provider (Messages API).                       |
| `OPENAI_PROVIDER_API_KEY`  | *(none)*                                                  | Enables the OpenAI provider (Chat Completions API).                  |
| `OPENAI_PROVIDER_BASE_URL` | `https://api.openai.com`                                  | Override the OpenAI base URL (useful for Azure OpenAI).              |
| `GOOGLE_PROVIDER_API_KEY`  | *(none)*                                                  | Enables the Google Gemini provider (via OpenAI-compatible endpoint). |
| `GOOGLE_PROVIDER_BASE_URL` | `https://generativelanguage.googleapis.com/v1beta/openai` | Override the Gemini base URL.                                        |


At least one provider key must be set or the router refuses to boot.

#### Postgres

Set `DATABASE_URL` directly, or compose it from the individual vars:


| Variable                   | Default                                                             | Purpose                                                           |
| -------------------------- | ------------------------------------------------------------------- | ----------------------------------------------------------------- |
| `DATABASE_URL`             | *(none)*                                                            | Full Postgres connection string (takes precedence).               |
| `POSTGRES_USER`            | *(required if no `DATABASE_URL`)*                                   | Postgres username.                                                |
| `POSTGRES_PASSWORD`        | *(required if no `DATABASE_URL`)*                                   | Postgres password.                                                |
| `POSTGRES_DB`              | *(required if no `DATABASE_URL`)*                                   | Database name.                                                    |
| `POSTGRES_HOST`            | *(required if no `DATABASE_URL` and no `POSTGRES_CONNECTION_NAME`)* | Hostname.                                                         |
| `POSTGRES_PORT`            | `5432`                                                              | Port number.                                                      |
| `POSTGRES_SSLMODE`         | `require`                                                           | TLS mode. Set to `disable` for local Docker.                      |
| `POSTGRES_CONNECTION_NAME` | *(none)*                                                            | Cloud SQL Auth Proxy instance connection name (Unix socket path). |


#### Routing


| Variable                          | Default                      | Purpose                                                                     |
| --------------------------------- | ---------------------------- | --------------------------------------------------------------------------- |
| `ROUTER_DISABLE_CLUSTER`          | `false`                      | Set `true` to skip the cluster scorer and use the heuristic only.           |
| `ROUTER_CLUSTER_VERSION`          | *(reads `artifacts/latest`)* | Pin a specific cluster artifact version (e.g. `v0.3`).                      |
| `ROUTER_CLUSTER_EMBED_TIMEOUT_MS` | `200`                        | Per-request ONNX embed timeout in ms. Increase for slower hosts.            |
| `ROUTER_EMBED_LAST_USER_MESSAGE`  | `false`                      | Feed the last user message to the embedder instead of concatenated context. |
| `ROUTER_STICKY_DECISION_TTL_MS`   | `0` (disabled)               | Reuse routing decisions per API key for this many ms.                       |
| `ROUTER_ONNX_ASSETS_DIR`          | `/opt/router/assets`         | Directory containing `model.onnx`.                                          |
| `ROUTER_ONNX_LIBRARY_DIR`         | *(system default)*           | Path to `libonnxruntime` (e.g. `/opt/homebrew/lib` on Apple Silicon).       |


#### Server


| Variable                       | Default                              | Purpose                                                                                                                                     |
| ------------------------------ | ------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `PORT`                         | `8080`                               | HTTP listen port.                                                                                                                           |
| `ROUTER_DEV_MODE`              | `false`                              | Bypass bearer auth on `/v1/`* for local development.                                                                                        |
| `ROUTER_DECISIONS_LOG_PATH`    | `~/.weave-router/decisions.jsonl`    | Path for the JSON-lines decision sidecar log (one line per routed request). Set to `off` to disable.                                        |


#### Telemetry (OpenTelemetry)

The router can export per-request trace spans to any OTLP-compatible
collector. Each proxied request produces two spans with routing decisions,
token usage, cost estimates, and latency data. Export is fully async and
non-blocking, buffered in a per-request `Buffer`, and flushed at
two deterministic points (before the upstream call and after the response
completes).

**To enable:** set `OTEL_EXPORTER_OTLP_ENDPOINT` to your collector URL.
When unset, OTel is completely disabled with zero runtime overhead (nil
emitter, nil buffer — all methods no-op).


| Variable                         | Default      | Purpose                                                                      |
| -------------------------------- | ------------ | ---------------------------------------------------------------------------- |
| `OTEL_EXPORTER_OTLP_ENDPOINT`    | *(disabled)* | Collector base URL (e.g. `https://api.honeycomb.io`). Required to enable.    |
| `OTEL_EXPORTER_OTLP_HEADERS`     | *(none)*     | Comma-separated `key=value` headers on every export POST (e.g. auth tokens). |
| `OTEL_EXPORTER_OTLP_TIMEOUT`     | `10000`      | Per-export HTTP timeout in milliseconds.                                     |
| `OTEL_SERVICE_NAME`              | `router`     | Value of the `service.name` resource attribute.                              |
| `OTEL_RESOURCE_ATTRIBUTES`       | *(none)*     | Comma-separated `key=value` pairs added to the OTLP resource.                |
| `OTEL_BSP_MAX_QUEUE_SIZE`        | `1000`       | Span queue capacity. Spans are dropped when the queue is full.               |
| `OTEL_BSP_MAX_EXPORT_BATCH_SIZE` | `50`         | Maximum spans per OTLP POST.                                                 |
| `OTEL_BSP_SCHEDULE_DELAY`        | `500`        | Partial-batch flush interval in milliseconds.                                |
| `OTEL_EXPORT_WORKERS`            | `2`          | Number of export goroutines draining the queue.                              |


The first five follow the [OTel SDK environment variable spec](https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables/).
`OTEL_BSP_`* follows the [Batch Span Processor spec](https://opentelemetry.io/docs/specs/otel/trace/sdk/#batch-span-processor).
`OTEL_EXPORT_WORKERS` is a custom extension (no standard equivalent for
worker-pool sizing).

**Span attributes exported per request (two spans per proxied call):**

`router.decision` — emitted after the routing decision, before upstream dispatch:

| Attribute                         | Type   | Description                                                     |
| --------------------------------- | ------ | --------------------------------------------------------------- |
| `requested.model`                 | string | Model the client asked for.                                     |
| `decision.model`                  | string | Model the router selected.                                      |
| `decision.provider`               | string | Provider the router selected (`anthropic`, `openai`, `google`). |
| `decision.reason`                 | string | Why the router chose this model (e.g. `cluster`, `heuristic`).  |
| `routing.estimated_input_tokens`  | int    | Estimated input tokens from the request envelope.               |
| `pricing.requested_input_per_1m`  | float  | Per-1M input price for the requested model.                     |
| `pricing.actual_input_per_1m`     | float  | Per-1M input price for the routed model.                        |
| `latency.route_ms`                | int    | Time spent computing the routing decision.                      |

`router.upstream` — emitted after the upstream response completes:

| Attribute                  | Type   | Description                                                     |
| -------------------------- | ------ | --------------------------------------------------------------- |
| `usage.input_tokens`       | int    | Actual input tokens reported by the upstream provider.           |
| `usage.output_tokens`      | int    | Actual output tokens reported by the upstream provider.          |
| `cost.requested_input_usd` | float  | Estimated input cost if the requested model had been used.       |
| `cost.requested_output_usd`| float  | Estimated output cost if the requested model had been used.      |
| `cost.actual_input_usd`    | float  | Actual input cost for the routed model.                          |
| `cost.actual_output_usd`   | float  | Actual output cost for the routed model.                         |
| `latency.upstream_ms`      | int    | Time spent waiting for the upstream provider.                    |
| `latency.total_ms`         | int    | End-to-end request latency.                                      |
| `upstream.status_code`     | int    | HTTP status from upstream (0 = no status captured, e.g. success or stream error). |
| `routing.cross_format`     | bool   | Whether the request crossed wire formats (e.g. Anthropic→OpenAI).|


**Example configurations:**

Any OTLP-compatible collector (Jaeger, OpenTelemetry Collector, Datadog Agent, etc.):

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318
OTEL_SERVICE_NAME=weave-router
OTEL_RESOURCE_ATTRIBUTES=deployment.environment=production,service.version=1.0.0
```

If your collector requires authentication, pass credentials via headers:

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=https://your-collector.example.com
OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer YOUR_TOKEN
OTEL_SERVICE_NAME=weave-router
```

High-throughput tuning (increase batch size, add workers):

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318
OTEL_SERVICE_NAME=weave-router
OTEL_BSP_MAX_QUEUE_SIZE=5000
OTEL_BSP_MAX_EXPORT_BATCH_SIZE=200
OTEL_BSP_SCHEDULE_DELAY=1000
OTEL_EXPORT_WORKERS=4
```

## Project layout

The repository root holds the build/run plumbing — `go.mod`, `Makefile`,
`Dockerfile`, `docker-compose.yml`, `.env.development` (committed
defaults) — and shared docs under [`docs/`](docs/) (`architecture/`,
`eval/`, `plans/`, `testing/`), plus root `README.md` and the `AGENTS.md` ↔
`CLAUDE.md` mirror pair.
Everything below is directory-only:

```
router/
├── docs/                      # architecture, eval results, plans, testing guide
│   ├── architecture/
│   ├── eval/
│   ├── plans/
│   │   └── archive/           # superseded planning docs (historical context)
│   └── testing/
├── cmd/
│   ├── router/                # composition root + multiversion cluster scorer bootstrap
│   ├── initdb/                # one-shot DB + schema creation
│   └── seed/                  # seeds an installation + API key (prints token once)
├── internal/
│   ├── api/                   # gin handlers, grouped by surface
│   │   ├── admin/             # /health, /validate
│   │   ├── anthropic/         # /v1/messages, metadata passthrough
│   │   └── openai/            # /v1/chat/completions
│   ├── auth/                  # identity domain: Service.VerifyAPIKey, APIKeyCache, id/hashing
│   ├── proxy/                 # routing/dispatch service: Route, Dispatch, ProxyMessages, ProxyOpenAIChatCompletion
│   ├── translate/             # OpenAI <-> Anthropic wire-format conversion (pure, no I/O)
│   ├── router/                # routing-decision types, Router interface, ModelSpec/ModelRegistry
│   │   ├── heuristic/         # Rules: deterministic token-threshold router (fallback)
│   │   ├── cluster/           # Scorer + Multiversion: AvengersPro cluster routing
│   │   │   └── artifacts/     # versioned bundles (v0.1/, v0.2/, v0.3/, ...) plus the `latest` pointer file
│   │   └── evalswitch/        # per-request primary/fallback switch for A/B eval
│   ├── providers/             # Client interface + Request/Response types
│   │   ├── anthropic/         # Anthropic Messages adapter (HTTP/2 pool)
│   │   ├── openai/            # OpenAI Chat Completions adapter
│   │   ├── google/            # Google Gemini adapter via OpenAI-compatible endpoint
│   │   └── noop/              # Null Object Client (test double for proxy.Service unit tests; not wired)
│   ├── postgres/              # SQLC adapter (PoEAA Repository over Data Mapper)
│   ├── sqlc/                  # SQLC-generated; regenerated by `make generate`
│   ├── sse/                   # streaming SSE JSON parsing helpers
│   ├── server/                # gin route registration
│   │   └── middleware/        # auth, timeout, eval-routing-override, embed-override, cluster-version-override
│   ├── config/                # env helpers (MustGet, GetOr, PostgresDSN)
│   └── observability/         # slog logger + gin per-request middleware
│       └── otel/              # async OTLP/HTTP span export (Buffer, Emitter, UsageExtractor, Pricing)
├── assets/                    # runtime assets (model.onnx loaded at boot; content gitignored)
│   └── eval/                  # committed eval Pareto plots referenced by docs/eval/EVAL_RESULTS.md
├── scripts/                   # offline Python tooling (cluster training pipeline, HF download, legacy)
│   ├── ingest/                # benchmark dataset ingestion helpers
│   └── tests/                 # pytest suite for the scripts package
├── eval/                      # Phase 1a eval harness (Python, Modal-driven)
│   ├── benchmarks/            # benchmark dataset loaders (aider_polyglot, bfcl_v4, ...)
│   ├── judges/                # LLM judges (ensemble, gemini, gpt5)
│   └── tests/                 # pytest suite (no external services required)
└── db/
    ├── init/                  # docker-compose init scripts (creates the `router` schema)
    ├── queries/               # SQLC source SQL (organized by primary table)
    └── migrations/            # golang-migrate format (new files via `make migrate-create`)
```

## Development

### Cluster-routing artifacts (HuggingFace Hub)

The cluster scorer's INT8-quantized ONNX model is **not** committed to
git. We use Jina's own quantization at
`[jinaai/jina-embeddings-v2-base-code](https://huggingface.co/jinaai/jina-embeddings-v2-base-code)`
(**public** — no token required), specifically the file
`onnx/model_quantized.onnx` that the model authors maintain. The Go
embedder loads from `/opt/router/assets/` at boot
(`internal/router/cluster/embedder_onnx.go`); override via
`ROUTER_ONNX_ASSETS_DIR`.

**Self-hosting / open-source:** the build pulls anonymously from the
public HF repo:

```bash
git clone https://github.com/workweave/router.git
docker build -t weave-router .
docker compose up
```

If the public HF rate limit ever bites you in CI, supply an HF token
(any valid HF account works — read scope is enough): pass
`--secret id=hf_token,env=HF_TOKEN` to `docker build`. The
`[Dockerfile](Dockerfile)` treats the secret as optional.

**Pinning a revision:** the `[Dockerfile](Dockerfile)` defaults
`HF_MODEL_REVISION` to a known-good Jina commit SHA (last weight
change was Apr 2024, so drift risk is low — but pinning is free).
Bump it deliberately if you want to pick up a new Jina export.

**Local dev (Go integration test):** populate the assets directory
once via the download script. No token needed for the public repo.

```bash
cd router/scripts
poetry install
poetry run python download_from_hf.py
```

If `model.onnx` is missing or under 1 MiB, `cluster.NewEmbedder`
errors at boot and `main.go` fail-opens to the heuristic — the router
stays serviceable but the cluster scorer is inactive until the file
is populated.

To **retrain the cluster centroids/rankings** (Python training
pipeline, ~30 minutes on a laptop), see
`[scripts/README.md](scripts/README.md)`. The embedder weights
themselves come from Jina — we don't maintain our own quantization.

### Regenerating SQLC

```bash
make generate
```

The router's `db/sqlc.yml` runs in **schema-only** mode (no `database:`
block), so SQLC parses the migration files directly to build its type model.
No running Postgres is required for code generation. 

Generated code lives at `[internal/sqlc/](internal/sqlc)` and is committed to
git so `docker compose build` (and CI) work without `sqlc` installed.

### Adding a migration

```bash
# Scaffold a new up/down pair via golang-migrate (timestamp-prefixed).
make migrate-create NAME=add-xyz

# Edit the generated files.
$EDITOR db/migrations/<ts>_add-xyz.up.sql
$EDITOR db/migrations/<ts>_add-xyz.down.sql

# Apply via the compose stack (drops + recreates the postgres volume for a clean run):
docker compose down -v && docker compose up --build -d

# Or via the Makefile against the DB pointed at by DATABASE_URL:
make migrate-up      # apply all pending
make migrate-down    # roll back the most recent
```

After editing migrations, regenerate SQLC: `make generate`.

### Adding a query

Add it to one of the `.sql` files in `db/queries/` (organize by primary table)
and run `make generate`. Then update the corresponding adapter method in
`internal/postgres/repository.go`. Don't call `*sqlc.Queries` from anywhere
outside `internal/postgres/`.

### Tests

```bash
make test                                # all tests
go test -v ./internal/auth/...           # narrower
```

`auth.Service` and `proxy.Service` are unit-tested with in-memory fakes
for the repositories, router, and provider — no DB or HTTP server required.
See `[internal/auth/service_test.go](internal/auth/service_test.go)` and
`[internal/proxy/service_test.go](internal/proxy/service_test.go)`.
Translation logic is tested in
`[internal/translate/translate_test.go](internal/translate/translate_test.go)`
and exercises both directions plus the streaming SSE decorator.

## Roadmap

Future work:

- Admin endpoint for issuing API keys (today: `cmd/seed/main.go` or direct SQL into `model_router_installations` / `model_router_api_keys`)
- Token-aware rate limiting (Redis sliding window keyed by `installation_id`)
- Sub-installations (parent FK on `model_router_installations` for tenant
hierarchies)
- Speculative dispatch + hedging for tail latency
