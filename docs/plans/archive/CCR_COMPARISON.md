Created: 2026-05-02
Last edited: 2026-05-03

# Comparison: `router/` vs `musistudio/claude-code-router` (CCR)

A side-by-side look at what CCR ships that we don't, scoped to two
dimensions: (1) the actual routing mechanics, and (2) the surrounding
product (CLI, UI, packaging, distribution).

CCR is a **local, single-user proxy** distributed as an `npm` package
(`@musistudio/claude-code-router`, v2.0.0, MIT). It runs on `127.0.0.1`,
reads `~/.claude-code-router/config.json`, and proxies Claude Code
traffic to whichever LLM provider the user has wired up. Our `router/`
is a **multi-tenant Go service** with bearer-key auth, Postgres-backed
installations, an embedded ML model for routing, and an OTLP telemetry
pipeline. They overlap in the surface ("a thing in front of Claude
Code that picks a model") but the design centers are very different.

---

## Part 1 — Routing

### What CCR has that we don't

**1. Config-driven routing categories with `/model` runtime override.**
CCR's `Router` block in `config.json` has named scenarios:

```json
"Router": {
  "default": "deepseek,deepseek-chat",
  "background": "ollama,qwen2.5-coder:latest",
  "think": "deepseek,deepseek-reasoner",
  "longContext": "openrouter,google/gemini-2.5-pro-preview",
  "longContextThreshold": 60000,
  "webSearch": "gemini,gemini-2.5-flash",
  "image": "..."
}
```

The router function (`packages/core/src/utils/router.ts`) selects a
scenario from request shape:

- `tokenCount > longContextThreshold` → `longContext`
- request contains `claude` + `haiku` model name → `background`
- `tools[].type` starts with `web_search` → `webSearch`
- `req.body.thinking` present → `think`
- otherwise → `default`

Inside Claude Code the user can also force a specific model with
`/model provider_name,model_name`, and a subagent prompt can pin its
own model with a `<CCR-SUBAGENT-MODEL>provider,model</CCR-SUBAGENT-MODEL>`
sentinel that the router strips before forwarding.

We have **no equivalent runtime override surface** — our routing is
purely cluster-scorer-driven (with a token-threshold heuristic
fallback), and the only per-request override is the eval-allowlist
header (`x-weave-disable-cluster`, `x-weave-cluster-version`) which is
not a customer-facing knob. We have nothing that maps to
`background` / `think` / `webSearch` / `image` scenarios as named
routes.

**2. Custom router scripts.** `CUSTOM_ROUTER_PATH` lets a user point
at a JS module that exports `async (req, config) => "provider,model" | null`
and bypasses the built-in router on a per-request basis. This is
"bring your own routing logic" without recompiling. We have no
equivalent — adding a routing strategy in our codebase means a new
`router.Router` impl in Go and a redeploy.

**3. Transformer plugin system.** CCR's central abstraction for
provider compatibility is the **transformer**: a small request/response
mutator that adapts payloads to/from a given provider's wire quirks.
The full built-in list (`packages/core/src/transformer/`):

`anthropic`, `cerebras`, `cleancache`, `customparams`, `deepseek`,
`enhancetool`, `forcereasoning`, `gemini`, `groq`, `maxcompletiontokens`,
`maxtoken`, `openai`, `openai.responses`, `openrouter`, `reasoning`,
`sampling`, `streamoptions`, `tooluse`, `vercel`, `vertex-claude`,
`vertex-gemini`. Plus user-loaded transformers via the `transformers`
config field, and gist-distributed experimental ones (`gemini-cli`,
`qwen-cli`, `rovo-cli`, `chutes-glm`).

Transformers are composable per-provider and per-model (e.g.
`{"deepseek": {use: ["deepseek"], "deepseek-chat": {use: ["tooluse"]}}}`)
and accept options (`["maxtoken", {"max_tokens": 16384}]`).

We have **`internal/translate`** for OpenAI ↔ Anthropic wire-format
conversion, plus per-adapter stripping of unsupported fields. That's a
single hardcoded translation pair, not a plugin system — we can't
"add a transformer" without writing Go and redeploying. Conversely,
our `translate` package is much more rigorous about streaming SSE
correctness than CCR's per-transformer ad-hoc patching.

**4. Provider breadth out of the box.** CCR's docs walk users through
configuring OpenRouter, DeepSeek, Ollama, Gemini (direct + Vertex),
Volcengine, ModelScope, Dashscope, AIHubmix, SiliconFlow, Groq,
Cerebras, and "anything OpenAI-compatible." Each provider is just an
entry in `Providers[]` — `name` + `api_base_url` + `api_key` +
`models[]` + `transformer`.

We have **three native providers** (`anthropic`, `openai`, `google`)
hardcoded in `cmd/router/main.go`. Adding a new provider is a new Go
package implementing `providers.Client`, plus a registry entry, plus a
deploy. CCR's provider list is data; ours is code.

**5. Project- and session-specific router config.** CCR can layer
`~/.claude-code-router/<project-path>/config.json` and
`~/.claude-code-router/<project-path>/<session-id>.json` on top of the
global `Router` block. So "use Sonnet for repo A, Haiku for repo B,
and a specific scratch model for *this* session" is a config-only
change. We have no per-installation routing config at all — per-org
preferences are listed under "Roadmap (Deferred)" in our README.

**6. `webSearch` and `image` scenario routing.** CCR detects web-search
tool calls and routes them to a search-capable model
(`gemini-2.5-flash` etc.). It also has a built-in **image agent**
(`packages/server/src/agents/image.agent.ts`) that handles
image-related tool calls, with `forceUseImageAgent` to send images
through the agent even when the underlying model can't tool-call. We
have neither — the cluster scorer doesn't introspect tool definitions
at all and there's no image-mode dispatch.

**7. `/model` slash-command integration with Claude Code.** Because
the proxy lives at `127.0.0.1:3456` and CCR ships an `activate`
command that sets `ANTHROPIC_BASE_URL`, the `/model` command inside
Claude Code is wired to flip the routing scenario in real time. We
expose nothing inside the CC client; routing happens silently and the
only way to inspect is a debug header (`[router-model: <m>]`) or
OTLP spans.

**8. GitHub Actions integration.** CCR documents and provides a
`NON_INTERACTIVE_MODE` plus a known-good workflow YAML for running
`anthropics/claude-code-action@beta` against a CCR-routed endpoint
inside CI. We don't ship anything CI-shaped — our deployment is "a
container behind a TLS endpoint" and the eval harness is Modal-only.

**9. `longContextThreshold` is configurable per deployment.** CCR
exposes `longContextThreshold` as user config (60k default). Our
heuristic threshold is a Go constant in
`internal/router/heuristic/`; changing it requires a release.

### What we have that CCR doesn't

**1. Learned routing.** Our P0 router is a **cluster scorer derived
from AvengersPro** (arxiv 2508.12631):

- in-process Jina v2 INT8 ONNX embedder (`embedder_onnx.go`,
  loaded from `/opt/router/assets/model.onnx`)
- K-means centroids over a benchmark corpus
  (OpenRouterBench-derived) → cluster assignment
- α-blended cost/quality ranking matrix per cluster
  (`rankings.json`)
- argmax over `available_providers ∩ deployed_models` at request
  time

CCR routes on **regex/threshold rules over the request envelope**
(token count, tool name pattern, model-name substring). Their
"intelligence" is a JS function the user writes; ours is a learned
model with a training pipeline (`scripts/train_cluster_router.py`)
and a frozen-version artifact format (`artifacts/v0.1/`,
`artifacts/v0.2/`, `artifacts/v0.3/`, `artifacts/latest`).

This is the headline difference. CCR is rule-based; we're
embedding-based.

**2. Versioned routing artifacts + per-request version pinning.**
`internal/router/cluster/Multiversion` instantiates one `Scorer` per
committed bundle and dispatches per-request via a trusted
`x-weave-cluster-version` header. So a single staging deployment can
serve `v0.1`, `v0.2`, `v0.3` simultaneously and an A/B harness can
compare them on identical prompts. CCR has no concept of routing
versions — there is one `router.ts` function per release.

**3. Eval-routing override switch.** `internal/router/evalswitch`
wraps a primary + fallback router and flips per-request based on a
DB-allowlisted installation flag (`is_eval_allowlisted`) reading the
`x-weave-disable-cluster` header. This is the gate that lets us run
the cluster scorer against the heuristic on the same staging binary
without touching customer traffic. CCR has no equivalent — the
custom-router escape hatch covers individual experimentation but not
A/B comparison.

**4. Multi-tenant auth.** Bearer keys (`rk_*`) are hashed and stored
in Postgres (`model_router_installations`, `model_router_api_keys`)
with an in-process LRU cache. Every routed request is attributable
to an installation. CCR is single-user — its `APIKEY` config field
is one shared secret for the entire local proxy.

**5. Cost-aware routing.** Cost values
(`DEFAULT_COST_PER_1K_INPUT`) are baked into the rankings at training
time, so the argmax is genuinely cost/quality-blended rather than
quality-only. CCR's routing has zero cost awareness — it picks by
scenario type, full stop.

**6. OTLP telemetry.** Every proxied request emits two spans
(`router.decision`, `router.upstream`) with the full set of routing
attributes — requested vs decided model, estimated and actual token
counts, requested vs actual cost in USD, latency split between route
and upstream, cross-format flag, upstream status. Async, batched,
worker-pool-drained. CCR has two pino log files
(`ccr-*.log` and `claude-code-router.log`) and that's it.

**7. Sticky decisions.** `ROUTER_STICKY_DECISION_TTL_MS` reuses a
routing decision for the same API key over a short TTL — useful for
keeping a multi-turn conversation pinned to one model without the
embedder rerunning. CCR has no concept of decision stickiness.

**8. Deterministic fallback chain.** Cluster scorer fails → heuristic
takes over (token-threshold short/long). Heuristic always succeeds
because it's pure logic on a single registered provider. No
embedding, no DB, no I/O. CCR has no fallback path — if the user's
single configured provider dies, the request errors.

**9. SSE translator decorator.** `internal/translate/SSETranslator`
streams chunk-by-chunk and converts wire formats live, never
buffering the whole response. CCR's transformers buffer or stream
ad-hoc per transformer (`enhancetool` explicitly drops streaming).

---

## Part 2 — Product, UI, packaging

### What CCR has that we don't

**1. A web UI.** `ccr ui` opens a React/Vite app served from the
proxy. Top-level pages (from
`packages/ui/src/components/`):

- **Providers / ProviderList** — add/edit providers, paste API
  keys, pick models
- **Router** — drag-and-drop set the model used for `default`,
  `background`, `think`, `longContext`, `webSearch`, `image`
- **Transformers / TransformerList** — pick built-in transformers,
  point at custom ones
- **Presets** — browse marketplace, install, export, dynamic config
  forms
- **Settings dialog** — APIKEY, host, log level, proxy URL, timeout
- **StatusLineConfigDialog** + import/export — Beta status-line
  shown inside Claude Code, configured visually
- **DebugPage** — request history drawer + log viewer
  (`LogViewer.tsx`, `RequestHistoryDrawer.tsx`)
- **Login / ProtectedRoute / PublicRoute** — auth scaffolding for
  the UI itself
- **JsonEditor** — Monaco editor for raw config edits

Stack: React 19, Radix UI primitives, Tailwind v4, i18next (EN/ZH),
Monaco editor, lucide icons, cmdk command palette.

We have **zero UI**. There is no admin surface, no config browser,
no log viewer, no model picker — admin tasks are direct SQL into
`model_router_installations` (this is called out as deferred work
in our README's roadmap).

**2. A CLI.** `ccr` ships a real interactive command tool
(`packages/cli/src/cli.ts`):

```
ccr start | stop | restart | status
ccr code "prompt"           # spawn claude wired to the proxy
ccr model                   # interactive TUI: pick / add / configure
ccr preset export|install|list|info|delete
ccr install <preset>        # install from GitHub marketplace
ccr activate                # eval "$(ccr activate)" sets env vars
ccr ui                      # open web UI in browser
ccr statusline              # the in-Claude-Code statusline
ccr env                     # print the env block
```

`ccr model` is an interactive selector that walks the user through
adding a provider, configuring transformers, and choosing scenario
models — inputs validated as they're typed.

We have **a `Makefile`** (`make dev`, `make test`, `make generate`,
`make migrate-up`, etc.) and `docker compose up`. There is no
command-line surface meant for humans configuring routes.

**3. Presets / marketplace.** CCR has a first-class preset concept:

- **Export** your config as a preset directory with a `manifest.json`
  (sensitive fields auto-replaced with `{{field}}` placeholders)
- **Install** from a local path or from a GitHub marketplace repo
- **Dynamic input schemas** — preset can declare required inputs
  (e.g. "your DeepSeek API key") and the UI/CLI prompt for them at
  install time
- **Versioning** built into the manifest

This is the closest thing CCR has to "deployment" — share a preset,
someone else installs it, they fill in their keys, done. We have
nothing analogous; the unit of deployment is "the router container,
operated by us, with installations seeded via SQL."

**4. Statusline integration.** CCR ships an in-Claude-Code statusline
(v1.0.40+) showing live routing state — current scenario, current
model, recent latency. Configured via `ccr statusline` and the UI's
`StatusLineConfigDialog`. We have nothing in the user's terminal —
the only feedback is the optional `[router-model: <m>]` prefix on
responses (`ROUTER_DEBUG_TAG_RESPONSES`).

**5. Documentation site.** `docs/` is a full Docusaurus site with
EN/ZH i18n, sections for CLI / server / presets, blog posts, and a
public deployment. Our docs are five Markdown files in the repo
(`README.md`, `AGENTS.md` / `CLAUDE.md`, `CLUSTER_ROUTING_PLAN.md`,
`EVAL_RESULTS.md`, `FUTURE_RESEARCH.md`, `ROUTER_V1_PLAN.md`,
`scripts/README.md`) — comprehensive but engineer-facing.

**6. Distribution model.** `npm install -g @musistudio/claude-code-router`
is the entire install. Cross-platform (macOS/Linux/Windows), no
Postgres, no Docker, no API keys to provision — runs against
whatever providers the user already pays for. We ship a Docker
image + a Postgres schema, intended to run as managed
infrastructure. That's a fundamentally different product shape:
they're a developer tool, we're a service.

**7. Environment-variable interpolation in config.** `"$OPENAI_API_KEY"`
or `"${OPENAI_API_KEY}"` anywhere in `config.json` is expanded from
the process environment, recursively through nested objects and
arrays. Lets users keep secrets in their shell and check the
config file into version control. We don't have a config file at
all — we're 12-factor env-vars-only — but our env-var surface is
small and not nested.

**8. Sponsor / community-facing presence.** Discord badge, sponsor
list, ko-fi/PayPal, sponsor banner (Z.ai GLM coding plan, AIHubmix,
BurnCloud, 302.AI, etc.). MIT license. Z.ai is the named project
sponsor. We're a closed component of a commercial product — no
community surface, no MIT license at this layer.

**9. `activate` command for shell integration.** `eval "$(ccr activate)"`
sets `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL`,
`DISABLE_TELEMETRY`, `DISABLE_COST_WARNINGS`, etc. so the bare
`claude` binary (or any Anthropic Agent SDK app) routes through CCR
without `ccr code` wrapping. Our deployment expects the client to
already know to point at the router URL with a bearer key — there's
no equivalent shell shim.

**10. Token counting and tokenizer registry.** CCR has a
`TokenizerService` keyed by `(provider, model)` that picks the right
tokenizer per upstream (Anthropic's, Gemini's, OpenAI's tiktoken,
etc.) for `/v1/messages/count_tokens`. We forward
`/v1/messages/count_tokens` straight to Anthropic as a passthrough.

### What we have that CCR doesn't (product side)

**1. Multi-tenancy + installation lifecycle.** Postgres-backed
`installations` and `api_keys`, soft-delete via `deleted_at`,
LRU-cached key verification (`auth.Service.VerifyAPIKey` +
`APIKeyCache`), opaque external `organization_id` / `created_by`.
CCR doesn't model "users" at all — there's one local instance per
user and one shared `APIKEY`.

**2. Production deployability.** Cloud Run-ready container
(`Dockerfile`, `docker-compose.yml`), Cloud SQL Auth Proxy support
(`POSTGRES_CONNECTION_NAME`), forced `127.0.0.1` host binding when
no APIKEY is set as a security guardrail (CCR has the same guard
actually), structured slog logging with snake_case keys, OTLP
export to any compatible collector. CCR is explicitly localhost
software.

**3. SQLC-generated DB layer + golang-migrate migrations.**
Schema-only SQLC mode (`db/sqlc.yml` parses migration files
directly), `make migrate-create NAME=...` scaffolding, named
parameters with type casts, every query commented. CCR has no
persistent state at all — config is one JSON file.

**4. CLEAN architecture and import-rule enforcement.** Concentric
inner/adapter/presentation rings, documented in `AGENTS.md` /
`CLAUDE.md`, with explicit "never do this" lists. CCR is a
flatter monorepo (`cli` / `core` / `server` / `shared` / `ui`)
without comparable layering rules.

**5. Eval harness.** `eval/` is a Modal-driven Python package that
runs benchmark loaders (BFCL v4, SWE-bench, Aider Polyglot, …),
ensemble LLM judges (Gemini, GPT-5), Pareto scoring against the
router. The output flows back into the cluster artifacts. CCR has
no eval infrastructure — its quality story is "users tune their
own rules until requests look right."

**6. Build-tag layered embedder.** `embedder_onnx.go` /
`embedder_stub.go` gated by `no_onnx`, plus `-tags ORT` for hugot's
ONNX runtime, plus `-tags onnx_integration` for the parity
integration test. Contributors without `libonnxruntime` installed
can still `go test` locally. CCR ships pure JS — there's no
analogous CGO complexity to abstract.

---

## TL;DR positioning

| | CCR | router/ |
|---|---|---|
| **Distribution** | `npm i -g`, runs on `127.0.0.1` | Docker container + Postgres, runs as a service |
| **Tenancy** | Single user, shared APIKEY | Multi-tenant, per-installation bearer keys |
| **Routing logic** | Rule-based (token threshold, tool-name regex, model-name substring) | Learned (Jina embeddings → K-means cluster → α-blended cost/quality argmax) |
| **Provider breadth** | ~15+ via config, plus anything OpenAI-compatible | 3 hardcoded (Anthropic / OpenAI / Google) |
| **Wire-format adapters** | Plugin system (~22 transformers + user-loadable) | One Go translator (OpenAI ↔ Anthropic) |
| **Per-request override** | `/model` in CC, `<CCR-SUBAGENT-MODEL>` sentinel, custom JS router | Allowlisted `x-weave-*` headers (eval only) |
| **UI** | Full React app: providers, router, transformers, presets, debug, statusline | None |
| **CLI** | `ccr` with start/stop/code/model/preset/install/activate/ui/statusline | `make` targets only |
| **Config** | `~/.claude-code-router/config.json` + per-project + per-session overlays | 12-factor env vars |
| **Telemetry** | Pino log files | OTLP spans + slog |
| **Versioning** | Per-release | Versioned cluster artifacts, per-request pinning |
| **Eval** | None | Modal-driven harness, Pareto scoring |
| **License / community** | MIT, Discord, sponsors | Internal |

The two projects answer different questions. **CCR**: "I'm a developer
who wants Claude Code to talk to DeepSeek/Gemini/Ollama on my laptop
with a UI to configure it." **router/**: "we want a managed service
that learns which model to dispatch each prompt to and exposes that
to multiple tenants." The interesting borrowable ideas going the
other direction are mostly UX surface — a web UI for inspecting
routing decisions, a CLI for issuing keys instead of raw SQL, an
in-Claude-Code statusline for routing visibility, a transformer
plugin system if/when our provider list grows past three.
