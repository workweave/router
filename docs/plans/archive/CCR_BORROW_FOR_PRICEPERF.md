Created: 2026-05-02
Last edited: 2026-05-03

# What to borrow from CCR for price/performance

Goal: maximize quality-per-dollar on real Claude Code traffic.
CCR (`musistudio/claude-code-router`, MIT) is mature, has a wide
provider/transformer ecosystem, and ships several tricks that we
can adapt without abandoning our learned cluster scorer. This doc
ranks borrowable ideas by expected price/perf impact, sketches a
concrete implementation that fits our Go layered architecture
(`internal/router/`, `internal/proxy/`, `internal/translate/`),
and calls out what's *not* worth taking.

The ranking weighs: (1) realistic cost reduction on Claude Code
traffic shape, (2) quality risk, (3) implementation effort given
our CLEAN layering, and (4) whether the idea is additive (wraps the
cluster scorer) or replacing (would force us to abandon learned
routing).

---

## Tier 0 — the model registry is the bottleneck

Before listing CCR-derived ideas, the single biggest price/perf
lever isn't an idea at all — it's that **`v0.5/model_registry.json`
contains only frontier-tier models** (Claude 4.5/4.7, GPT-5/5.5/4.1,
Gemini 3.1 Pro/Flash). Every routing decision picks between
"expensive" and "slightly less expensive." There is no cheap tier
the scorer can actually argmax onto.

CCR's example configs route their `background` and easy-default
buckets to **DeepSeek-V3** (~$0.27/M input), **Qwen3-Coder via
Groq/Cerebras** ($0.10–0.79/M), **Llama 3.3 70B via Groq** ($0.05/M),
or local **Ollama** ($0). That's 5–100× cheaper than Haiku, and on
the easy long tail of CC traffic (file reads, status updates,
conversation summarizers) the quality delta is small enough to be
invisible to most users.

**Takeaway:** none of the CCR borrows below matter if the cluster
scorer can only choose between four flavors of frontier model. The
work order should be:

1. Add 2–3 cheap-tier providers/models to `model_registry.json` and
   retrain (`v0.6`).
2. *Then* add the pre-cluster fast-path layer (T1 below) to bias
   easy traffic onto them harder than the α-blend already does.

The rest of the doc assumes (1) is in flight or already done.

---

## Tier 1 — high impact, fits our architecture

### T1.1 Pre-cluster signal-based "fast path" router

**What CCR does** (`packages/core/src/utils/router.ts`):

```ts
if (tokenCount > longContextThreshold && Router.longContext)
  return Router.longContext;
if (model.includes("claude") && model.includes("haiku") && Router.background)
  return Router.background;
if (tools?.some(t => t.type?.startsWith("web_search")) && Router.webSearch)
  return Router.webSearch;
if (req.body.thinking && Router.think)
  return Router.think;
return Router.default;            // <-- their "default" is our cluster scorer
```

Four cheap deterministic checks short-circuit the router on the
high-confidence cases before the embedder ever runs. CCR then falls
through to whatever's configured in `default` (a static model for
them; the cluster scorer for us).

**Why it's a price win for us specifically:**

- **`haiku` short-circuit.** Claude Code itself fires *enormous*
  numbers of background calls tagged `claude-3-5-haiku-*` /
  `claude-haiku-4-5` for things CC pre-classified as cheap:
  conversation summarizers, file titlers, "is this question worth
  thinking about" pre-checks. Anthropic's own client already
  decided these are small-model territory. Today our cluster scorer
  embeds them, runs argmax, and lands on whatever the scorer thinks
  is best — sometimes Sonnet, sometimes Haiku. Routing them
  unconditionally to the cheapest registered small-tier model
  (Gemini 3 Flash Lite Preview today, DeepSeek-chat once we add it)
  is **pure margin**. We trust CC's prior.
- **`thinking` field short-circuit.** When the client opts into
  `thinking: {type: "enabled", budget_tokens: N}`, the user is
  paying for reasoning. The cluster scorer doesn't read this field
  and could still pick a non-reasoning model, then we forward
  `thinking` through, the upstream may either ignore it or error.
  A short-circuit to a designated reasoning model
  (`gpt-5`, `claude-opus-4-7`, eventually DeepSeek-R1) is both a
  quality win and a cost-correctness win (no wasted reasoning
  tokens on a non-reasoning model).
- **Token-threshold long-context short-circuit.** Pricing has
  step-function cliffs at context windows: Gemini 1M-context tier
  is 2× the 200k-context tier; Anthropic 1M beta is 2× the 200k.
  The α-blend in our rankings matrix is *linear* in cost — it
  smooths over the cliff. A hard threshold pre-empts: "above 200k
  est. tokens, jump straight to a model that prices well at 1M."
  CCR's `longContextThreshold` defaults to 60k.
- **Web-search tool short-circuit.** If `tools[]` contains a
  `web_search` entry, only models that natively grounding-search
  will return useful answers. Today our scorer doesn't read tool
  shapes; it would happily route to a model that ignores the
  search tool, costing the request entirely.

**Implementation in our architecture:**

New adapter `internal/router/fastpath/` implementing
`router.Router`:

```go
type Rules struct {
    primary           router.Router  // the cluster Multiversion
    haikuTarget       *router.Decision
    thinkTarget       *router.Decision
    webSearchTarget   *router.Decision
    longContextTarget *router.Decision
    longContextTokens int
}

func (r *Rules) Route(ctx context.Context, req router.Request) (router.Decision, error) {
    if req.RequestedModelHasHaiku && r.haikuTarget != nil {
        return *r.haikuTarget, nil
    }
    if req.ThinkingEnabled && r.thinkTarget != nil {
        return *r.thinkTarget, nil
    }
    if req.HasWebSearchTool && r.webSearchTarget != nil {
        return *r.webSearchTarget, nil
    }
    if req.EstimatedInputTokens > r.longContextTokens && r.longContextTarget != nil {
        return *r.longContextTarget, nil
    }
    return r.primary.Route(ctx, req)
}
```

Wiring (in `cmd/router/main.go`): `evalswitch.New(fastpath.New(cluster, ...), heuristic)` —
the fastpath layer wraps the cluster Multiversion *inside* the
evalswitch, so eval allowlist behavior is unchanged.

`router.Request` already carries `EstimatedInputTokens`. We need to
add `ThinkingEnabled bool`, `HasWebSearchTool bool`, and
`RequestedModelHasHaiku bool` (or generalize: `RequestedModelTier`)
populated in `proxy.Service.ProxyMessages` from
`translate.AnthropicEnvelope`. Pure data, no I/O — fits the inner
ring rule.

Targets are env-configured: `ROUTER_FASTPATH_HAIKU_TARGET=anthropic,claude-haiku-4-5`,
etc. Empty = layer disabled for that signal.

**Risk:** the fastpath bypasses the embedder so misclassification
becomes deterministic rather than averaged. Mitigations:

- Only enable signals that have very high precision (CC tagging a
  call as haiku is unambiguous; `thinking` enabled is unambiguous;
  long-context is a tail-condition where the alternative is
  guaranteed-bad anyway).
- Log shadow decisions: even when the fastpath fires, run the
  cluster scorer and log what it would have picked. If the cluster
  scorer disagrees materially over time, we have data for whether
  the prior was actually right.
- Per-installation kill-switch (`fastpath_disabled` column) for
  customers who want pure-cluster behavior.

**Effort:** small (~1 day). The hardest part is the eval to prove
it doesn't degrade quality.

---

### T1.2 Subagent / sentinel-based model declaration

**What CCR does:** subagent prompts can pin their model with
`<CCR-SUBAGENT-MODEL>provider,model</CCR-SUBAGENT-MODEL>` at the
start of the prompt. The router strips the sentinel and routes
exactly as instructed.

**Why it's a price win:** Claude Code's subagent feature spawns
sub-tasks — often *narrow* sub-tasks (one tool category, one focused
question). Subagents typically don't need frontier capability;
they need to read three files and answer one question. Today every
subagent call hits the cluster scorer at full price. If the agent
SDK / Claude Code itself learns to declare "I'm a doc-summarizer
subagent, route me cheap," the price savings on heavy agentic
workflows is large (20–60% of total spend on agentic users
empirically — subagent traffic dominates token volume on Claude
Code Plan tier).

**Implementation:** support both forms:

- Header: `x-weave-subagent-tier: cheap|reasoning|frontier` (we own
  the header, we trust it because the *outer* call is bearer-authed
  and the agent runs inside that auth scope). Maps to
  configurable target decisions.
- Optional sentinel parsing in `translate.ParseAnthropic`:
  `<WEAVE-SUBAGENT-TIER>cheap</WEAVE-SUBAGENT-TIER>` (or reuse
  CCR's exact tag for compatibility — it's MIT, this is a
  protocol-shaped feature, not borrowed code).

This is a tier abstraction, not a model name. Subagents declare
intent ("cheap"); the deployment maps the tier to a concrete model
in registry. Decoupled the way they *aren't* in CCR — gives us
control over what "cheap" means without subagent prompts knowing
our provider list.

**Risk:** the surface is opt-in; no quality risk for non-adopters.
The only real risk is leaking the tier vocabulary into customer
prompts and then having to support it forever. Mitigation:
namespace it (`x-weave-*`), document explicitly that the tier
mapping is a deployment-time decision, not a stable contract.

**Effort:** small (~half a day) once the fastpath layer (T1.1)
exists, since both share the "decision override before scorer"
machinery.

---

### T1.3 Tool-result-aware embed input (we already have this; make it default-on)

This isn't borrowed from CCR — they don't have it — but it pairs
with the fastpath logic and is the single lever already in our code
that's underused. `ROUTER_EMBED_LAST_USER_MESSAGE=true` flips the
embedder input from the concatenated message stream (which inside
agentic loops is dominated by `tool_result` content and fingerprints
"Claude Code session" instead of the real prompt) to the most
recent user-authored text.

**The price relevance:** without this flag, agentic-loop interior
turns all embed to the same neighborhood and route to the same
model. That's stable, but it means the cluster scorer can't
distinguish "user just typed a one-line follow-up" (Haiku-worthy)
from "user just typed the original 2-page spec" (Opus-worthy). With
the flag on, the scorer sees the user-authored text and the price
distribution of routed models tracks the true prompt difficulty.

**Recommendation:** make `ROUTER_EMBED_LAST_USER_MESSAGE=true` the
default in production after the next eval pass clears it on
non-agentic traffic too. This has zero CCR connection but sits
naturally next to the fastpath work.

---

### T1.4 Provider expansion via the transformer pattern

**What CCR has:** ~22 transformers
(`packages/core/src/transformer/`) plus user-loadable plugins.
Each transformer is a tiny request/response shaper for one
provider's quirks (e.g. `deepseek` strips Anthropic-only fields,
`enhancetool` patches malformed tool-call JSON, `maxtoken` caps
`max_tokens` to a per-provider limit). Composable per-provider and
per-model.

**Why it's a price win:** the transformer pattern is what lets
CCR's user base hop onto any cheap OpenAI-compatible provider that
ships next week (`Groq`, `Cerebras`, `DeepSeek`, `Together`,
`Volcengine`, `ModelScope`, `Dashscope`, `AIHubmix`, etc.) without
upstream code changes. Our `internal/translate` is a single
hardcoded OpenAI ↔ Anthropic pair. Adding DeepSeek-V3 today means
writing a Go provider package, handling its specific quirks
(temperature clamping, tool-call argument repair, `max_tokens`
limits) inline in the adapter, and shipping a release. Friction
that we don't actually want once we're trying to chase the cheap
tier.

**Concrete win cases:**

- **`maxtoken` equivalent.** When we route a CC request to an
  upstream that caps output tokens lower than CC asked for, the
  request fails. The retry costs us. A small per-(provider, model)
  `MaxOutputTokens` clamp in `proxy.Service` (or in the provider
  adapter via a registry) is two hours of work and prevents a
  whole class of failures.
- **`enhancetool` equivalent.** Cheap models are great until they
  emit invalid JSON in `tool_calls.function.arguments`. The
  enhancetool transformer parses defensively and repairs common
  patterns (unquoted keys, trailing commas, code-fenced JSON). For
  us this is the gate that makes DeepSeek/Qwen tool-call workloads
  viable. Without it, we can't include tool-heavy clusters in the
  cheap-tier scoring honestly.
- **`cleancache` equivalent.** Strips Anthropic `cache_control`
  fields when targeting providers that don't support them and
  would error or charge differently. We do some of this in
  `AnthropicToOpenAIRequest`; making it a composable layer means
  adding a fifth provider doesn't require auditing the translator
  again.
- **`reasoning` equivalent.** Maps Anthropic-style
  `thinking.budget_tokens` to provider-specific reasoning toggles
  (`enable_thinking` for DeepSeek, `reasoning_effort` for OpenAI).

**Implementation:** I do *not* think we should ship a plugin system
(loadable JS files at runtime — CCR's surface). The win is the
composable in-process pipeline. Adapt the pattern to Go:

```go
// internal/translate/transform/transform.go
type RequestTransform interface {
    TransformRequest(*providers.Request) error
}
type ResponseTransform interface {
    TransformResponse(*providers.Response) error
}

// internal/providers/openai/client.go
client := &Client{
    transforms: []RequestTransform{
        cleancache.New(),
        maxtoken.New(8192),
        enhancetool.New(),  // applied to response side
    },
}
```

Per-provider transform stack, configured at composition root, no
runtime loading. Keeps the inner-ring purity (transforms are
I/O-free pure functions over `providers.Request` / `providers.Response`)
while making provider expansion a sub-day task instead of a
multi-day release.

**Effort:** medium (~3 days) for the framework + the four
transforms above. The framework dominates; each transform is
~50 LoC.

---

## Tier 2 — medium impact

### T2.1 Per-installation routing policy

**What CCR has:** project- and session-scoped config overlays. The
runtime picks the most specific config that exists. For us this
maps to: per-`installation` rules in the DB.

**Why it's a price win:** different customers have different
quality bars. A research team will pay for frontier on every
prompt; a CI pipeline can run on cheap-tier across the board with
zero quality complaints. Today the cluster scorer treats them
identically. A column on `model_router_installations`:

```sql
ALTER TABLE model_router_installations
  ADD COLUMN routing_policy_overrides JSONB
  DEFAULT '{}'::jsonb NOT NULL;
```

with optional fields like `cost_ceiling_per_1m_input_usd`,
`forbidden_providers[]`, `forced_tier`, `cluster_version_pin`.
The scorer reads these from request context (auth middleware
already loads the installation) and biases / clamps before argmax.

**Effort:** medium (~2 days). The SQL is trivial; the work is
deciding the schema and threading installation policy through the
scorer cleanly without violating the inner-ring rule (handle by
attaching policy to `router.Request`, scorer reads it pure-functionally).

### T2.2 Tokenizer registry per (provider, model)

**What CCR has:** `TokenizerService` in
`packages/core/src/services/tokenizer.ts` with per-model tokenizer
config; falls back to tiktoken (`cl100k_base`).

**Why it's a price win:** our threshold decisions
(`longContextThreshold`, the soon-to-be-fastpath threshold from
T1.1) are only as good as our token estimate. Today we estimate;
we either over-trigger long-context routing (overpaying) or
under-trigger (cheap-routing prompts that should go to 1M-context
tier and then truncate). A 5–10% improvement on threshold accuracy
translates to a 5–10% direct cost shift on near-threshold traffic.

**Implementation:** small Go map of model → tokenizer choice.
Native libraries: `github.com/tiktoken-go/tokenizer` for
OpenAI/cl100k families, `github.com/sugarme/tokenizer` (or call
HuggingFace tokenizers via FFI like we already do for ONNX) for
sentencepiece-family models. Estimate cost: <100 µs per request
for the common case.

**Effort:** medium (~2 days). Library integration is the time
sink, not the code.

### T2.3 Session-keyed decision stickiness with usage history

**What CCR does:** `sessionUsageCache` records the previous
turn's `input_tokens` per session, and uses that to bias
long-context routing on the *next* turn even if the next turn's
prompt looks short on its own.

**Why it's a price win:** in real CC sessions, the conversation
context grows monotonically. Once you're past 80k input tokens,
every subsequent turn is also long-context, regardless of what the
user just typed. Our existing `ROUTER_STICKY_DECISION_TTL_MS`
sticks the decision per-API-key for a short TTL — but it sticks
*the model*, not the *signal*. Augmenting with "if the last turn
in this session had >threshold input tokens, treat the next turn
as long-context regardless of its own size" is a tighter,
session-aware version of the same thing.

The current sticky cache is keyed on `api_key_id`; CCR's is keyed
on `sessionId` parsed from `req.body.metadata.user_id`. Anthropic
emits `metadata.user_id` in CC requests; we already see it. Add a
per-session `sessionUsage` cache (LRU, short TTL) and feed
`lastInputTokens` into the fastpath threshold check as
`max(currentEst, lastObserved)`.

**Effort:** small (~half a day) given the existing sticky cache
infrastructure.

### T2.4 Configurable `longContextThreshold` env var

Trivial wrapper over T1.1: surface
`ROUTER_FASTPATH_LONG_CONTEXT_THRESHOLD_TOKENS` so deployments tune
the cliff per provider mix. Costs nothing extra; ship with T1.1.

---

## Tier 3 — explicitly NOT borrowing

### T3.1 Custom router script (`CUSTOM_ROUTER_PATH`)

CCR ships a JS-loading escape hatch:
`module.exports = async (req, config) => "provider,model" | null`.
It's their answer to "I want non-default routing logic without a
fork." For us this is the wrong primitive:

- Embedding a JS engine (V8/QuickJS) in a Go server is heavy.
- Lua/Starlark is lighter but customer-facing scripting is a
  support burden we don't want.
- Our learned scorer is the differentiator. A scripting hook
  encourages customers to *replace* it with hand-tuned rules,
  defeating the point.

The need behind the feature (per-tenant override) is real — that's
T2.1 (DB-backed policy), not arbitrary code execution.

### T3.2 Web UI for config

CCR's UI is the right product surface for a single-user local
proxy. For us the equivalent is admin tooling for issuing API keys,
inspecting routing decisions, and tuning policy — that's a Weave
app feature, not a router-package feature. Routing visibility
inside the existing Weave dashboard is the right place; standing
up a separate React app inside `router/` would diverge from our
deployment and auth model.

### T3.3 Image agent

We don't currently route multimodal traffic; the registry has no
image-capable entries. Defer until the registry includes a vision
model.

### T3.4 Preset / marketplace

The "preset" concept is for sharing a personal config. Our
equivalent — versioned cluster artifacts — already covers
"deployable, frozen routing strategy." A marketplace adds a
distribution problem we don't have.

### T3.5 Statusline integration

Could borrow it as a header-driven feature (we already have
`ROUTER_DEBUG_TAG_RESPONSES`), but it's UX polish, not
price/perf. Defer.

---

## Recommended sequencing

```
v0.6 cluster artifacts
└── adds cheap-tier deployed_models (DeepSeek-V3 + Groq Llama or Qwen)
    + retraining pass against existing benches

PR-A  internal/router/fastpath/  (T1.1)
      + RequestedModelHasHaiku / ThinkingEnabled / HasWebSearchTool
        plumbed through router.Request from translate envelope
      + shadow-decision logging behind a flag

PR-B  internal/translate/transform/  (T1.4)
      + maxtoken + enhancetool + cleancache transforms
      + per-provider transform stack wired in cmd/router/main.go

PR-C  default-on ROUTER_EMBED_LAST_USER_MESSAGE  (T1.3)
      + eval pass to confirm no regression on non-agentic traffic

PR-D  per-installation policy column  (T2.1)
      + tokenizer registry  (T2.2)
      + session-usage stickiness  (T2.3)

PR-E  subagent tier sentinel/header  (T1.2)
      (ships last because it depends on the fastpath override path
       that PR-A puts in place)
```

The ordering puts the registry work first because nothing else
matters until there's a cheap tier the scorer can actually pick.
PR-A is the highest-leverage code change, PR-B unlocks honest
evaluation of cheap tiers, and PR-C–E are quality-of-life wins on
the routing signal.

## What we're explicitly NOT taking

- The JS plugin runtime (`CUSTOM_ROUTER_PATH`, custom transformers
  loaded at runtime).
- The web UI.
- The preset/marketplace concept.
- The localhost-single-user product shape.
- MIT licensing of `router/` itself (adopting their patterns ≠
  forking their code; we should keep the integration to
  borrowed-design level, not borrowed-source level, to keep license
  options open).
