# Configuration reference

All router configuration is via environment variables ([12-factor](https://12factor.net/config)).
This page is the exhaustive reference; the [README](../README.md) has the
60-second quickstart.

## Table of contents

- [Provider API keys](#provider-api-keys)
- [Postgres](#postgres)
- [Server](#server)
- [Routing](#routing)
- [Provider and model exclusions](#provider-and-model-exclusions)
- [Policy sidecars](#policy-sidecars)
- [BYOK encryption](#byok-encryption)
- [Telemetry (OpenTelemetry)](#telemetry-opentelemetry)
- [Cluster-routing artifacts](#cluster-routing-artifacts)

## Provider API keys

The router registers each upstream provider only when its API key is present
in the environment. Anthropic is special: when `ANTHROPIC_API_KEY` is unset,
the router still registers the provider but forwards Anthropic auth headers
(`Authorization` / `x-api-key`) to `api.anthropic.com` directly. This lets
Claude Code keep using the user's logged-in plan.

| Variable              | Default                                                   | Effect |
| --------------------- | --------------------------------------------------------- | ------ |
| `OPENROUTER_API_KEY`  | *(none)*                                                  | **Recommended baseline.** Enables OpenRouter and the full OSS-model pool the cluster scorer is trained against. |
| `OPENROUTER_BASE_URL` | `https://openrouter.ai/api/v1`                            | Override for OpenRouter or any OpenAI-compatible endpoint (vLLM, Together, Fireworks, self-hosted). |
| `TRUSTEDROUTER_API_KEY`  | *(none)*                                               | Enables TrustedRouter as an OpenAI-compatible aggregate provider. |
| `TRUSTEDROUTER_BASE_URL` | `https://api.trustedrouter.com/v1`                     | Override for TrustedRouter-compatible deployments. |
| `ANTHROPIC_API_KEY`   | *(none — passthrough)*                                    | Router's own Anthropic key. When unset, client `Authorization` headers pass through. |
| `OPENAI_API_KEY`      | *(none)*                                                  | Enables the OpenAI provider (Chat Completions API). |
| `OPENAI_BASE_URL`     | `https://api.openai.com`                                  | Override for OpenAI (e.g. Azure OpenAI). |
| `GOOGLE_API_KEY`      | *(none)*                                                  | Enables Gemini via its OpenAI-compatible endpoint. |
| `GOOGLE_BASE_URL`     | `https://generativelanguage.googleapis.com/v1beta/openai` | Override for Gemini. |
| `ANTHROPIC_GATEWAY_BASE_URL` | *(none)*                                           | Base URL of an Anthropic-compatible gateway; `/v1/messages` is appended to it. |
| `ANTHROPIC_GATEWAY_TOKEN`    | *(none)*                                           | Token for that gateway, sent as `Authorization: Bearer`. Only used when `ANTHROPIC_GATEWAY_BASE_URL` is also set. |

**Anthropic-compatible gateway.** Some enterprises front Claude with their own
gateway that speaks the Anthropic Messages spec but authenticates with a bearer
token instead of `x-api-key`. The router serves the Claude family through it on
the same translation path as direct Anthropic. There is no default endpoint: an
unconfigured gateway does *not* fall back to `api.anthropic.com`. The provider
is always registered so BYOK installations can point at their own gateway
without deployment-level credentials; the env vars above are only for a
deployment that has a gateway of its own.

**BYOK (per-installation keys).** Instead of (or in addition to) the env vars
above, each installation can supply its own provider keys via the dashboard.
Those are stored in Postgres and used only for that installation's traffic.
See [BYOK encryption](#byok-encryption).

Each key may also carry its own endpoint, which overrides the deployment's base
URL for that provider on that installation's requests. Set it in **Settings →
Provider API keys → Endpoint URL**, or through the admin API:

```bash
# /admin/v1 mutations take the dashboard cookie, not an rk_ bearer.
curl -sS -c jar -X POST https://<router>/admin/v1/auth/login \
  -H 'content-type: application/json' -d '{"password":"<admin password>"}'

curl -sS -b jar -X POST https://<router>/admin/v1/provider-keys \
  -H 'content-type: application/json' \
  -d '{"provider":"anthropic_gateway","key":"<token>","base_url":"https://gateway.example.com/api"}'
```

The value must be an absolute `http(s)` URL; anything else is rejected with
`400`. A trailing slash is stripped, and the provider appends its own API path
(`/v1/messages` for the Anthropic family), so give the base only. Omit the
field to keep the deployment endpoint — except for `anthropic_gateway`, which
has no default to fall back to and rejects a key without one.

A key may also carry a model alias map for endpoints that publish the catalog's
models under their own names:

```bash
curl -sS -b jar -X POST https://<router>/admin/v1/provider-keys \
  -H 'content-type: application/json' \
  -d '{"provider":"anthropic_gateway","key":"<token>","base_url":"https://gateway.example.com/api",
       "model_aliases":{"claude-fable-5":"internal.claude-fable-5"}}'
```

Keys are catalog model IDs (an ID outside the deployed catalog is rejected with
`400`) and values are what goes on the wire to that endpoint. Only the outbound
model name changes: routing, pricing, and analytics stay keyed on the catalog
ID. Omit the field to send catalog IDs unchanged.

An endpoint that authenticates the org rather than the person can be given the
calling user in a header of its choosing:

```bash
curl -sS -b jar -X POST https://<router>/admin/v1/provider-keys \
  -H 'content-type: application/json' \
  -d '{"provider":"anthropic_gateway","key":"<token>",
       "identity_header":"X-Caller-Identity","identity_header_format":"json"}'
```

`email` sends the bare address; `json` sends a percent-encoded JSON property bag
(`user_email`, `user_name`, `session_id`, `client_app`, empty fields omitted).
The header is set on the upstream request after the client's own headers, so a
caller can't attribute their turns to someone else by sending it themselves, and
nothing is sent when the request carries no identity. Naming a header the
request depends on (`Authorization`, `x-api-key`, `Host`, `Content-Type`,
`Content-Length`, `Accept`) is rejected with `400`. Omit both fields to forward
nothing — identity only ever reaches the endpoint configured to receive it.

In `selfhosted` mode BYOK is always active (it's the only credentialing path).
In `managed` mode it is opt-in per installation: the control plane sets
`byok_enabled` on the installation row, and until it does, the auth middleware
strips BYOK keys so a stored key can't spend against a deployment that bills
prepaid credits. Once enabled, a BYOK turn debits no inference cost (the
customer paid their own provider) and is charged a platform fee instead,
recorded as a separate `byok_fee` ledger row.

## Postgres

Set `DATABASE_URL` directly, or compose it from the individual vars:

| Variable                   | Default                           | Purpose |
| -------------------------- | --------------------------------- | ------- |
| `DATABASE_URL`             | *(none)*                          | Full connection string (takes precedence). |
| `POSTGRES_USER`            | *(required if no `DATABASE_URL`)* | Username. |
| `POSTGRES_PASSWORD`        | *(required if no `DATABASE_URL`)* | Password. |
| `POSTGRES_DB`              | *(required if no `DATABASE_URL`)* | Database name. |
| `POSTGRES_HOST`            | *(required if no `DATABASE_URL`)* | Hostname. |
| `POSTGRES_PORT`            | `5432`                            | Port. |
| `POSTGRES_SSLMODE`         | `require`                         | TLS mode. Use `disable` for local Docker. |
| `POSTGRES_CONNECTION_NAME` | *(none)*                          | Cloud SQL Auth Proxy instance connection name. |

## Server

| Variable                 | Default      | Purpose |
| ------------------------ | ------------ | ------- |
| `PORT`                   | `8080`       | HTTP listen port. |
| `ROUTER_DEPLOYMENT_MODE` | `selfhosted` | `selfhosted` mounts `/ui/*` and `/admin/v1/*`. `managed` skips both (for SaaS deployments with a separate admin UI). |
| `ROUTER_ADMIN_PASSWORD`  | `admin`      | Dashboard password. Defaults to `admin` with a startup warning when unset — **set this for any internet-facing deployment**. |

## Routing

| Variable                          | Default                      | Purpose |
| --------------------------------- | ---------------------------- | ------- |
| `ROUTER_DEFAULT_STRATEGY`         | `cluster`                    | Strategy used when an installation has no persisted strategy. Change only after the policy rollout gate passes. |
| `ROUTER_CLUSTER_VERSION`          | *(reads `artifacts/latest`)* | Pin a specific cluster artifact version (e.g. `v0.27`). |
| `ROUTER_CLUSTER_EMBED_TIMEOUT_MS` | `200`                        | Per-request ONNX embed timeout. Increase for slower hosts. |
| `ROUTER_EMBED_ONLY_USER_MESSAGE`  | `true`                       | Feed only user-role text to the embedder. Set `false` to embed the full concatenated turn. |
| `ROUTER_STICKY_DECISION_TTL_MS`   | `0` (disabled)               | Reuse a routing decision per API key for this many ms. |
| `ROUTER_SESSION_PIN_ENABLED`      | `true`                       | Pin a session to its first-routed model so multi-turn conversations stay coherent. |
| `ROUTER_HARD_PIN_MODEL`           | *(none)*                     | Force every request to a specific model, bypassing the cluster scorer. Debugging only. |
| `ROUTER_HARD_PIN_PROVIDER`        | *(none)*                     | Pair with `ROUTER_HARD_PIN_MODEL`. |
| `ROUTER_HARD_PIN_EXPLORE`         | `true`                       | Pin Claude Code Task-tool sub-agent turns to `ROUTER_HARD_PIN_MODEL`/`ROUTER_HARD_PIN_PROVIDER` (or the cheapest deployed model, if those are unset). Set `false` to route sub-agents through the scorer like any other turn. |
| `ROUTER_SUBAGENT_MODEL`           | *(none)*                     | Route Claude Code Task-tool sub-agent turns to a distinct model, independent of `ROUTER_HARD_PIN_MODEL` — e.g. a local/self-hosted OpenAI-compatible model (point `OPENROUTER_BASE_URL` at your local server) while the main loop keeps using Anthropic/whatever the scorer picks. Requires `ROUTER_SUBAGENT_PROVIDER`; either alone is ignored. Takes effect regardless of `ROUTER_HARD_PIN_EXPLORE`, but the HMM strategy keeps its own sub-agent handling and isn't affected. |
| `ROUTER_SUBAGENT_PROVIDER`        | *(none)*                     | Pair with `ROUTER_SUBAGENT_MODEL`. |
| `ROUTER_TRANSLATION_COMPATIBILITY_MODE` | `shadow` | Translation representability rollout: `off` disables broad filtering, `shadow` records candidate exclusions without changing routes, and `enforce` makes declared semantic requirements hard routing constraints. Native-only safety paths (such as unsupported Responses tool unions and native Gemini ingress) remain protected unless mode is `off`. |
| `ROUTER_COMPACTION_PCT`           | `0.85`                       | Fraction of the largest eligible model's context window at which the proactive compaction cascade engages (clear old tool results → structured summary → trim). Range `(0,1]`; `0` disables compaction (over-window requests then 413). Mirrors Claude Code's ~0.85 auto-compact trigger. |
| `ROUTER_ONNX_ASSETS_DIR`          | `/opt/router/assets`         | Directory containing `model.onnx` + `tokenizer.json`. |
| `ROUTER_ONNX_LIBRARY_DIR`         | *(system default)*           | Path to `libonnxruntime` (e.g. `/opt/homebrew/lib` on Apple Silicon). |

If the cluster scorer can't run (missing model, embed timeout, etc.), the
router returns HTTP 503 — it does *not* silently fall back to a default
model. Failures are loud by design.

## Provider and model exclusions

Exclusions keep traffic away from a provider or model — the control to reach
for when an installation may only talk to, say, its own enterprise gateway.

| Variable                     | Default  | Purpose |
| ---------------------------- | -------- | ------- |
| `ROUTER_EXCLUDED_PROVIDERS`  | *(none)* | Comma-separated provider names no request may be routed to. Pins the list deployment-wide: per-installation edits are refused (403) while it is set. |
| `ROUTER_EXCLUDED_MODELS`     | *(none)* | Comma-separated model IDs no request may be routed to, same deployment-wide pinning. |

Without either env var the lists come from the installation, editable in the
dashboard or through `PUT /admin/v1/excluded-providers` and
`PUT /admin/v1/excluded-models`.

Exclusions are authoritative, not a preference. An excluded provider is
subtracted from the request's eligible set before anything routes, so the
scorer, the turn-type hard pins, session pins, and cross-binding failover all
stay off it — including when the caller holds their own BYOK key for it.

That extends to explicit forcing. `/force-model` and the `x-weave-force-model`
header are refused when every provider that could serve the model is excluded:
the command answers with the reason and leaves routing (and any prior pin)
alone, and the header fails the request with HTTP 400. A model with one
permitted binding left is forced normally and served through that binding. A
live session whose forced pin is later excluded fails the same way rather than
quietly reverting to automatic routing — clear it with `/unforce-model`. The
same holds a level up: exclusions that empty a forced routing cluster fail the
request too (see [Forcing a model or a routing
cluster](#forcing-a-model-or-a-routing-cluster)).

Excluding every provider that serves the models you route to leaves requests
with nowhere to go (HTTP 503 from the scorer), so exclude deliberately.

## Forcing a model or a routing cluster

Two request headers let a headless caller (eval harness, CI, any client whose
UI eats slash commands) override routing. Both fail the request rather than
routing on, so a typo can't look like it took effect.

| Header | Effect |
| ------ | ------ |
| `x-weave-force-model` | Pins the session to one model, exactly as `/force-model` does. Accepts a canonical catalog ID or an alias (`opus`, `gpt`, `qwen-max`, …) plus an optional `:level` effort suffix (`opus:high`). A value naming no catalog model is HTTP 400. |
| `x-weave-force-cluster` | Constrains serving to one of the policy sidecar's routing clusters, leaving the choice *within* it to the policy. |

`x-weave-force-cluster` takes an opaque label — the router holds no list of
valid ones. The live cluster vocabulary belongs to the deployed policy artifact
and changes when it does, so the only authority is the roster the sidecar
reports on that very request; a hardcoded list would silently go stale on the
next roster bump. Consequences:

- A label absent from the live roster is HTTP 400, whether it's a typo or a
  cluster the current artifact retired. Both are equally unservable.
- A label that *is* in the roster but has no eligible model for this request
  (everything in it excluded, over-window, or filtered out on capability) is
  also HTTP 400 — including when a per-key cluster model list empties it.
- The header only works on the `hmm` / `hmm_embedding` strategies. The default
  `cluster` strategy scores anonymous centroids with no named groups, so there
  is nothing to constrain to and the request is HTTP 400 rather than a silent
  no-op.
- A sidecar too old to report its clusters also 400s. The constraint can't be
  proven against a roster the router can't see, and serving anyway would ignore
  the force.

Unlike `x-weave-force-model` the cluster header writes no session pin: every
turn carrying it is constrained on its own merits. Which models make up a
cluster stays control-plane config (the dashboard's per-API-key "Cluster model
lists" panel) — the header only says *which* cluster this turn must come from,
and any list configured for that cluster still orders the arm that serves.

## Policy sidecars

Out-of-process policy routers use the versioned contract in
[Policy router harness](POLICY_ROUTER_HARNESS.md). The router remains the
authority for candidate eligibility, provider binding, dispatch, retries,
privacy context, and telemetry.

| Variable                           | Default | Purpose |
| ---------------------------------- | ------- | ------- |
| `ROUTER_POLICY_SIDECARS`           | *(none)* | JSON object mapping a new strategy ID to its sidecar origin, for example `{"quality-v2":"https://quality-v2.internal"}`. IDs must match `[a-z][a-z0-9_-]{0,63}`. At most 16 may be configured. `cluster`, `rl`, `hmm`, and `bandit` are reserved. |
| `ROUTER_POLICY_SIDECAR_AUTH`       | *(none)* | JSON object mapping configured generic strategy IDs to `none` or `google-id-token`, for example `{"quality-v2":"google-id-token"}`. Google ID-token mode uses the exact sidecar origin as token audience and fails router startup when application default credentials cannot build the client. |
| `ROUTER_POLICY_SIDECAR_TIMEOUT_MS` | `3000`  | Total timeout for each generic policy decision, including transient retries. Also bounds startup capability discovery. |
| `ROUTER_HMM_SIDECAR_URL`           | *(none)* | Legacy built-in HMM registration. Prefer the generic map for new strategies. |
| `ROUTER_HMM_SIDECAR_TIMEOUT_MS`    | `3000`  | Total HMM decision timeout. |
| `ROUTER_HMM_SIDECAR_AUTH`          | `none`  | Authentication for the HMM sidecar. Use `google-id-token` for managed Cloud Run; the exact sidecar origin is used as the token audience. |
| `ROUTER_RL_SIDECAR_URL`            | *(none)* | Legacy built-in RL registration. Prefer the generic map for new strategies. |
| `ROUTER_RL_SIDECAR_TIMEOUT_MS`     | `3000`  | Total RL decision timeout. |
| `ROUTER_RL_SIDECAR_MODAL_KEY`      | *(none)* | Optional Modal proxy token id (`Modal-Key`) when the RL sidecar is a Modal ASGI app with `requires_proxy_auth`. |
| `ROUTER_RL_SIDECAR_MODAL_SECRET`   | *(none)* | Optional Modal proxy token secret (`Modal-Secret`); required when `ROUTER_RL_SIDECAR_MODAL_KEY` is set. |

`GET /capabilities` is queried at router startup. A failed probe does not
silently remove the strategy: serving stays registered and fails closed if
`POST /route` is unavailable, while optional outcome and feedback callbacks
remain disabled until the next successful restart. This keeps persisted
rollout state visible without pretending that a different strategy served.

Policy route requests retry network failures and HTTP 500, 502, 503, and 504
up to three attempts within the configured total timeout. Other failures are
not retried. An unavailable or invalid policy decision returns HTTP 503; it
never falls back to cluster or another policy.

### Self-hosted frozen HMM sidecar

The repository includes an optional companion container under
`sidecars/hmm/`. Start it with `make up-hmm`; the normal `make up` and
`make full-setup` paths remain cluster-only. HMM is not selected unless an
operator explicitly chooses the `hmm` strategy.

| Variable | Default | Purpose |
| --- | --- | --- |
| `HMM_PACKAGE_URL` | Published `hmm-model-v1` GitHub Release asset | HTTPS URL for the portable frozen package. |
| `HMM_PACKAGE_PATH` | *(none)* | Local package path when running the sidecar outside Compose. Set exactly one of path or URL. |
| `HMM_PACKAGE_SHA256` | Pinned release digest in the sidecar image | Required digest for URL downloads; optional but recommended with a local path. |
| `HMM_ARTIFACT_CACHE_DIR` | `/tmp/workweave-hmm-artifacts` | Atomic download/extraction cache. |
| `HMM_EMBEDDING_PROVIDER` | `google` | `google` or `openai-compatible`. |
| `GOOGLE_API_KEY` | *(none)* | Google Gemini API key for the exact embedding model named by the artifact. |
| `HMM_EMBEDDING_BASE_URL` | *(none)* | Base URL for an OpenAI-compatible `/embeddings` endpoint. |
| `HMM_EMBEDDING_API_KEY` | *(none)* | Optional bearer token for that endpoint. |
| `HMM_EMBEDDING_MODEL` | Artifact model ID | Model sent to an OpenAI-compatible endpoint. |

The published v1 package is tied to `google/gemini-embedding-2` at 3,072
dimensions. Those embedding values are direct classifier features and define
the HMM emission space, so another 3,072-dimensional model is not a substitute.
At startup the sidecar embeds a fixed probe and compares it to the reference
vector stored in the artifact. Readiness fails closed when the endpoint serves
an incompatible vector space. A fully local embedder is supported only with a
separately trained package that declares and probes that embedder.

The self-hosted sidecar is frozen: it keeps only a bounded in-memory embedding
cache, advertises no learning/outcome/feedback callbacks, and never persists
request or response content.

Selection precedence is:

1. An authorized internal `x-weave-router-strategy` request override.
2. The installation's persisted strategy.
3. `ROUTER_DEFAULT_STRATEGY`.

The request header is ignored unless the installation explicitly enables
policy-header overrides. `x-weave-router-debug` follows the same authorization
rule and cannot enable training. Shadow decisions are always non-dispatching,
non-debug, and non-learning.

## BYOK encryption

| Variable                      | Default   | Purpose |
| ----------------------------- | --------- | ------- |
| `EXTERNAL_KEY_ENCRYPTION_KEY` | *(unset)* | Tink AES-256-GCM keyset (JSON) that encrypts customer-supplied upstream provider keys at rest. |

**If unset, BYOK secrets are stored unencrypted** and the router logs a
`WARN` at startup. Set this in any deployment that handles real customer
secrets. Generate with:

```bash
tinkey create-keyset --key-template AES256_GCM --out-format json
```

A *malformed* keyset still fails closed (the router refuses to boot); only a
genuinely absent value triggers the unencrypted bypass.

## Telemetry (OpenTelemetry)

The router exports per-request trace spans to any OTLP-compatible collector.
Each proxied request emits two spans (`router.decision` and `router.upstream`)
with routing decisions, token usage, cost estimates, and latency. Export is
async/non-blocking; when `OTEL_EXPORTER_OTLP_ENDPOINT` is unset, OTel is
fully disabled at zero runtime cost. Everything the router records leaves the
process over OTLP only — there is no hardcoded analytics endpoint.

### High-fidelity content capture (`router.call` log records)

When `WV_CAPTURE_CONTENT` is set, the router additionally emits a `router.call`
OTLP **log record** per upstream call to `${OTEL_EXPORTER_OTLP_ENDPOINT}/v1/logs`.
Each record carries the same routing/decision metadata as the spans plus the
call outcome, and — depending on the mode — the request/response bodies. This
is the ML-ready event stream (one record per LLM call, full inputs and
outputs). It is **opt-in**: with `WV_CAPTURE_CONTENT` unset (the default) no
log records are emitted and behavior is unchanged.

| Variable             | Default | Purpose |
| -------------------- | ------- | ------- |
| `WV_CAPTURE_CONTENT` | `off`   | `off` = no log records; `hashed` = metadata + SHA-256 content hashes (no raw text); `full` = metadata + raw request/response bodies. |
| `WV_CAPTURE_MAX_BYTES` | `1048576` | Max buffered response bytes; larger responses are dropped and flagged `io.truncated=true` (the client still receives the full stream). |

Captured bodies are in the client's native wire format (Anthropic / OpenAI /
Gemini, matching the inbound surface). The `router.deployment_mode` resource
attribute (`selfhosted` / `managed`) is stamped on every export so a collector
can branch redaction or content-opt-out by deployment.

`WV_CAPTURE_CONTENT` is the deployment-wide **ceiling**. An installation can
tighten it below that (`GET`/`PUT /admin/v1/content-capture`, body
`{"mode": "off" | "hashed" | "full"}`; `{"mode": null}` clears the override),
and the effective mode for a request is the stricter of the two — so a tenant
on a `full` deployment can opt down to `hashed` or `off`, but an installation
asking for `full` under a `hashed` deployment still gets `hashed`.


| Variable                         | Default      | Purpose |
| -------------------------------- | ------------ | ------- |
| `OTEL_EXPORTER_OTLP_ENDPOINT`    | *(disabled)* | Collector base URL (e.g. `https://api.honeycomb.io`). Required to enable. |
| `OTEL_EXPORTER_OTLP_HEADERS`     | *(none)*     | Comma-separated `key=value` headers (e.g. auth tokens). |
| `OTEL_EXPORTER_OTLP_TIMEOUT`     | `10000`      | Per-export HTTP timeout in ms. |
| `OTEL_SERVICE_NAME`              | `router`     | `service.name` resource attribute. |
| `OTEL_RESOURCE_ATTRIBUTES`       | *(none)*     | Comma-separated `key=value` resource attributes. |
| `OTEL_BSP_MAX_QUEUE_SIZE`        | `1000`       | Span queue capacity. Spans drop when full. |
| `OTEL_BSP_MAX_EXPORT_BATCH_SIZE` | `50`         | Max spans per OTLP POST. |
| `OTEL_BSP_SCHEDULE_DELAY`        | `500`        | Partial-batch flush interval in ms. |
| `OTEL_EXPORT_WORKERS`            | `2`          | Export-goroutine count (spans and logs each get this many workers). |

The first five follow the [OTel SDK env spec](https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables/);
`OTEL_BSP_*` follows the [Batch Span Processor spec](https://opentelemetry.io/docs/specs/otel/trace/sdk/#batch-span-processor).
`OTEL_EXPORT_WORKERS` is a router-specific extension.

## Cluster-routing artifacts

Each embedder the cluster scorer can use needs two files at runtime —
`model.onnx` (INT8-quantized) and `tokenizer.json` — in its own subdirectory
of the assets root, keyed by embedder ID:

- `jina-v2-base-code-int8/` — from the public
  [`jinaai/jina-embeddings-v2-base-code`](https://huggingface.co/jinaai/jina-embeddings-v2-base-code)
  HuggingFace repo (Jina's own INT8 export; we don't maintain our own
  quantization). Default for every bundle through v0.66; the flat legacy
  layout (`<root>/model.onnx`) still resolves for this embedder.
- `qwen3-embedding-0.6b-int8/` — produced by `scripts/export_qwen3_onnx.py`
  (Qwen3-Embedding-0.6B with last-token pooling baked into the graph) and
  uploaded to the public
  [`weave-eng/qwen3-embedding-0.6b-onnx-router`](https://huggingface.co/weave-eng/qwen3-embedding-0.6b-onnx-router)
  HF repo. Only needed when serving a bundle whose `metadata.yaml` declares
  this embedder; the runtime loads embedders lazily.

Neither is committed to git.

**Docker (default):** the Dockerfile downloads the files at image build time
into `/opt/router/assets/<embedder-id>/`. Both repos are public — no token
needed (the optional `hf_token` secret still works for rate-limit headroom);
set `HF_QWEN_REPO=` (empty) to skip the Qwen pull for Jina-only deploys.

**`make dev` (host-mode hot reload):** fetch the Jina files once into a local
directory and point `ROUTER_ONNX_ASSETS_DIR` at it:

```bash
mkdir -p assets/jina-v2-base-code-int8
BASE="https://huggingface.co/jinaai/jina-embeddings-v2-base-code/resolve/516f4baf13dec4ddddda8631e019b5737c8bc250"
curl -L "$BASE/onnx/model_quantized.onnx" -o assets/jina-v2-base-code-int8/model.onnx
curl -L "$BASE/tokenizer.json" -o assets/jina-v2-base-code-int8/tokenizer.json
echo "ROUTER_ONNX_ASSETS_DIR=$(pwd)/assets" >> .env.local
```

To also serve Qwen bundles locally, run `scripts/export_qwen3_onnx.py
--out-dir assets/qwen3-embedding-0.6b-int8` (or download the uploaded export
into that directory).

The pinned revisions (`HF_MODEL_REVISION`, `HF_QWEN_REVISION`) in the
Dockerfile keep local dev and the container build on the same weights. Bump
deliberately if you want a newer export.

The committed cluster artifacts (centroids, rankings, model registry,
metadata) live under `internal/router/cluster/artifacts/v<X.Y>/`. The
`artifacts/latest` pointer selects the default served version;
`ROUTER_CLUSTER_VERSION` overrides per-deployment.
