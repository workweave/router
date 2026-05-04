Created: 2026-05-02
Last edited: 2026-05-03

# Future research — beyond AvengersPro for coding agents

> **Status.** Speculative research direction. Not a P0/P1 commitment. Read
> this *after* `CLUSTER_ROUTING_PLAN.md` lands the AvengersPro baseline
> and the eval harness produces a first set of numbers.
>
> **Living document.** Revised twice — once after a competitive-
> landscape audit (April 2026), then again after a deep-research
> audit (April 30, 2026, see "Provenance" at the end). The first
> revision moved the headline from "step-typed routing" to
> **cache-aware switching cost**. The second revision narrowed that
> headline once a direct prior-art counterexample (arxiv 2508.11291,
> wireless edge-device KV-cache-aware routing) was identified that
> the first revision had missed.
>
> The framing assumption: our deployment target is **coding agents**
> (multi-turn, tool-using, KV-cache-heavy), not single-shot chat.
> Routing-for-coding-agents is no longer first-mover territory — it
> was actively colonized in Q1 2026 — but cache-aware *routing
> objectives* remain genuinely open.

## TL;DR

A four-layer system for coding-agent routing. Headline contribution is
**Layer 3 (cache-aware switching cost under API-priced prompt caching)**.
The unqualified "first to model KV-cache invalidation cost in a routing
objective" claim does not survive — arxiv 2508.11291 (wireless edge-
device routing, Aug 2025) said it first. The narrowed contribution that
*does* survive: integrating **API-provider-priced** prompt-cache
invalidation cost (Anthropic-style 1.25× / 2.0× write multipliers and
the 0.10× cached-read pricing) into the routing-time argmax across
**frontier-tier API models**, with **TTL-tier choice (5-min vs 1-hour)
as a router decision variable**, validated on agentic coding benchmarks.
Layers 1, 2, 4 are engineering components in service of that headline,
each with partial precedent.

1. **Feature-augmented cluster routing (Layer 1).** Extend the
   AvengersPro embedding feature with deterministic agent-step
   signals (last-tool-result type, turn index, prefix length, system-
   prompt fingerprint). Step-level routing has precedent in TRIM
   (arxiv 2601.10245) and Budget-Aware Agentic Routing (arxiv
   2602.21227); the marginal contribution here is the *coding-agent
   feature set* and how those features compose with cluster routing,
   not the abstract idea of step-level granularity.
2. **Confidence-gated speculative escalation (Layer 2).** Let the
   cheap model (Haiku) emit the first ~32 tokens; gate commit /
   escalate on a **structural-validity oracle** (tool-schema partial
   parse, unified-diff prefix structure, tree-sitter incremental
   parse) backed by token confidence as a secondary signal. Direct
   precedent: MCCom (arxiv 2603.05974) does local-cloud cascading
   for line-level code completion; the confidence-vs-entropy finding
   we cite from TAPS (arxiv 2603.27027) is **about choosing among
   speculative-decoding drafters** (HASS / EAGLE-2 trained on
   different data), not about model-tier escalation — kept here only
   as an "uncertainty-routing signal" reference, not as a precedent
   for our actual mechanism. The novel piece is the **structural
   oracle for code/tool-calls** — high-precision failure detector,
   low-recall pass detector — and the asymmetric routing policy that
   exploits that asymmetry. Conformal Alignment (arxiv 2510.17543)
   offers a stronger theoretical frame: oracle-pass with bounded
   coverage guarantees instead of point estimates.
3. **Cache-aware switching cost under API pricing (Layer 3 —
   headline).** Add a
   `γ · prefix_tokens_invalidated · cache_write_multiplier(m, ttl)`
   penalty to the routing argmax, instantiated against **Anthropic-
   style prompt-cache pricing** (1.25× write for 5-min TTL, 2.0×
   write for 1-hour TTL, ~0.10× read). Make **TTL-tier choice
   (5-min vs 1-hour) a router decision variable** — verified novel.
   Nearest published prior art: arxiv 2508.11291 ("Dynamic Quality-
   Latency Aware Routing for LLM Inference in Wireless Edge-Device
   Networks", Aug 2025) — quantifies KV-cache *recomputation* cost
   when switching between an on-device small model and an edge-
   server large model, evaluated on MMLU / GSM8K / MT-Bench-101.
   Differentiators that survive: (a) provider-priced caches
   (dollars from a published price sheet) vs. on-device GPU
   recomputation (latency); (b) TTL tier as a learned decision
   variable; (c) frontier-tier model pool (Haiku / Sonnet / Opus,
   not local + edge); (d) coding-agent benchmarks. Adjacent prior
   work: Continuous Semantic Caching (arxiv 2604.20021) treats
   *response-cache* switching cost in continuous space with
   sublinear regret bounds; GORGO (arxiv 2602.11688) is dispatcher-
   level prefix-cache + network-latency optimization. Cache-aware
   *serving* has precedent (KVFlow 2507.07400, KVCOMM 2510.12872,
   DroidSpeak 2411.02820, Continuum 2511.02230); none operate at
   the model-selection objective.
4. **Decision-aware online training (Layer 4 — engineering, not
   novel).** Replace AvengersPro's α-blended absolute-score
   regression with a Bradley-Terry / dueling-bandit ranker.
   **Already published**: EquiRouter (arxiv 2602.03478) prescribes
   exactly this fix; CCFT (arxiv 2510.00841) is the contextual
   dueling-bandits formulation; Causal LLM Routing (arxiv
   2505.16037) is the regret-minimization frame for observational
   data. We implement and cite — we do not claim novelty here.

The publishable framing, if all four layers ship and validate:

> **Cache-aware LLM routing for tool-using agents under
> prompt-cached API pools.** First routing system to (a) integrate
> **API-provider-priced** prompt-cache invalidation cost into the
> routing objective at the *frontier-tier* model-selection level,
> (b) make **cache-TTL-tier a learned router decision variable**,
> (c) use structural-validity oracles to gate speculative
> escalation in code/tool-calls, and (d) validate on contamination-
> resistant agentic coding benchmarks (SWE-bench Pro, SWE-bench-
> Live, FeatureBench, Terminal-Bench 2.0). Differentiated from
> arxiv 2508.11291 (which models on-device KV-recomputation
> latency for an edge-device pair on chat benchmarks) and from
> OpenRouter's production "sticky routing" (which keeps requests
> on the same provider once cached but does not score model
> alternatives by cache-cost delta).

A name. The obvious one — *AgentRouter / AgentRoute* — is taken
(arxiv 2510.05445, knowledge-graph-guided multi-agent QA, Oct
2025). Working candidates: **CacheRoute**, **PrefixRouter**,
**AgentCascade**, **CacheCascade**. Pick at paper-draft time.

---

## Update — competitive landscape audit (Q1 2026)

The space was open in mid-2025; it has filled in fast. Anything
calling itself a "router for coding agents" without engaging the
following work will be rejected:

- **TRIAGE** (arxiv 2604.07494, Apr 2026) — task-level Haiku /
  Sonnet / Opus tier routing on **SWE-bench Lite**, using code-
  health metrics (cyclomatic complexity, coupling, file size,
  duplication) as the routing signal. Three policies (heuristic
  threshold, ML classifier, oracle). Derives explicit falsifiable
  conditions for when tier-routing pays off. **The most direct
  competitor.** Different signal axis (code-health vs. cache
  cost), so distinct contribution, but reviewers will demand a
  head-to-head on SWE-bench Lite at minimum.
- **Budget-Aware Agentic Routing / BoPO** (arxiv 2602.21227,
  Microsoft, Feb 2026) — boundary-guided SFT + PPO for step-wise
  cheap-vs-expensive routing under per-task budgets. Diagnoses an
  "always-small collapse" failure mode in sparse-reward agent
  rollouts. Authored by the SWE-bench-Live team. Closest published
  approach to step-level agentic routing; we differ by adding
  cache cost and avoiding RL on the routing decision (we use
  ranking loss, they use BoPO).
- **xRouter** (arxiv 2510.08439, Salesforce/UIUC, Oct 2025) —
  RL-trained tool-calling LLM-as-router for cost-aware
  orchestration. Heavy; rules itself out for our P95 ≤ 100ms
  budget but the cost-aware reward design is reusable.
- **AgentRouter (the name conflict)** (arxiv 2510.05445, Oct
  2025) — different domain (KG-guided multi-agent QA, GNN-based)
  but it owns the name. Pick a different one.
- **Inside the Scaffold** (arxiv 2604.03515, Apr 2026) — taxonomy
  of 13 coding-agent scaffolds across 12 dimensions, explicitly
  catalogs "multi-model routing" as one dimension where most
  agents are single-model and a small minority do manual tier
  escalation. Useful as a "the field needs this" citation.

**What this means:** the bar is now "first routing paper to
integrate API-priced prompt-cache invalidation cost and TTL tier
into the model-selection objective on agentic coding benchmarks,
with TRIAGE / BoPO / sticky-routing baselines." The "step-level
routing for coding agents" headline is gone. The
"cross-tier cache-aware routing under API-priced prompt caches"
headline is narrowed but still defensible.

A second-revision audit (April 30, 2026) added two important
prior-art finds the first revision missed:

- **arxiv 2508.11291** "Dynamic Quality-Latency Aware Routing for
  LLM Inference in Wireless Edge-Device Networks" (Bao et al.,
  Aug 2025) — explicitly claims the *first* routing framework to
  quantitatively model KV-cache recomputation overhead during
  model switching. Wireless edge-device setting; on-device + edge
  server pair; MMLU / GSM8K / MT-Bench-101. The unqualified version
  of our headline claim does not survive contact with this paper.
  We must cite it prominently and frame the contribution
  narrowly: API-priced caches (dollars from a published price
  sheet, not on-device GPU recomputation latency) + TTL tier as
  router decision variable + frontier-tier API model pool +
  coding-agent benchmarks.
- **OpenRouter "sticky routing"** — production application-layer
  feature that keeps subsequent requests on the same provider
  once a cache is warm, conditional on `cache_read < base_input`.
  Industry baseline closer in spirit to Layer 3 than any academic
  prior work, although it operates at provider selection for one
  model rather than at model-tier selection. Cite as production
  baseline.
- **HotSwap** (DEV.to, March 2026, application-layer blog post) —
  "routing LLM subtasks by cache economics" using Anthropic's
  5-min TTL: nano model handles read-only exploration while the
  primary's cache stays warm. Not peer-reviewed; useful as
  evidence the framing is not academic-only.

Note on source-AI critique provenance (now twice-confirmed): both
audit rounds asserted that MTRouter (arxiv 2604.23530) does not
exist and is a hallucinated citation. Both claims are **wrong**.
We re-verified via direct arxiv lookup on 2026-04-30: MTRouter
is real, was published 2026-04-26, title "MTRouter: Cost-Aware
Multi-Turn LLM Routing with History-Model Joint Embeddings",
authors Zhang/Li/Wang/Feng/Yang/Wang/Zhang/Bai/Hu, code linked at
`github.com/ZhangYiqun018/MTRouter` in the abstract. The citation
stays. **Lesson learned**: web-search-only critique tools cannot
reliably disprove arxiv IDs newer than their search-index cutoff;
direct arxiv API/PDF verification is the only ground truth. We
mention this twice now because two independent reviewers fell
into the same trap on the same paper. Other points raised by
both reviews — TRIAGE / BoPO scoops, name collision, κ-judge
inadequacy, the 2508.11291 prior art the second reviewer caught —
were valid and are incorporated below.

---

## Where the field is in late 2025 / early 2026

Routing research has split into **seven** roughly-independent
branches. Coding-agent-specific work is now active.

| Direction                          | Representative paper                                                | Mechanism                                                                          | Limitation that matters to us                                                                  |
| ---------------------------------- | ------------------------------------------------------------------- | ---------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| **Cluster + α-blend (us)**         | AvengersPro, arxiv 2508.12631                                       | K-means over query embeddings; per-cluster (model, score) table; static            | Single-shot, offline, no agent loop, no cache awareness                                        |
| **Output-length-as-control**       | R2-Router, arxiv 2602.02823                                         | Joint `(model, length-budget)` decision via "routing-as-reasoning" output-length policy | Single-shot; no coding tasks                                                                   |
| **Cache-aware routing (edge-device)** | Dynamic Quality-Latency Aware Routing, arxiv 2508.11291          | Quantifies KV-cache recomputation overhead when switching small↔large in edge-device pair | On-device GPU cost (not API pricing); MMLU/GSM8K/MT-Bench (not coding); two-model edge pair only |
| **Multi-turn cost-aware**          | MTRouter, arxiv 2604.23530                                          | Joint history-model embeddings; AEC-discounted outcome estimator                   | **No code eval**; KV-cache cost ignored (admitted limitation); offline                         |
| **Step-level / per-step**          | TRIM, arxiv 2601.10245; STEER, arxiv 2511.06190                     | Step uncertainty / pre-step logits gate escalation                                 | Math / multi-hop QA only; no tool-use / code                                                   |
| **Decision-aware (anti-collapse)** | EquiRouter, arxiv 2602.03478; CCFT, arxiv 2510.00841                | Pairwise / ranking loss instead of regression                                      | Single-shot; small model pools                                                                 |
| **Coding-agent (new in Q1 2026)**  | TRIAGE, arxiv 2604.07494; BoPO, arxiv 2602.21227                    | Code-health signals (TRIAGE) or RL with budget-aware boundaries (BoPO)             | TRIAGE: task-level only, no cache cost. BoPO: no cache cost, no structural oracles             |
| **Cache-aware (serving, not routing)** | KVFlow, arxiv 2507.07400; KVCOMM, arxiv 2510.12872; DroidSpeak, arxiv 2411.02820 | Prefix-cache scheduling, cross-context KV reuse, layer-wise cross-LLM cache reuse  | All operate **inside one model or family**; none use cache state as a routing-time signal      |

A few additional pieces:

- **Router-R1** (arxiv 2506.09033) — RL multi-round LLM-as-router.
  Heavyweight; useful only as a reward-design reference.
- **kNN baseline** (arxiv 2505.12601) — well-tuned kNN often
  matches or beats complex learned routers; AvengersPro's Voronoi
  is essentially 1-NN-to-centroids, so we're in the right family.
- **ICL-Router** (arxiv 2510.09719), **Model-SAT** (arxiv
  2502.17282) — zero-shot model addition. Relevant once the
  candidate set grows past 3.
- **Causal LLM Routing** (arxiv 2505.16037) — end-to-end regret
  minimization on observational data. Methodologically correct
  frame for "real production paired rollouts."

### The blind spots common to *all* of the above

1. They embed the prompt as a flat string. Step-level work (TRIM,
   STEER) operates at reasoning-step granularity, not at
   tool-using-agent step-type granularity. BoPO does step-wise
   routing but RL-policy-based, not feature-based.
2. **No coding-agent router costs KV-cache invalidation when
   switching models.** MTRouter explicitly admits this as a
   limitation. TRIAGE is task-level so the question doesn't arise.
   BoPO ignores it. The closest published work that *does* cost it
   — arxiv 2508.11291 — is wireless-edge-device, not API, and not
   coding. Closing this gap on the API + coding-agent surface is
   the surviving headline.
3. **No published router makes cache-TTL-tier a learned decision
   variable.** OpenRouter exposes both 5-min and 1-hour TTLs as
   caller parameters; no routing system *learns* which to choose.
4. None combine **prefix-cluster routing + per-token confidence +
   structural oracle**. The pieces are sister problems but treated
   independently.
5. No coding-agent router validates on **Terminal-Bench 2.0**,
   **FeatureBench**, or contamination-resistant **SWE-bench Pro**
   (the three hardest current agentic coding surfaces); TRIAGE is
   on SWE-bench Lite, BoPO on synthetic agent rollouts.

Blind spots #2 and #3, taken together, are the addressable
contribution.

---

## The proposed system

A four-layer routing decision evaluated **per turn**, all hot-path
under ~10ms median, with the worst case (cheap-tier speculation
rejected, big tier takes over) bounded around ~80ms.

### Layer 1 — Feature-augmented cluster routing

Extend the current scorer's input from `embed(last_user_message)`
to a feature vector composed of:

```
features = [
  embed(last_user_message),       # 768-d Jina, current pipeline
  last_tool_result_type_onehot,   # 8-d, deterministic from message
  turn_index_bucket,              # 4-d, log-bucketed
  prefix_token_count_bucket,      # 4-d, log-bucketed
  tool_signature_embed,           # 64-d, learned over tool-name set
  cache_state_features,           # 6-d (warm flag, freshness, etc.)
]
```

`last_tool_result_type` is deterministically extracted from the
message structure: `{None, Read, Edit, Bash, Grep, Glob, WebFetch,
TaskCreate, TaskUpdate, ...}`. This is much cheaper than a learned
step-type classifier and probably captures most of the signal — the
hypothesis "different agent step types route differently" reduces in
practice to "what tool was just invoked" + "how deep in the
conversation are we."

**Why this is the right framing now (vs. the earlier "step-typed
ranking matrix per type" proposal):**

- **TRIM (arxiv 2601.10245)** is the strongest evidence that
  step-level granularity dominates query-level granularity for
  multi-step reasoning (5–6× cost efficiency on AIME). But TRIM uses
  **process reward model uncertainty** as the step signal, not step
  type. A typed step-routing ontology has weaker support in the
  literature than the original draft of this document implied.
- **Budget-Aware Agentic Routing / BoPO (arxiv 2602.21227)** does
  step-wise cost-aware routing but with an RL policy, not a
  step-type classifier. They explicitly diagnose the "always-small
  collapse" failure mode and address it with boundary-guided
  training. We should compare to BoPO in any experimental section.
- **Step type is partially redundant with cheaper features.**
  Tool-call-formatting steps are short and arrive late in
  conversations; planning steps arrive early with shorter prefixes.
  Turn index + prefix length + last-tool-result-type already encode
  most of step-type. Don't pay the maintenance cost of a learned
  classifier unless an ablation shows marginal lift.
- **Per-step-type ranking matrix multiplies parameters.** With 8
  step types × K clusters × M models, statistical efficiency
  suffers. If we ever do go to typed matrices, share via low-rank
  factorization or hierarchical Bayes — don't fit them
  independently.

**Latency cost:** trivial. Feature extraction is regex + dict
lookup. Concat + scorer is a memcpy + an existing matmul.

**Risk:** if features beyond the embedding don't change the
argmax distribution, Layer 1 collapses to AvengersPro and we've
done feature-engineering for nothing. Cheap to test offline before
committing.

### Layer 2 — Confidence-gated speculative escalation

For routes that picked the cheap tier (Haiku), do not commit yet.
Let Haiku emit the first ~32 tokens, then gate:

```
if   structural_oracle(haiku_prefix) == "fail":
       discard, route to Opus from token 0   # high-precision escalation
elif structural_oracle(haiku_prefix) == "pass" and confidence_low:
       discard, route to Opus from token 0   # ambiguous
else:  commit haiku
```

The two gates, in priority order:

1. **Structural-validity oracle (primary).** Cheap parsers run
   against the speculation:
   - **Tool-use blocks** — validate against the declared tool
     schema (name correct, JSON shape parses, required arguments
     present). The most common Haiku failure mode in Claude Code
     production is malformed `tool_use` payloads; this catches them
     before any execution.
   - **Diff hunks** — validate unified-diff prefix structure
     (`@@` lines, file headers, hunk arithmetic).
   - **Code in target languages** — tree-sitter incremental parse.
     Syntax errors in the first 32 tokens of a code block almost
     always predict a wrong direction.
2. **Token confidence (secondary).** Haiku's logprob mass over the
   first 32 tokens. Confidence > entropy as an uncertainty-routing
   signal — TAPS (arxiv 2603.27027) reports this finding, but
   **note carefully**: TAPS is about combining specialized
   speculative-decoding **drafters** (HASS / EAGLE-2 trained on
   MathInstruct vs. ShareGPT) at inference time, not about
   escalating between model tiers. We borrow the empirical finding
   ("rejected tokens have high entropy but confidence is the
   cleaner routing signal") as evidence that token confidence is
   a sane secondary gate, but **TAPS is not a precedent for
   first-N-token escalation across tiers**. The actual code-level
   precedent is MCCom; TAPS is methodological inspiration only.
   See "Learning to Route LLMs with Confidence Tokens" (arxiv
   2410.13284) for an actual confidence-as-routing-signal paper
   with ground-truth uncertainty supervision.

**Asymmetry to design around:** the structural oracle is
**high-precision, low-recall**. If it says "fail," the completion
is almost certainly bad — escalate definitively. If it says
"pass," the completion may still be wrong (most bad completions
parse fine). So the routing policy is asymmetric:
- `oracle_fail` → escalate (no further check).
- `oracle_pass + low_confidence` → escalate (ambiguous).
- `oracle_pass + high_confidence` → commit.

**Why this should help:** anecdotally, ~70% of coding-agent steps
are routine (read this file, list dir, apply the obvious refactor,
run the test). Haiku gets these right. The remaining ~30% need
Opus. Today's routing decides up-front; speculation pushes the
decision past the point where Haiku has proven it can handle the
turn.

**Direct precedent for code-cascading:**
- **MCCom (arxiv 2603.05974)** does local-cloud cascading for line-
  level code completion. Uses user accept/reject actions plus
  speculative decoding to gate, not parser-based oracles. Reduces
  inference latency 47.9% and LLM usage 46.3%. The most direct
  precedent, and a required citation. Our delta is the
  **structural-validity oracle** in place of (or alongside) user-
  action gating — particularly valuable for autonomous coding
  agents where there is no human to accept/reject.
- **STEER (arxiv 2511.06190)** — pre-step confidence gating, but for
  reasoning chains, not code, and at *step boundaries* (not first-N
  tokens).
- **Confidence Tokens (arxiv 2410.13284)** — trains an LLM to emit
  a confidence token alongside its answer; the confidence token
  reliably predicts correctness for downstream routing. Methodologically
  cleaner than reading raw logprobs, requires fine-tuning. Worth
  citing as a "if confidence-based gating turns out to be the binding
  constraint, here's the principled fix" reference.
- **Reliable Inference via Conformal Alignment (arxiv 2510.17543)**
  — frames edge-cloud cascade escalation as multiple-hypothesis
  testing with conformal alignment, providing **statistical
  guarantees on the fraction of edge decisions that satisfy cloud-
  level conditional coverage**. If we want the structural oracle
  to come with calibrated coverage rather than ad-hoc thresholds,
  this is the right machinery.

**Empirical claim and risk:** the original proposal said
"first-32-token confidence is predictive of full-completion
correctness." That is **likely too strong on confidence alone** for
two reasons:

- Token entropy correlates with code-syntactic ambiguity (variable
  name choice) more than with logical correctness. Token-level
  confidence is a known-noisy signal for code correctness.
- Coding-agent failures are dominated by *plan-level* errors
  (wrong file, wrong abstraction, missing edge case) rather than
  *token-level* errors. Cancelling 32 tokens in catches superficial
  errors at best.

The structural oracle is what saves Layer 2. The defensible
empirical claim is: **first-32-token oracle-failures are
high-precision indicators of bad completions; conditional on
oracle-pass, confidence stratifies expected quality**. Spike both
claims on offline traces *first*. If the oracle catches < 30% of
Haiku failures, Layer 2 is dead and only Layers 1 + 3 + 4 carry.

**Co-design with Layer 3:** the speculative call has a real cache
cost. If Haiku writes 32 tokens then escalates, those 32 tokens
were paid for and the prefix on Sonnet/Opus is cold. Escalation is
not free. The oracle threshold should be tuned with the cache cost
of escalation factored in — see Layer 3 for the cost model.

### Layer 3 — Cache-aware switching cost under API pricing (headline)

Today's argmax is `argmax_m [α · p̃(m) + (1−α) · (1−q̃(m))]`
(cost-blended quality, baked at training time). Extend to:

```
score(m, ttl | h) = Q̂(m | features, cluster)
                  − α · expected_output_cost(m)
                  − γ · prefix_tokens · cache_write_multiplier(m, ttl)
                                       · (1 − cache_warm[m, ttl])
                  + δ · expected_future_reads · ttl_amortization(ttl)
```

Where:

- `cache_warm[m, ttl]` is a boolean: does model `m` currently have
  a warm cache (within the chosen TTL window) for this conversation
  prefix?
- `cache_write_multiplier(m, ttl)` is the model-specific writeup
  premium. Verified Apr 2026 against Anthropic's documented pricing:
  **1.25× base input for 5-minute TTL; 2.0× base input for 1-hour
  TTL; 0.10× for cached reads.** Re-verify per release.
- `γ` calibrates the prefix value with `0` floor (cache-warm =
  free) and an analytical ceiling tied to cached-read pricing.
- The new `+ δ · …` term captures **TTL amortization**: a 1-hour
  write is more expensive upfront but breaks even after roughly
  `(2.0 − 1.25) / (1 − 0.10) ≈ 0.83` reads, so for any session
  expected to read the prefix more than once it's the dominant
  choice — the open question is *expected reads*, which depends
  on cluster-typical session length and is the lever the router
  actually pulls.

**This makes the routing decision path-dependent across (m, ttl)
jointly.** Sticky routing emerges naturally where the prefix is
large; switching becomes economical only when expected quality
lift exceeds re-prefill cost.

**Pricing nuances the cost model must absorb (verified 2026-04-30):**

- **TTL default flipped from 1h to 5m on the Anthropic API around
  March 2026** (issue #46829, partially reverted, no longer
  reliable as a default). The router must pass
  `cache_control: {type: "ephemeral", ttl: "1h"}` or `"5m"`
  *explicitly* — never assume a default.
- **Cache scope differs by deployment surface.** Direct Anthropic
  API and Azure AI Foundry: workspace-level (since Feb 5, 2026).
  AWS Bedrock and Vertex AI: organization-level isolation. If our
  deployment routes through Bedrock, cache state is shared org-
  wide and the `cache_warm` lookup is fundamentally different
  — flag this as a deployment-surface assumption.
- **Per-model tokenizer skew.** Opus 4.7 ships a new tokenizer
  that uses up to **+35% tokens for the same fixed text vs Opus
  4.6**. Per-token prices are unchanged but per-request cost can
  rise materially. The cost model must use **per-model token
  counts**, never byte counts, and re-tokenize at write/read time.
- **Cache invalidation triggers** (verified): exact-byte matching
  above the breakpoint; ordering is `tools → system → messages`,
  any change above invalidates everything below; **adding a new
  tool invalidates every cache that uses tools**; image presence
  / `tool_choice` changes / `tool_use` key reordering all break
  caches. Our prefix-stability heuristic in cache-state features
  must encode all of these.
- **Data-residency multiplier.** US-only `inference_geo` adds
  **1.1× to all token categories** (input, output, cache reads,
  cache writes) on Opus 4.7+. For compliance-bound deployments
  this scales the entire cost equation.

**Why this contribution survives prior art:**

- **arxiv 2508.11291 (Bao et al., Aug 2025)** is the nearest
  published prior art. Their cost model fuses BERT-predicted
  semantic difficulty with communication / computation overheads
  *and* KV-cache recomputation overhead during model switching
  — explicitly the same structural idea as Layer 3. Differences
  that survive: (a) edge-device pair (small on-device + large
  edge-server), not a frontier-tier API pool; (b) on-device GPU
  recomputation latency, not provider-priced dollars; (c) MMLU /
  GSM8K / MT-Bench-101 (no coding/agentic eval); (d) no TTL-tier
  decision variable. **The "TTL-tier as a learned router
  decision" piece is the cleanest unique survivor.**
- **OpenRouter sticky routing** is the closest production
  baseline. It keeps subsequent requests on the same provider
  once a cache is warm, conditional on cache-read price < base
  input. But it does *not* score model alternatives by cache-
  cost delta (it only chooses provider, not model tier), and
  TTL is exposed as a caller knob, not a learned variable.
- **MTRouter (arxiv 2604.23530)** explicitly admits switching
  invalidates cache and "we leave this for future work."
- **TRIAGE (arxiv 2604.07494)** is task-level — one model per
  SWE-bench task. Cache-cost question doesn't arise.
- **BoPO (arxiv 2602.21227)** is step-wise but RL on outcome
  reward without cache cost in the reward.
- **GORGO (arxiv 2602.11688)** optimizes prefix-cache reuse +
  network latency at the **dispatcher level** for replicas of one
  model. Adjacent but not at model-tier selection.
- **Continuous Semantic Caching (arxiv 2604.20021)** treats
  **response-cache** switching cost in continuous embedding space
  with sublinear regret. Adjacent (response, not prefix).
- **Cache-aware *serving* literature** (KVFlow 2507.07400,
  KVCOMM 2510.12872, DroidSpeak 2411.02820, Continuum 2511.02230)
  models cache cost in dispatch / scheduling / prefix reuse, but
  at the serving layer — not as input to a model-selection
  objective.

**DroidSpeak as the negative-result baseline:** cross-LLM cache
sharing across architecturally-different models is generally not
viable; DroidSpeak shows it's *possible* for fine-tuned siblings
of a shared base, not for Anthropic's heterogeneous Haiku /
Sonnet / Opus pool. So in our setting the switching penalty is
real and non-removable. That's the load-bearing assumption for
the contribution.

**Empirical anchor (re-verify Anthropic pricing at paper draft
time):** under current pricing, cached reads are 10× cheaper than
base input; cache writes carry a 1.25× / 2.0× premium. For long
Claude Code sessions with 100k+ token prefixes, the per-turn
cache cost is comparable to or larger than the quality-difference
cost between models. Sticky routing is the default; the router's
job is to recognize when the stickiness should break.

**Latency cost:** zero — it's a scalar in the existing argmax,
plus one TTL discriminator.

**Risk:** mostly a tuning question (`γ`, `δ`). The analytical
floor based on Anthropic's cache pricing gives a starting value;
production data refines it. Secondary risk: cache-state mis-
tracking. With workspace-level isolation on the direct API the
error is bounded; with org-level isolation on Bedrock/Vertex,
the cost estimate may be very wrong if other workspaces evict our
prefix. **Mitigation: scope deployment to direct Anthropic API
for paper-draft, and treat Bedrock/Vertex as a separate ablation.**

**Open algorithmic question:** with switching cost and TTL choice,
this is a **contextual bandit with switching costs and arm-
specific decay** (a "metrical task system" with non-stationary
arm rewards / smoothed online learning). PILOT (arxiv 2508.21141)
is the closest published frame for budget; Continuous Semantic
Caching (arxiv 2604.20021) provides the regret-bound machinery
for cache-state switching. We can extend with a per-(m, ttl)
switching penalty derived from cache state. Worth a literature
deep-dive during the formal write-up.

### Layer 4 — Decision-aware online training (engineering, not novel)

Drop AvengersPro's α-blended absolute-score regression. Replace
with a **Bradley-Terry / contextual-dueling-bandit ranker**
trained on production paired rollouts.

This is the right engineering choice but **not a research
contribution** — already published:

- **EquiRouter (arxiv 2602.03478)** prescribes exactly this fix
  for the "routing collapse" failure mode and reports 17% / 12%
  cost reductions on RouterBench / MMR-Bench at GPT-4-level
  performance.
- **CCFT / Dueling Feedback (arxiv 2510.00841)** is the
  contextual-dueling-bandit formulation with Feel-Good Thompson
  Sampling and Category-Calibrated Fine-Tuning. Direct method.
- **Causal LLM Routing (arxiv 2505.16037)** is the methodologically
  cleanest frame: end-to-end regret minimization with counterfactual
  estimation under treatment-selection bias from observational data.

We implement, cite, move on. The marginal contribution is
**applying these methods inside our cluster-routing structure**
(per-cluster ranking matrices instead of a single global model)
and on **coding-agent paired rollouts**, not the ranker itself.

**Critical change to the original proposal: drop κ-based judge
stability from the online retraining loop.** Cohen's κ for code
judges is poor (verified against arxiv 2507.16587, "On the
Effectiveness of LLM-as-a-judge for Code Generation and
Summarization"):

- GPT-4-turbo as code judge: κ ≈ 0.21 (Java), κ ≈ 0.10 (Python)
  vs. test-execution ground truth.
- False-positive rate (missing bugs): ~50% on Java.
- False-negative rate (rejecting correct code due to artificial
  hallucination): ~54% on Python.
- Smaller judges (DeepSeek 1.3B/6.7B, CodeLlama 7B) score near
  zero or negative.
- The mainstream rule of thumb (κ ≥ 0.6) is **not currently
  achievable with single-LLM-as-judge for code**.

(For non-code MCQ-style tasks, GPT-4 Turbo / Llama-3 70B reach
κ ≈ 0.79–0.84 per arxiv 2406.12624 — the inadequacy is task-
specific to code, not general.)

**Replace with a tiered judge protocol:**

1. **Execution-grounded preference (primary).** Run the completions
   inside the agent's tool environment; the arm whose tests /
   type-check / lint pass becomes the preferred arm. Binary
   preference, no κ floor. ~30–60% of agent traces in Claude Code
   admit this depending on the tool mix.
2. **Adversarial-mutant stability check (secondary).** SWE-ABS
   (arxiv 2603.00520) demonstrates that ~20% of "passing" patches
   on top SWE-bench-Verified agents are *semantically incorrect*
   — the test suite is too weak to catch them. Apply SWE-ABS-style
   mutation testing (program-slicing-driven coverage augmentation
   + adversarial mutant synthesis) to a sample of execution-passing
   preferences before promoting them into training data. Discard
   preferences where the "winning" patch fails strengthened tests.
   Expect this to reject ~15–20% of nominally-clean preferences.
3. **Multi-judge ensemble panel (tiebreaker only).** For the
   ~40–70% of traces with no execution signal (refactors, dialog
   turns, tool calls with no test impact), use a 3-judge panel
   chosen from the Judge's Verdict (arxiv 2510.09738) tier-1 list
   — judges that pass both the correlation gate (Pearson r ≥ 0.80
   vs. human) and the human-likeness z-test. Require 2/3 agreement.
   This is **fallback only**, not the primary signal.
4. **Reasoning-trace drift monitoring (Stability Trap, arxiv
   2601.11783).** Don't just monitor verdict agreement — judges can
   exhibit >99% verdict stability while their reasoning traces
   diverge to ~19% agreement on objective tasks. Track both.
   If reasoning-trace stability drops below 70% across runs, the
   panel is silently drifting and verdicts will eventually follow.
5. **Quarterly recalibration** on a 200–500-item human-labeled
   gold set. Block training updates if judge-vs-gold verdict
   agreement drops below 75% **or** reasoning-trace stability
   drops below 70%.
6. **Bias monitoring** every retrain: position bias (swap order),
   verbosity (length-controlled probes), self-preference (judge
   prefers same model family).
7. **Routing Collapse Index (RCI)** from EquiRouter, computed on
   every retrain. If RCI rises, the decision-aware loss is
   failing — inspect before promoting.

**Online retraining loop guard:** the κ floor is *out*. The new
floor is "execution-grounded fraction × adversarial-mutant pass
rate" — we won't promote a retrained ranker unless it preferred
the execution-correct arm in ≥ 70% of execution-grounded cases
*and* the adversarial-mutant rejection rate is < 25%.

---

## Why this is publishable

A paper-worthy contribution needs to be (a) novel against the
*current* literature, (b) empirically validated on a serious
benchmark, (c) reproducible. Reframed contributions:

- **Novelty (narrowed).** The unqualified "first to model KV-cache
  invalidation cost in a routing objective" claim does not survive
  arxiv 2508.11291. The surviving novel pieces:
  1. **First routing system to integrate API-provider-priced
     prompt-cache cost** (`γ · prefix_tokens · cache_write_multiplier
     (m, ttl) · (1 − warm)`) into model selection across **frontier-
     tier API models on agentic-coding benchmarks**. Differentiated
     from 2508.11291 (on-device GPU recomputation latency in an
     edge-device pair, MMLU/GSM8K/MT-Bench) and from OpenRouter
     sticky routing (provider-level, single-model, doesn't score
     model alternatives by cache delta).
  2. **First to make TTL-tier (5-min vs 1-hour) a learned router
     decision variable.** Verified open after a deep prior-art
     audit — no published or production system learns this choice.
  3. **Structural-validity oracle for code/tool-use speculative
     escalation in autonomous agents** — distinct from MCCom (user-
     action gating) and from STEER (math step boundaries), and
     extensible to a conformal-coverage formulation per arxiv
     2510.17543.
- **Validation surface (revised).** Lock the headline matrix to
  contamination-resistant agentic benchmarks: **SWE-bench Pro on
  the Scale SEAL standardized scaffold (primary)**, **SWE-bench-
  Live**, **FeatureBench**, and **Terminal-Bench 2.0** (cache-
  cost-heavy because tool-use prefixes dominate). Use **SWE-bench
  Verified** *only* for direct head-to-head with TRIAGE (TRIAGE's
  evaluation surface) and as the SWE-ABS-strengthened tier (since
  the leaderboard is contamination-impaired). **Drop Aider Polyglot
  from the headline**: Exercism is widely indexed and contamination
  is plausible per audit. Optional additions: SWE-rebench (monthly
  rolling), SWE-Bench++ (auto-generated 11k instances across 11
  languages), Multi-SWE-bench (cross-language). **Required
  baselines:** AvengersPro, TRIAGE, BoPO / Budget-Aware Agentic
  Routing, OpenRouter-style sticky routing (cross-tier variant),
  always-Sonnet, always-Opus, and "Claude Code default."
- **Failure-mode demonstration.** Replicate "routing collapse"
  (EquiRouter) on AvengersPro using our own paired-rollout data,
  then show the decision-aware ranking loss eliminates it.
- **Reproducibility.** Training scripts, centroid format,
  feature extractors, cache-cost formula, and oracle parsers are
  all small enough to release. Existing eval harness has the
  bones for the reproducibility appendix. Cite RouterXBench
  (arxiv 2602.11877) as the principled router-eval framework
  to align our reproducibility appendix with.

Working title candidates:

- **"Cache-Aware LLM Routing for Tool-Using Agents Under
  Anthropic-Style Prompt Caching"** (cleanest, leads with the
  novel piece).
- **"From Avengers to CacheRoute: Routing-Time Cache Cost for
  Multi-Turn Coding Agents"** (failure-mode-diagnostic frame).
- **"PrefixRouter: Structural Oracles and Cache-Aware Switching
  for Coding-Agent LLM Selection"** (foregrounds Layers 2 + 3).

---

## Industry landscape and competitive risk

This section was added during the second-revision audit. Routing
is no longer just an academic problem — multiple production
systems already ship cost-quality model routers, and the
commercial threat to the *concept* of an LLM router is high. The
specific niche we're staking out (cache-aware cross-tier model
selection on coding agents) remains uncontested, but the moat
is narrower than it would have been in 2024.

**Production systems to engage / cite:**

- **OpenRouter Pareto Router (Apr 2026)** — Pareto-frontier
  routing over a curated coding-model shortlist parameterised by
  `min_coding_score ∈ [0, 1]`. Tuned for coding. Selection by
  benchmark, not state. **Does not model cache cost.**
- **OpenRouter sticky routing** — keeps subsequent same-
  conversation requests on the same provider once a cache is
  warm; activates only when `cache_read_price < base_input_price`.
  Tracked per (account × model × conversation). Closest production
  analog to Layer 3 — but at provider-stickiness for *one* model,
  not cross-tier model selection. **The right baseline for the
  experimental comparison.**
- **Microsoft Foundry Model Router (Nov 2025)** — trained ML
  router across 18 LLMs, three modes (Quality / Cost / Balanced),
  tool-use support. **Does not model cache cost in routing
  decision.** Highest commercial threat for the *router-as-
  product* concept, but does not contest Layer 3 specifically.
- **AWS Bedrock Intelligent Prompt Routing** — encoder-based
  cost-quality routing on a fixed model family. No published
  cache-awareness. Medium-low threat.
- **HotSwap** (DEV.to, March 2026) — application-layer router
  exploiting Anthropic 5-min TTL: nano model handles read-only
  exploration while primary's cache stays warm. Industry/blog,
  not peer-reviewed. Earliest application-layer demonstration of
  the cache-economics framing — cite to ground the framing.
- **LiteLLM / Portkey / Kong AI Gateway / Cloudflare AI Gateway**
  — gateway/observability products, not learned routers. Low
  threat.

**Coding-agent product threat surface:**

- **Cursor / Cline / Aider / Continue / Roo Code** — all let
  users pick a model. Roo Code via OpenRouter inherits sticky
  routing. None has a learned router with cache-cost objective.
- **Claude Code itself** — Anthropic has not shipped automatic
  Haiku/Sonnet/Opus routing inside Claude Code. **GitHub issue
  #44976** ("Feature: Auto model routing by task type") is
  **open and unresolved**. Community estimates suggest 60–70% of
  Opus tokens could go to Sonnet/Haiku without quality loss —
  exactly the gap our system targets. **This is also the largest
  hidden risk:** if Anthropic ships native auto-routing inside
  Claude Code with cache-aware logic during our 3-month
  engineering window, the productization argument collapses
  even though the research argument survives.

**Verdict on commercial moat:**

- Threat to the *concept* of an LLM router: high. (Foundry,
  OpenRouter, Bedrock all in market.)
- Threat to *cross-tier cache-aware routing as an objective*: low.
  No production system optimizes
  `γ · prefix_tokens · cache_write_multiplier(m, ttl) · (1 − warm)`
  across model tiers. OpenRouter sticky routing is the nearest
  analog and operates at provider level only.
- Threat to *coding-agent-specific cache-aware routing*: low.
  TRIAGE / BoPO are the academic competitors; neither costs cache.

**Kill criteria** (from the second-revision audit, slightly
adapted):

- *Flips RED if:* (a) Anthropic announces native automatic
  Haiku/Sonnet/Opus routing inside Claude Code with cache-aware
  logic; or (b) a head-to-head on SWE-bench Pro shows our cache-
  aware objective adds < 5% relative cost reduction over a
  cross-tier sticky-routing baseline; or (c) a second cache-aware-
  routing-at-API-pricing paper surfaces in the next 60 days.
- *Flips GREEN if:* a working SWE-bench Pro prototype shows
  > 15% cost reduction at iso-quality vs. (a) AvengersPro
  baseline, (b) TRIAGE-style task-level routing, (c) cross-tier
  sticky routing — within 6 weeks of starting Layer 3.

**Fallback paper if Layer 3 is scooped further:** "Structural-
validity oracles for cheap-model speculative escalation in tool-
using coding agents" — Layer 2 alone is paper-worthy with cleaner
novelty (asymmetric oracle policy, tree-sitter / diff-prefix /
JSON-schema-partial parser ensemble for code-and-tool-use is
genuinely new).

---

## Concrete next steps if we pursue this

Sequenced for risk-retirement, not project-management neatness.
Each step gates the next.

1. **Reproduce the AvengersPro baseline on the eval harness.**
   No point comparing to a paper number — produce a committed
   AvengersPro-faithful baseline against our own coding-agent
   eval set. Already on the Phase 1a critical path.
2. **Spike the structural-validity oracle on logged traces.**
   Replay Haiku's first 32 tokens on real production traffic;
   compute oracle pass/fail vs. ground-truth full-completion
   success. **Kill criteria:** if the oracle catches < 30% of
   Haiku failures with > 90% precision, Layer 2 is dead. Two-day
   spike.
3. **Build the cache-state cost model.** Implement
   `prefix_tokens · write_multiplier(m, ttl) · (1 − cache_warm[m])`
   with measured Anthropic pricing. Verify against billing on a
   representative sample of production conversations. **This is
   the headline contribution — get this right before anything
   else.** One-week spike.
4. **Instrument feature extraction (Layer 1).** Trivial code
   change in `internal/proxy/service.go`. Log the new features on
   every routing decision through OTel for one week, then look at
   feature-importance vs. routing decisions. If turn index +
   tool-result-type explain most of the structure, we don't need
   step-type classification.
5. **Pick the validation benchmarks.** SWE-bench Pro (SEAL) +
   SWE-bench-Live + FeatureBench + Terminal-Bench 2.0 minimum.
   Add Multi-SWE-bench / SWE-Bench++ for cross-language. SWE-bench
   Verified only for the TRIAGE head-to-head (and consider the
   SWE-ABS-strengthened version for the comparison to discount
   the ~20% inflated pass rate). Drop Aider Polyglot from headline
   evaluation. The commitment is to having all the headline ones
   running before paper draft.
6. **Implement the cache-aware switching cost behind a flag.**
   Cleanest individual contribution; should ship as a standalone
   PR even if the broader research direction stalls. `m_prev` and
   the cached prefix length are already observable in our proxy
   service; only `γ` is new.
7. **Stand up paired-rollout logging at low rate.** 1% of traffic,
   shadow-rolled to a second candidate model, scored by execution
   where available + multi-judge ensemble elsewhere. This is the
   data substrate for Layer 4.
8. **First paired-rollout retrain.** With a few thousand
   preferences accumulated, retrain the per-cluster matrices with
   the Bradley-Terry loss. Compute RCI. Shadow against the
   AvengersPro baseline. Promote only on statistical gate (≥ 1k
   decisions, p < 0.01) **and** RCI not rising.

If steps 2 and 4 both die (oracle no good, features add no lift),
the proposal collapses to "AvengersPro + cache-aware switching
cost + dueling-bandit training" — still a defensible engineering
improvement and *still* a publishable Layer-3 paper because that
piece stands alone.

---

## Open questions / unresolved before paper-track work

1. **`γ` and `δ` calibration with multi-tier caching.** Anthropic
   prompt caching has multiple TTL tiers (5-min, 1-hour); cache-
   hit isn't binary. The analytical floor is an upper bound on
   switch cost; production data will tighten it. Don't commit to
   a single (γ, δ) pair until we have a few weeks of cache-hit-
   rate telemetry. **Bonus complication:** the TTL default flipped
   1h→5m on the API around March 2026 (issue #46829, partially
   reverted) — pass `ttl` explicitly, never assume defaults.
2. **Tokenizer differences across the candidate pool.** Verified
   in the audit: Opus 4.7 ships a new tokenizer that uses **up to
   +35% tokens for the same fixed text vs. Opus 4.6**. Per-token
   prices unchanged but per-request cost moves materially. The
   cost model must measure per-model token counts, never byte
   counts, and re-tokenize at write/read time.
3. **Cache scope by deployment surface.** Direct Anthropic API and
   Azure AI Foundry: workspace-level (since 2026-02-05). AWS
   Bedrock and Vertex AI: organization-level isolation. The
   `cache_warm[m, ttl]` lookup model is fundamentally different
   across these. **Decision:** scope the headline experiment to
   direct Anthropic API; treat Bedrock/Vertex as a separate
   deployment-surface ablation if needed.
4. **US-only `inference_geo` 1.1× multiplier on Opus 4.7+.** For
   compliance-bound deployments this scales every term in the
   cost equation. Decide whether the paper assumes vanilla
   pricing or geo-restricted pricing; document.
5. **Feature-importance vs. classifier-importance for Layer 1.**
   We propose deterministic feature extraction over a learned
   step-type classifier. If a classifier turns out to add lift,
   the failure mode is increased maintenance. Ablate carefully.
6. **Online judge stability at scale.** Execution-grounded
   preferences cover 30–60% of traces; the multi-judge ensemble
   covers the rest. The remainder includes the most ambiguous
   cases (purely-creative refactors, no tests). Acceptable, but
   the ranker's variance on this slice will be higher. Monitor
   reasoning-trace stability separately from verdict stability
   (Stability Trap, arxiv 2601.11783) — the panel can drift
   silently while verdicts stay >99% stable.
7. **Anthropic-only candidate set.** All four layers generalize
   to multi-provider, but most of the cache-economics intuitions
   break down across providers (each has its own cache
   semantics). Stick with Anthropic-only for the paper.
8. **Where Layer 2 lives in the architecture.** Layer 2 is more
   invasive than 1, 3, 4 — it changes the dispatch path because
   the speculative call has to commit or discard. Probably its
   own `internal/router/speculative/` package wrapping the
   cluster scorer, not a modification to it.

---

## References

### Direct competitors (must engage in any paper draft)

- **TRIAGE** — arxiv 2604.07494, Madeyski, Apr 2026. Code-health
  signals → tier routing on SWE-bench Lite. **The most direct
  academic competitor.**
- **Budget-Aware Agentic Routing / BoPO** — arxiv 2602.21227,
  Feb 2026 (institutional attribution per audit not independently
  verified — was previously listed as Microsoft, but the audit
  flagged this as unverified). Step-wise RL with boundary-guided
  training; diagnoses "always-small collapse."
- **TRIM** — arxiv 2601.10245, Kapoor et al. Step-level routing
  via process reward model uncertainty (5–6× cost efficiency on
  AIME).
- **Dynamic Quality-Latency Aware Routing (Bao et al.)** — arxiv
  2508.11291, Aug 2025. **Nearest published prior art for Layer 3
  headline.** Wireless edge-device routing that quantifies KV-cache
  recomputation overhead during model switching. Differentiated
  from our work by setting (edge-device, not API), cost type
  (latency, not dollars), and benchmark suite (MMLU/GSM8K/MT-Bench,
  not coding). Must be cited prominently in any draft.

### Routing — direct relatives

- **AvengersPro (us)** — arxiv 2508.12631. Cluster + α-blend.
- **The Avengers v3** — arxiv 2505.19797. Predecessor.
- **MTRouter** — arxiv 2604.23530, Apr 2026 (Zhang et al., code at
  `github.com/ZhangYiqun018/MTRouter`). Multi-turn cost-aware
  routing with history-model joint embeddings; admits no KV-cache
  cost; no coding eval. **Note**: this paper has been mistakenly
  labeled "hallucinated" by web-search-based critique tools twice
  in our audit history — direct arxiv verification confirms it is
  real and load-bearing.
- **R2-Router** — arxiv 2602.02823. Output-length-as-control via
  "routing-as-reasoning" — a reasoning-style joint policy over
  `(model, length-budget)`, not a multi-head MLP as earlier drafts
  of this document claimed.
- **STEER** — arxiv 2511.06190. Step-level confidence-gated
  routing for math/multi-hop QA.
- **EquiRouter** — arxiv 2602.03478. Diagnoses "routing
  collapse"; ranking-loss fix; introduces RCI.
- **CCFT / Dueling Feedback** — arxiv 2510.00841. Contextual
  dueling bandits with Feel-Good Thompson Sampling.
- **Causal LLM Routing** — arxiv 2505.16037. End-to-end regret
  minimization on observational data.
- **Router-R1** — arxiv 2506.09033. RL-based LLM-as-router.
- **xRouter** — arxiv 2510.08439. RL cost-aware orchestration.
- **kNN baseline** — arxiv 2505.12601. kNN often matches SOTA.
- **ICL-Router** — arxiv 2510.09719. Zero-shot model addition.
- **Capability Instruction Tuning / Model-SAT** — arxiv 2502.17282.
  Alternative new-model-without-retraining approach.
- **AgentRouter (the name)** — arxiv 2510.05445, Oct 2025. KG-
  guided multi-agent QA. Different domain; owns the name.

### Code cascading and speculative routing

- **MCCom** — arxiv 2603.05974. Local-cloud cascading for line-
  level code completion via user-action gating + speculative
  decoding. **Direct precedent for Layer 2** (our delta is
  parser-based oracle vs. user-action gating).
- **TAPS** — arxiv 2603.27027. Speculative-*decoding* drafter
  selection (HASS / EAGLE-2 trained on different domains); we
  borrow the empirical "confidence > entropy as routing signal"
  finding only — **not a precedent for first-N-token cross-tier
  escalation**, contrary to earlier drafts of this document.
- **Confidence Tokens** — arxiv 2410.13284. Trains an LLM to
  emit a confidence token alongside its answer; cleaner method
  than reading raw logprobs if confidence-based gating becomes
  the binding constraint.
- **Reliable Inference via Conformal Alignment** — arxiv 2510.17543.
  Edge-cloud cascade with conformal-alignment guarantees on
  conditional coverage. Formal frame for upgrading the structural
  oracle from point estimate to coverage-bounded test.
- **R2R** — token-level routing for divergent reasoning paths.

### Cache-aware routing and serving

- **Dynamic Quality-Latency Aware Routing (Bao et al.)** — arxiv
  2508.11291. **Nearest published Layer 3 prior art**: edge-device
  cost model that quantifies KV-cache recomputation overhead during
  model switching. See "Direct competitors" above for differentiation.
- **Continuous Semantic Caching** — arxiv 2604.20021. Online
  algorithms for response-cache switching cost in continuous
  embedding space with sublinear regret. Adjacent prior work
  (response cache, not prefix cache).
- **GORGO** — arxiv 2602.11688. Cross-region LLM load balancing
  optimizing prefix-cache reuse + network latency at the
  dispatcher layer for replicas of one model. Adjacent (replica
  selection, not model-tier selection).
- **KVFlow** — arxiv 2507.07400. Workflow-aware KV cache for
  multi-agent. Introduces the "Agent Step Graph" abstraction.
- **KVCOMM** — arxiv 2510.12872. Cross-context KV cache reuse
  via offset alignment with anchor pool.
- **DroidSpeak** — arxiv 2411.02820. Cross-LLM KV cache sharing
  for fine-tuned siblings of a shared base. **The negative-result
  baseline for cross-architecture sharing.**
- **Continuum** — arxiv 2511.02230. Multi-turn agent scheduling
  with KV cache TTL.

### Coding-agent benchmarks (validation surface)

- **SWE-bench Pro** — arxiv 2509.16941. Long-horizon enterprise,
  contamination-resistant via GPL licensing. **Primary**: this is
  the headline-claim eval surface. Use the Scale SEAL standardized
  scaffold for fair comparison.
- **SWE-bench-Live** — arxiv 2505.23419. Live-updatable, multi-
  language, contamination-resistant. **Required.**
- **FeatureBench** — arxiv 2602.10975. Feature-level (multi-PR)
  agentic. Even Claude 4.5 Opus only resolves 11%. **Required.**
- **Terminal-Bench 2.0** — closest test to "agent-in-the-loop
  tool use" (what Claude Code actually does). Cache-cost-heavy
  because tool-use prefixes dominate. **Required.**
- **SWE-bench Verified** — used *only* for the TRIAGE head-to-
  head; treat as contamination-impaired. Consider running against
  the SWE-ABS-strengthened version (arxiv 2603.00520) which
  rejects ~20% of nominally-passing patches as semantically
  incorrect.
- **SWE-ABS** — arxiv 2603.00520. Adversarial benchmark
  strengthening for test-based evaluation; rejects ~20% of
  Verified leaderboard "solved" patches. Used in the judge stack
  (Layer 4) and as a tier-strengthening of SWE-bench Verified
  for the TRIAGE comparison.
- **SWE-rebench** (swe-rebench.com) — monthly-updated,
  contamination-resistant, live router-eval-suitable.
- **SWE-Bench++** — arxiv 2512.17419. Auto-generated 11k
  instances across 11 languages. Optional cross-language scale.
- **Multi-SWE-bench** — arxiv 2504.02605. Multilingual.
- **Aider Polyglot** — **dropped from headline** (Exercism
  contamination plausible per audit). Optional ablation only.
- **MasRouter** — arxiv 2502.11133. Only published routing
  system reporting MBPP/HumanEval, but single-shot.
- **Inside the Scaffold** — arxiv 2604.03515. Coding-agent
  scaffold taxonomy. Useful as a "the field needs this" cite.

### Routing benchmarks (cross-comparison)

- **RouterBench (Martian)** — arxiv 2403.12031. 11 models;
  ablation use.
- **RouterXBench** — arxiv 2602.11877. Principled router-
  evaluation framework (router ability × scenario alignment ×
  cross-domain robustness); align reproducibility appendix to
  this.
- **VL-RouterBench** — arxiv 2512.23562. Vision-language.
- **PILOT** — arxiv 2508.21141. LinUCB + multi-choice knapsack
  for budget. Closest published frame for our switching-cost
  bandit problem.

### Judge stability

- **On the Effectiveness of LLM-as-a-judge for Code** — arxiv
  2507.16587. Source of the κ ≈ 0.21 (Java) / 0.10 (Python)
  numbers; ~50% false-positive rate, ~54% false-negative rate
  for code judges. **Use as the primary citation justifying
  execution-grounded preference over κ-based judging.**
- **Judging the Judges** — arxiv 2406.12624. κ ≈ 0.79–0.84 on
  MCQ-style tasks for GPT-4 Turbo / Llama-3 70B; cite to
  contextualize that the κ inadequacy is task-specific to code.
- **Stability Trap** — arxiv 2601.11783. Verdict stability can
  exceed 99% while reasoning-trace stability falls below 20%
  on objective tasks. **Required for monitoring online judge
  drift in the retraining loop.**
- **Judge's Verdict** — arxiv 2510.09738. Two-tier protocol
  (correlation + human-likeness). Use for held-out judging when
  execution isn't available, but only as a tiebreaker — not as
  the primary signal.

### Industry / production systems (audit-discovered)

- **OpenRouter Pareto Router and sticky routing** —
  openrouter.ai. Pareto-frontier routing for coding +
  application-layer cache stickiness. The closest production
  baseline for Layer 3. **Required production baseline in any
  experimental section.**
- **Microsoft Foundry Model Router** — Nov 2025 release;
  trained ML router across 18 LLMs. No published cache-cost
  awareness. The largest commercial-threat-to-the-concept-of-a-
  router system.
- **AWS Bedrock Intelligent Prompt Routing** — encoder-based
  cost-quality routing on a fixed model family.
- **HotSwap** (DEV.to, Mar 2026) — application-layer router
  exploiting Anthropic 5-min TTL with cache-economics framing.
  Industry/blog precedent for the framing; cite for
  intellectual honesty.

### Anthropic pricing reference (verified 2026-04-30)

- `platform.claude.com/docs/en/build-with-claude/prompt-caching`
- `platform.claude.com/docs/en/about-claude/pricing`
- `github.com/anthropics/anthropic-cookbook/blob/main/misc/prompt_caching.ipynb`
- `github.com/anthropics/claude-code/issues/46829` — TTL default
  silent regression 1h→5m (March 2026).
- `github.com/anthropics/claude-code/issues/44976` — community
  feature request for native auto-routing inside Claude Code
  (open as of audit). **Watch this issue — if it ships, Layer 3's
  productization argument is at risk.**

---

## Provenance

### First revision (2026-04-30, mid-day) — competitive-landscape audit

Verified additions from a critique by another AI: TRIAGE
(2604.07494), BoPO / Budget-Aware Agentic Routing (2602.21227),
AgentRouter name collision (2510.05445), TRIM (2601.10245), MCCom
(2603.05974), TAPS (2603.27027), KVFlow (2507.07400), DroidSpeak
(2411.02820), KVCOMM (2510.12872), Causal LLM Routing (2505.16037),
xRouter (2510.08439), Inside the Scaffold (2604.03515), Judge's
Verdict (2510.09738).

Substantive changes from prior draft: Layer 3 promoted to headline
contribution; Layer 4 demoted from novel to engineering choice;
Layer 1 reframed from "step-typed ranking matrix" to "feature-
augmented cluster routing"; Layer 2's empirical claim softened from
"first-32-token confidence predicts correctness" to "oracle-fail is
high-precision, oracle-pass + low-confidence is ambiguous, oracle-
pass + high-confidence commits"; κ-based judge-stability protocol
replaced with execution-grounded + multi-judge + Judge's Verdict
tiered protocol; "AgentRoute" name flagged for replacement
(CacheRoute / PrefixRouter / AgentCascade as candidates).

One claim was rejected after direct arxiv verification: the
critique alleged that MTRouter (arxiv 2604.23530) does not exist
and is a hallucinated citation. Direct verification confirmed
MTRouter is real (Zhang et al., 2026-04-26, code at
`github.com/ZhangYiqun018/MTRouter`).

### Second revision (2026-04-30, evening) — deep-research audit

Re-verified critique from a second AI (a deep-research agent run
against this document). Verdict: yellow / proceed with reframe.

**Verified additions absorbed into this revision:**

- **arxiv 2508.11291** "Dynamic Quality-Latency Aware Routing for
  LLM Inference in Wireless Edge-Device Networks" (Bao et al.,
  Aug 2025) — **the prior-art counterexample the first revision
  missed**. Headline narrowed from "first to model KV-cache
  invalidation cost" to "first to integrate API-priced cache cost
  + TTL-tier as decision variable + coding benchmarks."
- **arxiv 2604.20021** Continuous Semantic Caching — adjacent
  prior work on response-cache switching cost with sublinear
  regret bounds.
- **arxiv 2602.11688** GORGO — dispatcher-level prefix-cache +
  network-latency optimization; demarcates dispatcher vs. model-
  selection layer.
- **arxiv 2603.00520** SWE-ABS — adversarial benchmark
  strengthening; ~20% of SWE-bench Verified passes are
  semantically incorrect. Inserted into the Layer 4 judge stack.
- **arxiv 2602.11877** RouterXBench — principled router-eval
  framework; alignment target for the reproducibility appendix.
- **arxiv 2601.11783** Stability Trap — verdict stability vs.
  reasoning-trace stability divergence; required for online judge
  drift monitoring.
- **arxiv 2510.17543** Conformal Alignment cascade — formal
  upgrade path for the structural oracle.
- **arxiv 2410.13284** Confidence Tokens — cleaner confidence-
  routing alternative if first-N-token logprobs prove noisy.
- **arxiv 2507.16587** LLM-as-a-judge for code — ground-truth
  source for the κ ≈ 0.21 / 0.10 numbers and false-pos / false-
  neg rates that motivate dropping κ from the retraining loop.
- **arxiv 2406.12624** Judging the Judges — context for why κ
  inadequacy is task-specific to code, not general.

**Substantive changes from first revision:**

1. Layer 3 headline narrowed (the unqualified KV-cache-cost-in-
   routing-objective claim does not survive 2508.11291). New
   headline: API-priced caches + TTL-tier as a learned router
   decision variable + frontier-tier API model pool + coding
   benchmarks. Differentiated from 2508.11291 (edge-device, GPU
   recomputation, MMLU/GSM8K/MT-Bench) and OpenRouter sticky
   routing (provider-level, single-model).
2. Layer 2 TAPS reference reframed: TAPS routes among speculative-
   decoding **drafters** (HASS / EAGLE-2 trained on different
   data), not across model tiers. The confidence-vs-entropy
   finding is borrowed only as a methodological signal; the actual
   code-cascading precedent is MCCom.
3. Layer 4 judge stack restructured: execution-grounded preference
   primary; SWE-ABS adversarial mutants as secondary stability
   check (catches the ~20% inflated pass rate); multi-judge panel
   demoted to tiebreaker only; reasoning-trace drift monitoring
   added (Stability Trap); κ removed from online retraining loop
   entirely.
4. Anthropic pricing nuances absorbed into Layer 3 cost model:
   verified 1.25× / 2.0× / 0.10× multipliers; TTL default flipped
   1h→5m on the API around March 2026 (must pass `ttl` explicitly);
   workspace-scope on direct API and Azure AI Foundry vs. org-scope
   on Bedrock/Vertex AI; Opus 4.7 tokenizer +35% vs. 4.6;
   `inference_geo` US-only adds 1.1× to all categories.
5. Benchmark matrix restructured: SWE-bench Pro (SEAL standardized
   scaffold) becomes primary; Aider Polyglot dropped from headline
   (contamination plausible); SWE-bench Verified used only for the
   TRIAGE head-to-head, ideally with SWE-ABS-strengthened tier.
6. Industry landscape and kill criteria added: OpenRouter sticky
   routing as the production baseline; Foundry Model Router /
   Bedrock IPR as commercial concept-threats; community demand for
   native Claude Code auto-routing (issue #44976) as the largest
   hidden risk; HotSwap blog post as application-layer prior art.

### MTRouter false-negative — **twice now**

Both audit rounds asserted that MTRouter (arxiv 2604.23530) does
not exist. Both claims are wrong. Direct arxiv-API lookup on
2026-04-30 confirms: title "MTRouter: Cost-Aware Multi-Turn LLM
Routing with History-Model Joint Embeddings", authors
Zhang/Li/Wang/Feng/Yang/Wang/Zhang/Bai/Hu, published 2026-04-26,
abstract links to `github.com/ZhangYiqun018/MTRouter`.

**Lesson learned**: web-search-based critique tools cannot reliably
disprove arxiv IDs newer than their search-index cutoff or the
indexing latency between arxiv submission and Google indexing.
Direct arxiv-API verification is the only ground truth for "does
paper X at ID Y exist." When integrating any future external
critique that calls a citation hallucinated, do this verification
before either trusting or rejecting the claim.

The MTRouter citation stays. So does this footer, as a permanent
warning to future readers (and future revisions).
