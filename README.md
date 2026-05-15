# Weave Router

[![Weave Badge](https://img.shields.io/endpoint?url=https%3A%2F%2Fapp.workweave.ai%2Fapi%2Frepository%2Fbadge%2Forg_QWsHDcRQWQEs6RpkdEZrlFK8%2F805349704&cacheSeconds=3600)](https://app.workweave.ai/reports/repository/org_QWsHDcRQWQEs6RpkdEZrlFK8/805349704)
[![Go Version](https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go)](go.mod)
[![Tests](https://github.com/workweave/router/actions/workflows/test.yml/badge.svg)](https://github.com/workweave/router/actions/workflows/test.yml)
[![License: ELv2](https://img.shields.io/badge/License-ELv2-00BFB3.svg)](https://www.elastic.co/licensing/elastic-license)

A standalone Go service for authenticating and routing LLM completions
to the most appropriate provider. The router proxies Anthropic Messages
and OpenAI Chat Completions requests, picks a model per-request via an
AvengersPro-derived cluster scorer, and dispatches to Anthropic, OpenAI,
Google Gemini, or any OpenAI-compatible endpoint (OpenRouter, vLLM,
Together, Fireworks, etc.).

#### Developed by: [Weave](https://www.workweave.ai) (the #1 engineering intelligence platform, loved by Robinhood, Posthog & Reducto)

## Quick start

**1. Add at least one upstream provider key to `.env.local` first.**
OpenRouter is the recommended baseline — it unlocks the full OSS-model
pool the cluster scorer is trained against:

```bash
echo "OPENROUTER_API_KEY=sk-or-v1-..." >> .env.local
# optionally add provider-direct keys too:
# echo "ANTHROPIC_API_KEY=sk-ant-..."  >> .env.local
# echo "OPENAI_API_KEY=sk-..."         >> .env.local
# echo "GOOGLE_API_KEY=..."            >> .env.local
```

See [Configuring API keys](#configuring-api-keys) for all supported
providers. These keys are loaded into the router process at boot; doing
this before `full-setup` means the router comes up already able to serve
traffic.

**2. Boot the stack and seed a router API key.**

```bash
make full-setup
```

`make full-setup` starts Postgres + the router on
`http://localhost:8080`, runs migrations, and seeds one installation +
`rk_...` key (printed to stdout). The seeded key lives on the dashboard's
admin installation, so it shows up under <http://localhost:8080/ui/> →
**Router keys** and can be rotated from there.

**3. (Optional) Add or rotate keys from the dashboard.**
Open <http://localhost:8080/ui/> (default password: `admin`) → **Provider
keys** to add BYOK keys without editing `.env.local` or restarting. BYOK
keys are stored in Postgres (encrypted at rest when
`EXTERNAL_KEY_ENCRYPTION_KEY` is set) and resolved per-request. If you
later change a key in `.env.local`, run `docker compose restart server`
to pick it up.

Once you have at least one provider key in place, you can call the router:

```bash
ROUTER_KEY=rk_...

# Anthropic Messages format
curl -sS http://localhost:8080/v1/messages \
  -H "Authorization: Bearer $ROUTER_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4-5",
    "max_tokens": 256,
    "messages": [{"role": "user", "content": "Hello"}]
  }'

# OpenAI Chat Completions format
curl -sS http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $ROUTER_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o-mini",
    "messages": [{"role": "user", "content": "Hello"}]
  }'

# Get the routing decision without proxying upstream
curl -sS http://localhost:8080/v1/route \
  -H "Authorization: Bearer $ROUTER_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model": "claude-sonnet-4-5", "messages": [{"role":"user","content":"Hello"}]}'
```

Useful follow-ups:

```bash
make logs   # tail server logs (per-request access lines at INFO)
make down   # stop the stack (keeps the postgres volume)
```

> **Dashboard password.** Defaults to `admin` when `ROUTER_ADMIN_PASSWORD`
> is not set (the router logs a warning). Set it in `.env.local` for any
> deployment you care about securing:
> `echo "ROUTER_ADMIN_PASSWORD=your-strong-password" >> .env.local`
>
> **Disabling the dashboard entirely.** Set
> `ROUTER_DEPLOYMENT_MODE=managed` to skip mounting `/ui/*` and the
> `/admin/v1/*` API. Used by SaaS deployments that have their own admin
> frontend.

### Wiring Claude Code or Cursor

`make full-setup` boots the router on `localhost:8080`, seeds an
`rk_...` key, and runs the Claude Code installer interactively (it
prompts whether to wire user scope or a project directory).

Or manually:

**Claude Code:**

```bash
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_CUSTOM_HEADERS="X-Weave-Router-Key: rk_..."
claude
```

**Cursor:**

1. Open Cursor Settings → Models → Override OpenAI Base URL.
   Set to `http://localhost:8080/v1`.
2. Add an API key: paste the `rk_...` value.

To wire an already-running router (e.g. a shared/staging deployment)
instead of booting locally:

```bash
make full-setup KEY=rk_... BASE_URL=https://router.example.com
```

> **Two different keys, do not confuse them.**
>
> - `sk-or-...` / `sk-ant-...` / `sk-...` — your **upstream provider**
>   key. The router uses it to call the LLM API. Lives in `.env.local`.
>   Never sent to clients.
> - `rk_...` — your **router** key. Clients (Claude Code, Cursor, your
>   app) send this to the router as a `Bearer` token or
>   `X-Weave-Router-Key` header. It's not a provider key.

### Hot-reload development

For iterating on router code itself with `CompileDaemon`:

```bash
make db                                # start Postgres only (port 5433)
echo "DATABASE_URL=postgresql://router:router@localhost:5433/router?sslmode=disable" >> .env.local
make setup                             # init schema + migrate + seed an rk_ key
make dev                               # run the server with hot reload
```

Prerequisites: Go 1.25+,
[golang-migrate](https://github.com/golang-migrate/migrate),
[CompileDaemon](https://github.com/githubnemo/CompileDaemon).

The cluster scorer uses an ONNX embedder; on Apple Silicon you also need:

```bash
# Populate ./assets/ first — see "Cluster-routing artifacts" below.
echo "ROUTER_ONNX_ASSETS_DIR=$(pwd)/assets" >> .env.local
echo "CGO_LDFLAGS=-L/path/to/libtokenizers" >> .env.local
echo "ROUTER_ONNX_LIBRARY_DIR=/opt/homebrew/lib" >> .env.local
```

(`brew install onnxruntime`; `libtokenizers` from
[daulet/tokenizers releases](https://github.com/daulet/tokenizers/releases).)
Without these the cluster scorer fails at boot and the router refuses to
start. The Docker path bundles all of this — Apple Silicon CGO setup
only matters for the `make dev` flow.

## Endpoints

| Endpoint                    | Method | Auth          | Purpose                                                                             |
| --------------------------- | ------ | ------------- | ----------------------------------------------------------------------------------- |
| `/health`                   | GET    | none          | Cheap liveness probe. Used by Cloud Run / Compose healthchecks.                     |
| `/validate`                 | GET    | bearer        | Bearer-key validity check. Returns the matched installation on success.             |
| `/v1/messages`              | POST   | bearer or dev | Anthropic Messages proxy. Routes to a model, dispatches to the upstream provider.   |
| `/v1/chat/completions`      | POST   | bearer or dev | OpenAI Chat Completions proxy. Same routing logic as `/v1/messages`.                |
| `/v1/messages/count_tokens` | POST   | bearer | Anthropic passthrough — forwarded as-is.                                            |
| `/v1/models`                | GET    | bearer | Anthropic passthrough — model availability list.                                    |
| `/v1/models/:model`         | GET    | bearer | Anthropic passthrough — single-model lookup.                                        |
| `/v1/route`                 | POST   | bearer | Returns the routing decision (model, provider, reason) without proxying upstream.   |
| `/v1beta/models/:modelAction` | POST | bearer | Google Gemini native format (`generateContent` / `streamGenerateContent`). Same routing logic as `/v1/messages`. |

## Configuring API keys

The router registers each provider only when its API key is present in
the environment. Anthropic is special: when `ANTHROPIC_API_KEY` is unset,
the router still registers the provider but forwards Anthropic auth
headers (`Authorization` / `x-api-key`) to `api.anthropic.com` directly.
This lets Claude Code keep using the user's logged-in plan.

| Variable                   | Default                                                   | Effect                                                                                                                          |
| -------------------------- | --------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| `OPENROUTER_API_KEY`       | *(none)*                                                  | **Recommended.** Enables the OpenRouter provider, unlocking the OSS-model pool the cluster scorer is trained against.           |
| `OPENROUTER_BASE_URL`      | `https://openrouter.ai/api/v1`                            | Override for OpenRouter (or any OpenAI-compatible endpoint — vLLM, Together, Fireworks, customer-hosted).                       |
| `ANTHROPIC_API_KEY`        | *(none — passthrough)*                                    | Router's own Anthropic key. When set, used for all Anthropic requests. When unset, client `Authorization` headers are passed through. |
| `OPENAI_API_KEY`  | *(none)*                                                  | Enables the OpenAI provider (Chat Completions API).                                                                             |
| `OPENAI_BASE_URL` | `https://api.openai.com`                                  | Override for OpenAI (e.g. Azure OpenAI).                                                                                        |
| `GOOGLE_API_KEY`  | *(none)*                                                  | Enables the Google Gemini provider via its OpenAI-compatible endpoint.                                                          |
| `GOOGLE_BASE_URL` | `https://generativelanguage.googleapis.com/v1beta/openai` | Override for Gemini.                                                                                                            |

**Recommended baseline:** set `OPENROUTER_API_KEY` only. That's enough
to exercise the cluster scorer end-to-end against the full OSS-model
catalog. Add provider-direct keys when you want first-party Anthropic /
OpenAI / Google traffic.

**BYOK (per-installation provider keys):** instead of (or in addition
to) the deployment-wide env vars above, the dashboard lets each
installation supply its own provider keys. Those are stored in Postgres
and used for that installation's traffic only. See
[BYOK encryption](#byok-encryption) for the at-rest encryption knob.

## Configuration

All configuration is via environment variables
([12-factor](https://12factor.net/config)).

### Postgres

Set `DATABASE_URL` directly, or compose it from the individual vars:

| Variable                   | Default                                  | Purpose                                             |
| -------------------------- | ---------------------------------------- | --------------------------------------------------- |
| `DATABASE_URL`             | *(none)*                                 | Full Postgres connection string (takes precedence). |
| `POSTGRES_USER`            | *(required if no `DATABASE_URL`)*        | Username.                                           |
| `POSTGRES_PASSWORD`        | *(required if no `DATABASE_URL`)*        | Password.                                           |
| `POSTGRES_DB`              | *(required if no `DATABASE_URL`)*        | Database name.                                      |
| `POSTGRES_HOST`            | *(required if no `DATABASE_URL`)*        | Hostname.                                           |
| `POSTGRES_PORT`            | `5432`                                   | Port.                                               |
| `POSTGRES_SSLMODE`         | `require`                                | TLS mode. `disable` for local Docker.               |
| `POSTGRES_CONNECTION_NAME` | *(none)*                                 | Cloud SQL Auth Proxy instance connection name.      |

### Server

| Variable                    | Default      | Purpose                                                                                                          |
| --------------------------- | ------------ | ---------------------------------------------------------------------------------------------------------------- |
| `PORT`                      | `8080`       | HTTP listen port.                                                                                                |
| `ROUTER_DEPLOYMENT_MODE`    | `selfhosted` | `selfhosted` mounts `/ui/*` and `/admin/v1/*`. `managed` skips both (for SaaS deployments with a separate admin UI). |
| `ROUTER_ADMIN_PASSWORD`     | `admin`      | Password for the admin dashboard. Defaults to `admin` with a warning when unset — set this for any internet-facing deployment. |

### Routing

| Variable                          | Default                      | Purpose                                                                                |
| --------------------------------- | ---------------------------- | -------------------------------------------------------------------------------------- |
| `ROUTER_CLUSTER_VERSION`          | *(reads `artifacts/latest`)* | Pin a specific cluster artifact version (e.g. `v0.27`).                                |
| `ROUTER_CLUSTER_EMBED_TIMEOUT_MS` | `200`                        | Per-request ONNX embed timeout. Increase for slower hosts.                             |
| `ROUTER_EMBED_ONLY_USER_MESSAGE`  | `true`                       | Feed only user-role text (no system, assistant, or tool_result) to the embedder. Set `false` to fall back to the concatenated turn context. |
| `ROUTER_STICKY_DECISION_TTL_MS`   | `0` (disabled)               | Reuse a routing decision per API key for this many ms.                                 |
| `ROUTER_SESSION_PIN_ENABLED`      | `true`                       | Pin a session to its first-routed model so multi-turn conversations stay coherent.     |
| `ROUTER_HARD_PIN_MODEL`           | *(none)*                     | Force every request to a specific model, bypassing the cluster scorer. Debugging only. |
| `ROUTER_HARD_PIN_PROVIDER`        | *(none)*                     | Pair with `ROUTER_HARD_PIN_MODEL` to also force the provider.                          |
| `ROUTER_ONNX_ASSETS_DIR`          | `/opt/router/assets`         | Directory containing `model.onnx`.                                                     |
| `ROUTER_ONNX_LIBRARY_DIR`         | *(system default)*           | Path to `libonnxruntime` (e.g. `/opt/homebrew/lib` on Apple Silicon).                  |

If the cluster scorer can't run (missing model, embed timeout, etc.),
the router returns HTTP 503 rather than silently falling back to a
default model. Failures are loud by design.

### BYOK encryption

| Variable                      | Default   | Purpose                                                                                                                                                                                                                                                                                                                       |
| ----------------------------- | --------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `EXTERNAL_KEY_ENCRYPTION_KEY` | *(unset)* | Tink AES-256-GCM keyset (JSON) that encrypts customer-supplied upstream provider keys at rest. **If unset, BYOK secrets are stored unencrypted** and the router logs a `WARN` at startup. Set this in any deployment that handles real customer secrets. Generate with `tinkey create-keyset --key-template AES256_GCM --out-format json`. |

A *malformed* keyset still fails closed (the router refuses to boot);
only a genuinely absent value triggers the unencrypted-bypass.

### Telemetry (OpenTelemetry)

The router exports per-request trace spans to any OTLP-compatible
collector. Each proxied request emits two spans (`router.decision` and
`router.upstream`) with routing decisions, token usage, cost estimates,
and latency. Export is async/non-blocking; when `OTEL_EXPORTER_OTLP_ENDPOINT`
is unset, OTel is fully disabled at zero runtime cost.

| Variable                         | Default      | Purpose                                                                |
| -------------------------------- | ------------ | ---------------------------------------------------------------------- |
| `OTEL_EXPORTER_OTLP_ENDPOINT`    | *(disabled)* | Collector base URL (e.g. `https://api.honeycomb.io`). Required to enable. |
| `OTEL_EXPORTER_OTLP_HEADERS`     | *(none)*     | Comma-separated `key=value` headers (e.g. auth tokens).                |
| `OTEL_EXPORTER_OTLP_TIMEOUT`     | `10000`      | Per-export HTTP timeout in milliseconds.                               |
| `OTEL_SERVICE_NAME`              | `router`     | `service.name` resource attribute.                                     |
| `OTEL_RESOURCE_ATTRIBUTES`       | *(none)*     | Comma-separated `key=value` resource attributes.                       |
| `OTEL_BSP_MAX_QUEUE_SIZE`        | `1000`       | Span queue capacity. Spans drop when full.                             |
| `OTEL_BSP_MAX_EXPORT_BATCH_SIZE` | `50`         | Max spans per OTLP POST.                                               |
| `OTEL_BSP_SCHEDULE_DELAY`        | `500`        | Partial-batch flush interval in ms.                                    |
| `OTEL_EXPORT_WORKERS`            | `2`          | Export-goroutine count.                                                |

The first five follow the
[OTel SDK env spec](https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables/);
`OTEL_BSP_*` follows the
[Batch Span Processor spec](https://opentelemetry.io/docs/specs/otel/trace/sdk/#batch-span-processor).
`OTEL_EXPORT_WORKERS` is a router-specific extension.

## Architecture

The router uses three concentric layers with imports flowing inward only:

- **Inner ring** — `internal/auth` (identity), `internal/proxy` (routing
  + dispatch), `internal/router` (Router interface + decision types),
  `internal/providers` (Client interface), `internal/translate` (pure
  OpenAI ↔ Anthropic wire-format conversion). No I/O outside Service
  methods.
- **Adapters** — `internal/postgres` (SQLC over pgx), `internal/router/cluster`
  (AvengersPro-derived scorer with embedded versioned artifacts), and
  the per-provider clients under `internal/providers/{anthropic,openai,google,openaicompat}`.
- **Presentation** — `internal/api/{admin,anthropic,openai,gemini}` for
  HTTP handlers, `internal/server` for route registration, and
  `internal/server/middleware` for auth, request timeouts, and the
  per-request cluster-version / embed-strategy overrides used by the
  eval harness.
- **Composition** — `cmd/router/main.go` is the only place that
  constructs concrete adapters and wires them into the services.

See [AGENTS.md](AGENTS.md) for the full layering rules, package-level
import contracts, and the recipes for adding endpoints, providers,
migrations, and routing strategies.

## Development

### Regenerating SQLC

```bash
make generate
```

The router's `db/sqlc.yml` runs in **schema-only** mode (no `database:`
block), so SQLC parses the migration files directly. No running Postgres
is required for code generation. Generated code at `internal/sqlc/` is
committed so `docker compose build` and CI work without `sqlc` installed.

### Adding a migration

```bash
make migrate-create NAME=add-xyz
$EDITOR db/migrations/<ts>_add-xyz.up.sql
$EDITOR db/migrations/<ts>_add-xyz.down.sql
make migrate-up      # apply pending against $DATABASE_URL
make migrate-down    # roll back the most recent
make generate        # regenerate SQLC after migration changes
```

Migrations must be wrapped in `BEGIN; ... COMMIT;`. Down migrations
must be precise rollbacks of the up — no `IF EXISTS` guards.

### Adding a query

Edit one of the `.sql` files in `db/queries/` (organized by primary
table) and run `make generate`. Then update the corresponding adapter
method in `internal/postgres/repository.go`. Don't call `*sqlc.Queries`
from anywhere outside `internal/postgres/`.

### Tests

```bash
make test                           # all tests
make check                          # generate + build + test (CI-equivalent)
go test -v ./internal/auth/...      # narrower
```

`auth.Service` and `proxy.Service` are unit-tested with in-memory fakes
for repositories, routers, and provider clients — no DB or HTTP server
required.

### Cluster-routing artifacts

The cluster scorer needs two files at runtime: `model.onnx` (the
INT8-quantized embedder) and `tokenizer.json`. Neither is committed to
git — both come from the public
[`jinaai/jina-embeddings-v2-base-code`](https://huggingface.co/jinaai/jina-embeddings-v2-base-code)
HuggingFace repo. We use Jina's own INT8 export; we don't maintain our
own quantization.

**Docker (default):** the Dockerfile downloads both files at image
build time into `/opt/router/assets/`. Nothing for you to do.

**`make dev` (host-mode hot reload):** fetch them once into a local
directory and point `ROUTER_ONNX_ASSETS_DIR` at it:

```bash
mkdir -p assets
BASE="https://huggingface.co/jinaai/jina-embeddings-v2-base-code/resolve/516f4baf13dec4ddddda8631e019b5737c8bc250"
curl -L "$BASE/onnx/model_quantized.onnx" -o assets/model.onnx
curl -L "$BASE/tokenizer.json" -o assets/tokenizer.json
echo "ROUTER_ONNX_ASSETS_DIR=$(pwd)/assets" >> .env.local
```

The pinned revision matches `HF_MODEL_REVISION` in the Dockerfile, so
local dev and the container build use the same weights. Bump both
together if you want a newer upstream export.

The committed cluster artifacts (centroids, rankings, model registry,
metadata) live under `internal/router/cluster/artifacts/v<X.Y>/`. The
`artifacts/latest` pointer selects the default served version;
`ROUTER_CLUSTER_VERSION` overrides per-deployment.

## Roadmap

- Token-aware rate limiting (Redis sliding window keyed by
  `installation_id`).
- Sub-installations (parent FK on `model_router_installations` for
  tenant hierarchies).
- Speculative dispatch + hedging for tail latency.
