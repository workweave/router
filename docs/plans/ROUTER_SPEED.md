Created: 2026-05-03
Last edited: 2026-05-03

# Router — Speed-Aware Routing Plan

> **Status: Draft, May 3, 2026.** Proposes extending the cluster scorer's
> 2-axis (quality, cost) ranking to a 3-axis (quality, cost, latency)
> blend. Algorithmic change is small (one extra normalized term in the
> per-cluster blend, 2-D simplex sweep in `train_per_cluster_alpha.py`
> instead of 1-D); engineering cost lives in sourcing latency data and
> defining a 3-axis evaluation methodology. **Motivating event:**
> Cerebras was added as a provider on `steven/router-cerebras-provider`
> (commit 5bd9b28a1e, May 2, 2026); without a speed axis the cluster
> scorer has no signal to prefer Cerebras over Anthropic for the same
> quality tier, so the new provider can serve traffic only via heuristic
> fallback or per-request override.
>
> Owner: Router team. Audience: one engineer shipping over ~2 weeks.
>
> **Companion docs:** `router/CLAUDE.md` (load-bearing layering rules —
> read first), `docs/plans/ROUTER_V1_PLAN.md` (telemetry foundation this
> plan piggybacks on for Phase 3 production-data calibration),
> `docs/plans/archive/CLUSTER_ROUTING_PLAN.md` (the AvengersPro baseline
> we're extending), `router/docs/eval/EVAL_RESULTS.md` (the quality bar
> this plan must not regress).
>
> **Relationship to ROUTER_V1_PLAN:** orthogonal. V1 owns cache-aware
> cross-tier scoring (a different cost-side correction) and per-cluster
> α calibration (a 1-D fit of the existing 2-axis blend). This plan adds
> a third axis to the blend itself. Both can ship under the multiversion
> router with no conflict — one publishes `v0.7-speed`, the other
> publishes `v0.7-cache`.

---

## 1. What we're building

A latency-aware extension to the cluster scorer that:

1. Treats per-(model) output throughput and TTFT as a third routing
   axis alongside quality and cost, baked into `rankings.json` at
   training time the same way α is.
2. Activates the Cerebras provider as a first-class routing target by
   giving the scorer a reason to pick a 2,000 tok/s Llama-3.3-70B over
   a 60 tok/s Claude Sonnet on speed-sensitive prompts.
3. Exposes a per-request preference vector via header so allowlisted
   callers can ask for "fast right now" without retraining the model.
4. Generalises the holdout-regret eval to a 3-axis Pareto-coverage
   methodology so promotions are evidence-gated the same way v0.5 →
   v0.6 was.

**Quality bar.** No regression on the 500-prompt judge-ensemble eval
(`router/eval/`) at the default trained weights. The eval harness is
the gate, not an abstract pp number.

**Cost bar.** Realised blended-cost-per-session **and**
median-time-to-first-token, measured on the holdout slice and (later)
on production telemetry. The headline isn't a single number — it's a
3D Pareto surface the eval harness must agree we extended.

**Speed bar.** Specific to coding-agent workloads: TTFT < 500 ms on
tool-call turns (where latency dominates wait time) and TPS >
500 tok/s on long-edit turns (where throughput dominates). These
thresholds are SLAs for the eval, not hard runtime gates.

---

## 2. State of the repo (May 3, 2026)

What's already done that this plan builds on:

| Surface | File / dir | What it does |
|---|---|---|
| Cluster scorer | `internal/router/cluster/scorer.go` | AvengersPro: embed → top-p centroids → uniform sum of ranking rows → argmax. Runtime is one scalar lookup per (cluster, model) — no α, no cost, no latency at request time. |
| 2-axis blend (training) | `scripts/train_cluster_router.py:484` `alpha_blend` | Single coefficient α: `score = α·q_norm + (1-α)·(1 - c_norm)`, both min-max normalised within the cluster. |
| Per-cluster α (v0.6) | `scripts/train_per_cluster_alpha.py` | 1-D grid sweep of α over `ALPHA_GRID = np.linspace(0, 1, 21)`; picks the α_k that minimises `mean_regret + λ_cost · mean_cost` on the train slice. K_clusters × 21 candidates ≈ ms-range. |
| Cost table | `train_cluster_router.py` `DEFAULT_COST_PER_1K_INPUT` | Flat per-model number, baked into `rankings.json` at training time. |
| Multiversion router | `internal/router/cluster/multiversion.go` | Per-bundle scorers (artifacts/v0.5, v0.6); `x-weave-cluster-version: v0.X` pins per request for allowlisted installations. The vehicle for shipping `v0.7-speed` alongside `v0.6` for head-to-head eval. |
| Cerebras provider | `internal/providers/cerebras/client.go` | OpenAI-compatible Chat Completions adapter, registered via `CEREBRAS_PROVIDER_API_KEY`. Strict tool calling enabled. Catalogue: `qwen-3-coder-480b`, `llama-3.3-70b`, `gpt-oss-120b`, `glm-4.7`. |
| Per-cluster cost penalty | `train_per_cluster_alpha.py:108` `DEFAULT_LAMBDA_COST` | Already a precedent for adding extra terms to the per-cluster fit objective; the latency term slots in the same way. |
| Eval harness | `router/eval/` | 500-prompt Modal judge ensemble; Pareto + per-router table. |

What does **not** exist that this plan needs:

- ❌ A per-(model) latency table analogous to `DEFAULT_COST_PER_1K_INPUT`.
- ❌ A 2-D simplex sweep in `train_per_cluster_alpha.py` (today's sweep
  is 1-D).
- ❌ A 3-axis blend function in `train_cluster_router.py`.
- ❌ A per-request preference header. `x-weave-cluster-version` is the
  closest precedent and the model to copy.
- ❌ A 3D Pareto-coverage evaluator. `holdout_eval.py` today reports
  scalar regret + cost; the third axis needs `ttft_ms` /
  `tps_tok_per_s` columns and an NSGA-II non-dominated-frontier
  computation.
- ❌ Per-(model, cluster) latency observations from production. Phase 0
  of `ROUTER_V1_PLAN.md` is wiring the OTel/observation table that
  would surface these for free; this plan's Phase 3 depends on that
  shipping.

---

## 3. Architecture additions (CLEAN-respecting)

Imports flow inward only — same rule as `router/CLAUDE.md`. The runtime
change is small enough that no new Go package is strictly required for
Phases 1–2; Phase 3 adds one pure package for the runtime preference
vector.

```
scripts/                                (training pipeline; offline only)
        + train_cluster_router.py             (extend: latency table; 3-axis alpha_blend)
        + train_per_cluster_alpha.py          (extend: 2-D simplex sweep over (α_q, α_c))
        + holdout_eval.py                     (extend: NSGA-II Pareto coverage; 3D AIQ-volume)
        + bench_walker.py                     (UNCHANGED — bench data shape unchanged)

internal/router/cluster/
        scorer.go                             (Phase 2 only: read triple from rankings.json,
                                              apply runtime weights from context)
        artifacts.go                          (extend: rankings.json schema versioning so
                                              v0.6 scalars and v0.7 triples coexist)

internal/router/preference/                   (NEW; Phase 2; pure types only)
        weights.go                            (Weights struct; FromContext / WithContext)

internal/server/middleware/
      + middleware/preference.go              (NEW; Phase 2; reads x-weave-speed-preference
                                              header, attaches preference.Weights to ctx)

internal/router/cluster/artifacts/v0.7-speed/ (NEW; Phase 1)
        centroids.bin                         (copied byte-for-byte from v0.6)
        model_registry.json                   (copied; may drop in Cerebras entries)
        rankings.json                         (NEW shape: per-(cluster, model) triples
                                              {q_norm, c_norm, l_norm} instead of scalar)
        metadata.yaml                         (records per-cluster (α_q, α_c, α_l) +
                                              latency table provenance)
```

Layering rules respected:
- `internal/router/preference/` is on the inner ring — pure types, no
  I/O. Mirrors how `evalswitch.ContextKey` is shaped today.
- The middleware lives in `internal/server/middleware/` next to
  `WithEvalRoutingOverride` and `WithClusterVersionOverride`, the exact
  pattern this is copied from.
- `cluster.Scorer` reads the preference from `ctx` if present, falls
  back to the per-cluster trained weights. No Postgres, no I/O on the
  request path.
- `rankings.json` schema version is bumped — `artifacts.go` decodes
  both shapes so v0.6 stays loadable. `centroidsMagic` does NOT bump
  (centroid format is unchanged).

---

## 4. Phased rollout

Three phases, ~2 weeks total if Phase 0 of `ROUTER_V1_PLAN.md` is
parked. Phases 1–2 are offline-only; Phase 3 depends on production
telemetry.

### Phase 1 (week 1, days 1–3) — 3-axis blend, vendor-sourced latency

**Goal.** Train `v0.7-speed` against a flat per-model latency table,
prove on the holdout slice that it beats `v0.6` when the user weights
latency > 0, and ties when latency weight = 0.

**Deliverables (file-level):**

1. `train_cluster_router.py` — add `DEFAULT_TPS_TOK_PER_SEC` and
   `DEFAULT_TTFT_MS` dicts mirroring `DEFAULT_COST_PER_1K_INPUT`.
   Sources: vendor pricing pages (Anthropic, OpenAI, Google) for the
   hosted models; Cerebras's published throughput table for OSS models;
   Artificial Analysis (LLMPerf-derived) as the cross-vendor
   tiebreaker. Recorded with the cite-date in the dict comment so the
   table is auditable.
2. `train_cluster_router.py` `alpha_blend` — extend signature to take
   `latency_per_1k_output_ms` and weights `(α_q, α_c, α_l)`. New body:

   ```python
   def alpha_blend_3axis(cell_means, cost_per_1k, latency_per_1k_out,
                         alpha_q, alpha_c, alpha_l, deployed_models):
       q_min, q_max = ...   # min-max within cluster, same as today
       c_min, c_max = ...   # cost
       l_min, l_max = ...   # latency (lower is better — flipped at use)
       blended = {}
       for m in deployed_models:
           q_norm = (cell_means[m] - q_min) / max(q_max - q_min, 1e-9)
           c_norm = (cost_per_1k[m] - c_min) / max(c_max - c_min, 1e-9)
           l_norm = (latency_per_1k_out[m] - l_min) / max(l_max - l_min, 1e-9)
           blended[m] = alpha_q * q_norm \
                      + alpha_c * (1 - c_norm) \
                      + alpha_l * (1 - l_norm)
       return blended
   ```

   Reduction property: when `α_l = 0` the function reduces exactly to
   today's `alpha_blend(α=α_q/(α_q+α_c))`. Unit-test invariant.

3. `train_per_cluster_alpha.py` — change `ALPHA_GRID` from 1-D
   `np.linspace(0, 1, 21)` to a 2-D simplex grid over `(α_q, α_c)` with
   `α_l = 1 - α_q - α_c`, filtering to `α_l ≥ 0`. ≈ 231 valid points
   per cluster (upper triangle of a 21 × 21 grid). Inner-loop work per
   point is unchanged; total fit cost is K × 231 ≈ 2,310 candidates
   for K=10 — still ms.
4. `train_per_cluster_alpha.py` — extend the per-cluster objective:

   ```python
   loss(α_q, α_c, α_l) = mean_regret(α_q, α_c, α_l)
                       + λ_cost · mean_cost_usd_per_prompt
                       + λ_latency · mean_latency_ms_per_prompt
   ```

   `λ_cost` keeps its current default (0.0 in v0.6; tunable). New
   `λ_latency` defaults to 0.0 for the parity check, then tuned via
   `--lambda-latency` once the parity baseline lands.
5. `train_per_cluster_alpha.py` — write a triple `{q_norm, c_norm,
   l_norm}` per (cluster, model) into `rankings.json` instead of a
   single scalar. Carry the per-cluster `(α_q_k, α_c_k, α_l_k)` in
   `metadata.yaml` for provenance and as the runtime default.
6. `internal/router/cluster/artifacts.go` — read both rankings shapes:
   `Rankings v1` (scalar, v0.1–v0.6) and `Rankings v2` (triple, v0.7+).
   Decoder picks shape from a top-level `"schema_version"` field that
   defaults to 1 when missing.
7. `internal/router/cluster/scorer.go` — for `Rankings v1`, runtime is
   unchanged. For `Rankings v2`, the per-cluster default weights from
   `metadata.yaml` collapse the triple to a scalar at boot — the
   request path stays a scalar lookup until Phase 2.
8. `train_per_cluster_alpha.py` — write a `v0.7-speed` artifact bundle
   into `internal/router/cluster/artifacts/v0.7-speed/`.
9. `holdout_eval.py` — extend the per-prompt cost-and-regret roll-up to
   also report `mean_latency_ms_per_prompt` per router version. Keep
   the existing 2D Pareto plot, add a 3D scatter (matplotlib) of
   (cost, regret, latency) per router for visual sanity.

**Acceptance for Phase 1:**

- v0.7-speed at `(α_q, α_c, α_l) = (0.6, 0.4, 0.0)` matches v0.6
  bench-holdout regret to within ±0.5 pp (the parity invariant).
- v0.7-speed at `(0.5, 0.3, 0.2)` shifts at least 15% of holdout
  traffic to Cerebras-hosted models on clusters where Cerebras has a
  competitive quality column, vs 0% for v0.6.
- `go test -tags no_onnx ./internal/router/cluster/...` passes (the
  schema-versioned decoder loads both shapes).
- `python holdout_eval.py --versions v0.6,v0.7-speed` runs and produces
  the new 3D scatter without crashing.

### Phase 2 (week 1, days 4–5; week 2, day 1) — Per-request preference vector

**Goal.** Allow allowlisted callers to override the trained per-cluster
weights at request time, mirroring the `x-weave-cluster-version`
mechanism.

**Deliverables:**

1. `internal/router/preference/weights.go` — pure types:

   ```go
   type Weights struct {
       Quality float32
       Cost    float32
       Latency float32  // weight on (1 - l_norm); higher = prefer fast
   }
   func WithContext(ctx context.Context, w Weights) context.Context
   func FromContext(ctx context.Context) (Weights, bool)
   ```

   Compile-time invariant: weights non-negative; package validates
   `Q+C+L > 0` to avoid all-zero.

2. `internal/server/middleware/preference.go` — `WithSpeedPreference`
   middleware that reads `x-weave-speed-preference: {quality|balanced|fast}`
   (or a JSON `{"q":0.4,"c":0.3,"l":0.3}` for the numeric form) from
   the same allowlisted-installation gate that `WithClusterVersionOverride`
   uses. Three named presets:

   | Preset | (α_q, α_c, α_l) |
   |---|---|
   | `quality` | (1.0, 0.0, 0.0) |
   | `balanced` | (0.5, 0.3, 0.2) |
   | `fast` | (0.2, 0.2, 0.6) |

   Numeric form skips the presets and uses the literal vector.

3. `internal/router/cluster/scorer.go` — when `preference.FromContext`
   returns weights and the loaded artifact is `Rankings v2`, compute
   per-cluster scores at request time as

   ```
   score(m, k) = w.Q · q_norm[k][m]
               + w.C · (1 - c_norm[k][m])
               + w.L · (1 - l_norm[k][m])
   ```

   summed over the top-p clusters. Scalar-product cost: ~3 floats ×
   models × top_p ≈ 100 μs, well under the existing `score_us` budget
   in the request log.

4. `cmd/router/main.go` — wire `middleware.WithSpeedPreference` into
   the authed gin group, after `WithClusterVersionOverride`.

5. `eval/types.py` + `eval/routing.py` — add `vX.Y-speed-fast` /
   `vX.Y-speed-balanced` / `vX.Y-speed-quality` synthetic router
   names that translate to header pairs on the staging deployment.
   No new Python Literal updates per the existing pattern (regex-based).

**Acceptance for Phase 2:**

- A request with `x-weave-speed-preference: fast` to staging routes to
  Cerebras-hosted models on at least 60% of the held-out coding-agent
  prompts; the same prompts with `quality` route to Anthropic or
  OpenAI frontier models.
- Latency overhead from the per-request weight application is ≤ 200 μs
  at p99 (measure via `score_us` in the existing log line).
- The default request path (no header) is byte-identical to today —
  the `Rankings v2` decoder uses the per-cluster trained weights when
  no context override is present.
- Eval harness side-by-side run of `v0.6 / v0.7-speed-quality /
  v0.7-speed-balanced / v0.7-speed-fast` lands in `EVAL_RESULTS.md`.

### Phase 3 (week 2, days 2–5; gated on ROUTER_V1_PLAN Phase 0) — Production-data calibration

**Goal.** Replace the vendor-published latency table with per-(model,
cluster) measured latency from production telemetry. Same loop the
v0.6 → v0.7 plan runs for cost recalibration; this is the latency
analogue.

**Deliverables (gated on `routing_observations` table existing —
ROUTER_V1_PLAN Phase 0):**

1. `db/queries/routing_observations.sql` — add `GetMeanLatencyByModelAndCluster`
   query returning per-(decision_model, cluster_id) median TTFT and
   p50/p95 TPS. The cluster_id column must be added to
   `routing_observations` as part of ROUTER_V1_PLAN Phase 0; if it
   is recorded only on the span and not in the row, raise it as a
   blocker for that phase.
2. `scripts/extract_production_latency.py` — pulls the query result
   into a `production_latency.json` keyed by `{cluster_k: {model_m:
   {ttft_ms, tps}}}`. Reads with `pgx`-equivalent (psycopg) against
   the staging proxy.
3. `train_per_cluster_alpha.py` — accept `--latency-source
   {vendor,production}` flag. `vendor` reads the static dict (Phase
   1); `production` reads `production_latency.json` and falls back to
   vendor for clusters with `n_observations < 50`.
4. Train `v0.8-speed-prod` from production observations, ship via
   multiversion alongside `v0.7-speed`. Eval gate runs the same
   judge-ensemble suite.
5. `metadata.yaml` for `v0.8-speed-prod` records the observation
   window (`obs_start`, `obs_end`, `n_observations_per_cluster`) so
   the bundle is reproducible from the raw observations.

**Acceptance for Phase 3:**

- v0.8-speed-prod's per-cluster `l_norm` differs from v0.7-speed's by
  ≥ 10% on at least 3 clusters (production latency disagrees with
  vendor numbers — expected, especially for long-context prompts).
- LLM-judge eval shows v0.8-speed-prod ≥ v0.7-speed at the trained
  default weights.
- `routing_observations` query plan stays under 100 ms p95 against
  the staging cluster (verify with `EXPLAIN ANALYZE`).

---

## 5. Score model (formal)

Two layers, matching how the existing α-blend is layered in the v0.6
artifact:

**Training-time per-cluster blend.** For cluster `k`, model `m`, with
per-cluster weights `(α_q_k, α_c_k, α_l_k)` chosen by the simplex
sweep:

```
LegacyScore(m, k) = α_q_k · q_norm_k[m]
                  + α_c_k · (1 - c_norm_k[m])
                  + α_l_k · (1 - l_norm_k[m])
```

This is the scalar baked into `rankings.json` (when the runtime
preference vector is absent) and the value summed across top-p
clusters at argmax time:

```
score(m, x) = sum over k in top_p(x) of LegacyScore(m, k)
```

**Runtime per-request override (Phase 2).** For caller-supplied
`(w.Q, w.C, w.L)`:

```
RuntimeScore(m, k, w) = w.Q · q_norm_k[m]
                      + w.C · (1 - c_norm_k[m])
                      + w.L · (1 - l_norm_k[m])
score(m, x, w) = sum over k in top_p(x) of RuntimeScore(m, k, w)
```

Reduction properties (unit-test invariants):

- `(w.Q, w.C, w.L) = (α_q_k, α_c_k, α_l_k)` for every k → runtime
  matches the trained default exactly.
- `w.L = 0` and `(w.Q, w.C) ∝ (α_v0.6, 1 - α_v0.6)` for every k →
  runtime matches v0.6 exactly.
- `w.L > 0` and Cerebras has the lowest latency in the cluster's
  candidate set → Cerebras gains positional ranking weight relative
  to the same blend without `w.L`.

**Normalisation.** Min-max per cluster, computed at training time,
identical shape to today's `alpha_blend` for the cost term — so the
same property holds: a cluster where every model has the same
latency contributes 0 to the latency term, regardless of `w.L`. This
is the right behaviour: latency only matters where it
discriminates.

---

## 6. Evaluation methodology

The bench-holdout regret eval (`holdout_eval.py`) ports cleanly to
3 axes; the LLM-judge eval gates promotions.

### Per-prompt outcomes

For each held-out prompt `p` and router `R` we record:

```
(quality(R, p), cost_usd(R, p), latency_ms(R, p))
```

`quality` = best-bench-column score for the chosen model.
`cost_usd` = `cost_per_1k_input · estimated_input_tokens` +
expected-output-cost (already implemented).
`latency_ms` = `ttft_ms + (tps · expected_output_tokens) / 1000` for
Phase 1; observed median for Phase 3.

### 3D Pareto coverage (the new gate)

Compute the non-dominated frontier of `(quality, -cost, -latency)`
across the union of routers and the oracle (per-prompt argmin
regret model). Report per router:

```
PC(R) = | {p : R(p) ∈ Pareto(p)} | / | held-out |
```

A router with `PC(R) ≥ 0.85` Pareto-covers most prompts. Comparison
shape: `PC(v0.6) vs PC(v0.7-speed)` at the trained default weights.

### Operating-point regret (the SLA gate)

For coding-agent SLAs:

| Workload | Constraint | Target metric |
|---|---|---|
| Tool-call turns | TTFT ≤ 500 ms | mean regret on `tool_call`-clustered prompts |
| Long edits | TPS ≥ 500 tok/s | mean regret on `long_edit`-clustered prompts |
| Default | none | mean regret across full holdout |

Each row is a constrained-max problem solved by filtering candidates
under the constraint and computing `oracle_quality - chosen_quality`.

### 3D AIQ-volume (the publishable contribution)

Sweep `(α_q, α_c, α_l)` over a 21 × 21 simplex grid; for each
grid point compute the (cost, quality, latency) the router achieves;
take the volume under the 3D Pareto surface relative to the
all-quality (1, 0, 0) corner. Generalises RouterBench's AIQ. Useful
as a single scalar for tracking over time, but not as the primary
gate — Pareto coverage and operating-point regret are the gates.

---

## 7. Risks and unknowns

- **Vendor-published latency disagrees with production.** Phase 1's
  flat per-model number ignores prompt-length and time-of-day effects.
  Mitigation: Phase 1 ships only as a candidate alongside v0.6 — never
  promoted to `latest` without LLM-judge confirmation. Phase 3 is the
  fix.
- **Cerebras quality scores are weak/missing.** OpenRouterBench has
  limited per-instance coverage of Llama-3.3-70B and Qwen-3-Coder-480B.
  The α-blend will under-pick Cerebras until proxy mappings improve.
  Mitigation: add Cerebras entries to `model_registry.json` as
  proxies (`proxy: true`) referencing the closest existing column;
  document the proxy_note. The cost/latency axes will pull traffic
  toward Cerebras on cost-sensitive and speed-sensitive clusters even
  with proxied quality.
- **Per-request preference is a load-bearing surface for misuse.**
  Customers requesting `fast` may accept regressed quality without
  realising it. Mitigation: keep the header gated on the
  eval-allowlist installation only for v0.7; promote to general
  availability only after Phase 3 calibration confirms that `fast`
  doesn't cliff quality on production-shaped prompts.
- **Schema-versioned `rankings.json` is a forward-compat hazard.** A
  future v0.8 that bumps to `Rankings v3` could ship before the
  runtime decoder is updated. Mitigation: `artifacts.go` rejects
  unknown `schema_version` at boot with a clear error message; CI
  test loads each committed artifact at every PR.
- **Latency normalisation flips intuition.** `(1 - l_norm)` rewards
  *low* latency, parallel to `(1 - c_norm)` rewarding low cost.
  Mitigation: explicit comment + named test case.

---

## 8. Telemetry plan

Phase 1–2 reuse the existing `router.decision` span; Phase 3 adds
columns to `routing_observations`. No new span kinds.

### Phase 1–2 span attributes (additive)

- `router.weights.quality` — float, the `w.Q` actually applied (either
  trained default or per-request override).
- `router.weights.cost` — float, `w.C`.
- `router.weights.latency` — float, `w.L`.
- `router.weights.source` — `"trained" | "header_preset" | "header_numeric"`.
- `router.candidate.l_norm` — recorded only on `router.decision` spans
  in eval/staging. Drop in production to keep span size bounded.

### Phase 3 routing_observations columns (new)

- `ttft_ms` — float, populated by the Anthropic/OpenAI/Google response
  reader when the first token byte lands. Already extractable from
  `otel.UsageExtractor` if the wall-clock-on-first-byte is added.
- `tps_observed` — float, `(output_tokens / wall_clock_seconds)` once
  the response stream closes.
- `cluster_id` — int (smallint), the top-1 cluster id at decision time.
  Required for the per-(model, cluster) latency aggregation in
  `extract_production_latency.py`.

### Decision-gate dashboards (Phase 1)

- 3D scatter (cost, regret, latency) per router on the holdout slice,
  rendered into `router/eval/results/v0.7-speed.html`.
- Per-cluster `(α_q, α_c, α_l)` distribution plotted in
  `metadata.yaml`-derived form, similar to v0.6's α-distribution
  histogram.

---

## 9. References

Literature reviewed (May 3, 2026 search). Closer-to-our-shape papers
first.

- **AvengersPro** (arxiv 2508.12631, Zhang et al., Aug 2025) — our
  baseline. 2-axis blend `α·perf + (1-α)·efficiency`. No latency. The
  cluster-and-rank primitive is what we extend.
- **Cascade Routing** (arxiv 2410.10347, Dekoninck/Baader/Vechev,
  ICLR'25) — the closest formal precedent. Lagrangian
  `τ_i(x, λ) = q̂_i(x) - λ·ĉ_i(x)`; explicitly notes "cost could
  measure either monetary cost or latency" and substitutes wall-clock
  for SWE-Bench experiments. The shape we are copying for the 3-axis
  blend.
- **RouterBench** (arxiv 2403.12031, Hu et al.) — canonical 405k-outcome
  eval set; defines AIQ as area under the convex hull. Latency is
  explicitly listed as future work, not in the dataset. We are the
  3-axis extension RouterBench flagged.
- **RouteLLM** (arxiv 2406.18665, Ong et al.) — per-request α
  threshold, runtime-supplied. Established that the runtime knob
  doesn't require retraining. Phase 2 is the N-D generalisation of
  this primitive.
- **Hybrid LLM** (arxiv 2404.14618, Ding et al., ICLR'24) — per-query
  threshold dial; router predicts P(small ≈ large). Cited primitive
  for runtime user choice.
- **Cloud-Edge 3-axis Routing** (arxiv 2507.15553, Jul 2025) — explicit
  (quality, response-time, cost) NSGA-II Pareto sorting. Closest
  published methodology for the evaluation we'll run.
- **xRouter** (arxiv 2510.08439, Oct 2025) — RL-trained orchestrator
  that explicitly considers capability + latency + price. Newest art
  in the 3-axis space. Reports Pareto operating points.
- **Dynamic Quality-Latency Aware Routing** (arxiv 2508.11291, Bao
  et al., Aug 2025) — edge/device, but the only paper that names
  "quality-latency aware" in the title. Uses single weighted cost,
  not Pareto.
- **Mahmood, Routing/Cascades/User Choice** (arxiv 2602.09902) —
  Stackelberg game where user utility = `quality - delay`; latency
  enters via re-prompt/abandon dynamics. Theoretical justification
  for treating latency as a first-class user-utility term.
- **LLMPerf** (open-source benchmark library, no arxiv) — de-facto
  industry standard for TTFT/TPS measurement; what Artificial Analysis
  uses. Phase 3's production telemetry is the LLMPerf approach
  applied to our actual traffic.

Per-(model, prompt) latency dataset of academic record: **none
exists.** RouterBench explicitly omits it; Cascade Routing measured
wall-clock during their own eval runs and used that as the cost
metric. Phase 1 sources from vendor-published numbers; Phase 3
generates the dataset from our production traffic.

---

## 10. Acceptance checklist (per phase)

Each phase merges only when its checklist clears.

### Phase 1

- [ ] `train_cluster_router.py` `DEFAULT_TPS_TOK_PER_SEC` and
      `DEFAULT_TTFT_MS` populated for every model in
      `DEFAULT_COST_PER_1K_INPUT`. Each entry carries a
      `# source: <vendor or LLMPerf>` cite-comment with the date.
- [ ] `alpha_blend_3axis` unit test asserts `α_l = 0` reduces to today's
      `alpha_blend(α=α_q/(α_q+α_c))` byte-identically.
- [ ] `train_per_cluster_alpha.py --lambda-latency 0.0 --version
      v0.7-parity --no-promote` produces an artifact whose holdout
      regret matches v0.6 to within ±0.5 pp.
- [ ] `train_per_cluster_alpha.py --lambda-latency 0.05 --version
      v0.7-speed --no-promote` produces an artifact where ≥ 15% of
      holdout traffic shifts to a different model vs v0.6.
- [ ] `internal/router/cluster/artifacts.go` `Rankings v2` decoder
      passes a round-trip test against both v0.6 and v0.7-speed
      bundles.
- [ ] `go test -tags no_onnx ./internal/router/cluster/...` passes.
- [ ] `holdout_eval.py --versions v0.6,v0.7-speed` produces a 3D
      scatter and a Pareto-coverage table.

### Phase 2

- [ ] `internal/router/preference/` package compiles with no I/O
      imports; pure types only.
- [ ] `WithSpeedPreference` middleware reads the header only from
      allowlisted installations; integration test asserts
      non-allowlisted requests get the trained default.
- [ ] `cluster.Scorer.Route` applies the runtime weights; per-request
      `score_us` overhead ≤ 200 μs at p99.
- [ ] Eval harness side-by-side run of v0.6 / v0.7-speed-{quality,
      balanced, fast} lands in `EVAL_RESULTS.md`.
- [ ] `cmd/router/main.go` wires the middleware after
      `WithClusterVersionOverride`; documented in `router/README.md`.

### Phase 3 (gated on ROUTER_V1_PLAN Phase 0)

- [ ] `routing_observations.cluster_id`, `ttft_ms`, `tps_observed`
      columns shipped (migration in ROUTER_V1_PLAN's Phase 0; this
      plan tracks the dependency).
- [ ] `extract_production_latency.py` runs against staging and
      produces `production_latency.json` with ≥ 50 observations per
      (cluster, model) for the top-5 traffic clusters.
- [ ] v0.8-speed-prod artifact bundle produced and ship-eligible
      (LLM-judge eval green).
- [ ] `metadata.yaml` records the observation window and per-cluster
      sample sizes.

---

## 11. What we explicitly are NOT doing in this plan

- **Per-prompt latency prediction.** The literature treats latency as
  per-(model, cluster) constant; per-prompt prediction is an open
  research problem. We follow the consensus: cluster-level only.
- **Reinforcement learning over the preference vector.** xRouter
  (arxiv 2510.08439) is the closest precedent; it requires reward
  modelling we don't have infrastructure for. Defer indefinitely.
- **Latency-aware cache decisions.** Phase 1 of `ROUTER_V1_PLAN.md`
  owns cache-aware scoring; the two plans compose at the rankings
  layer, not at the cost-model layer. Don't conflate them.
- **Auto-detection of "fast" intent from the prompt.** The agentic-
  intent-detection literature (Arch-Router etc.) is interesting but
  out of scope; for v0.7 the user explicitly opts in via header.
- **Cerebras-specific preferential routing.** Latency is the routing
  axis, not provider identity. If a future provider (Groq, SambaNova)
  matches Cerebras's TPS the scorer should pick it on the same merits;
  hard-coding Cerebras would break that property.
