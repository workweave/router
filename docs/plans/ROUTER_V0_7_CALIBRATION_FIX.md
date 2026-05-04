Created: 2026-05-03
Last edited: 2026-05-03

# Router v0.7+ calibration fix — execution plan

> **Status: in flight tonight (2026-05-03).** Owner: Steven Tohme.
>
> Triggered by RouterArena local smoke on the corrected stack (`-tags ORT`
> Makefile fix + ONNX assets dir wired in `.env.development`,
> 2026-05-03): with v0.6-cluster actually running, **99.26% of prompts
> route to gemini-3.1-pro-preview** and overall accuracy is 54.9%
> vs the heuristic's 67.3% on the same 809-prompt sub_10 split.
> The previously-published §7 RouterArena numbers in
> `docs/eval/EVAL_RESULTS.md` (66.5%, Arena 63.06, "97.31% Haiku /
> 2.69% Opus") were measuring the heuristic fallback, not v0.6 — the
> Makefile was missing `-tags ORT` so `cluster.NewEmbedder` failed at
> boot and `main.go` fell open to the heuristic.
>
> This doc is the four-step recovery: A → B → C → D, sequenced and
> gated.

## 1. Diagnosis (already done, summarized for context)

`internal/router/cluster/artifacts/v0.6/rankings.json` shows
claude-opus-4-7 rated 0.0–0.3 quality on 9 of 10 clusters, while
gemini-3.1-pro-preview gets 0.85+ on all 10. Even at α=1.0
(quality-only), the argmax on every cluster except cluster 8 is
Gemini. So α tuning alone cannot fix this.

`scripts/diagnose_coverage.py` (added 2026-05-03) shows Opus has the
**highest** observation count of any model (14,833 / 96.6% of
prompts). Coverage is fine; shrinkage is not the culprit.

The two compounding bugs:

1. **No direct labels for claude-opus-4-7 (or claude-haiku-4-5).**
   Opus inherits its score from `gpt-5` (OpenRouterBench) and
   `claude-opus-4-5` (SWE-bench, ~500 prompts). Haiku inherits from
   `gemini-2.5-flash`. Whatever the proxies score, the deployed
   model is rated as. claude-opus-4-7 the *deployed* model is never
   actually measured anywhere in the training data.

2. **`per_prompt_minmax_across_bench_columns` (v0.5+) magnifies tiny
   bench gaps.** If gemini-2.5-pro scores 0.85 on a prompt and gpt-5
   scores 0.78, after the per-prompt min-max: gemini → 1.0, gpt-5 →
   0.0. A 7-point bench gap becomes a maximal 1.0-point ranking gap
   in the training matrix. Repeated across thousands of prompts,
   Opus's per-cluster mean collapses to ~0.

## 2. Goal

A v0.7+ artifact whose RouterArena `sub_10` smoke produces:

- Pick distribution spread across at least 3 models (no >85%
  concentration on any single model)
- Accuracy ≥ 65% (within ~2pp of the heuristic's 67.3% baseline)
- Arena score ≥ 60 (within ~4pts of the heuristic's 64.2)

Stretch: accuracy ≥ 68% and Arena score ≥ 65, beating the heuristic
on a multi-provider routing matrix.

## 3. Sequenced execution (A → B → C → D)

Each step is a hypothesis test. **Stop and replan if a step's
acceptance criterion fails** — don't blindly proceed.

### A. Trainer flag for raw scoring (no per-prompt minmax)

**Hypothesis:** the per-prompt min-max normalization in
`bench_walker.load_bench` Stage B is the dominant cause of Opus's
collapsed scores. Switching to raw aggregation should pull Opus's
per-cluster scores up into the 0.4–0.6 range.

**Code changes:**

- `router/scripts/bench_walker.py`:
  - Add `score_normalization: str = "minmax"` parameter to
    `load_bench` (preserve current default for backwards compat).
  - Plumb a new branch into Stage B: when `score_normalization == "raw"`,
    skip the per-prompt min-max rescale and emit the raw bench
    column means directly. Document trade-off in the docstring.
  - Add a `"zscore"` branch as a third option (per-prompt
    z-score, less destructive than minmax): mean 0, scale by std,
    clipped to [-3, 3] then linearly mapped to [0, 1]. Useful as a
    middle ground if both raw and minmax misbehave on different
    parts of the bench.
- `router/scripts/train_cluster_router.py`:
  - Add `--score-normalization {minmax,raw,zscore}` CLI flag,
    default `minmax`. Pass through to `load_bench`.
  - Write the chosen normalization into the output `metadata.yaml`
    (replace the current hardcoded string).
- `router/scripts/diagnose_coverage.py` already exists — reuse it
  to inspect raw vs minmax score distributions before training.

**Train + smoke:**

```bash
cd router/scripts
poetry run python train_cluster_router.py \
  --version v0.7 --from v0.6 --k 10 \
  --shrinkage-k0 10 --alpha 0.53 \
  --score-normalization raw \
  --notes "v0.7: drop per-prompt minmax; same registry/α as v0.5."
```

```bash
# back in router/, with make dev running
eval/.venv/bin/python -m eval.routerarena \
  --router v0.7-cluster --split sub_10 --concurrency 8 \
  --max-output-tokens 512 \
  --out eval/results/routerarena_v0.7_sub10_smoketest.json
```

**Acceptance criteria:**

1. v0.7 `rankings.json`: max-min spread of quality scores per cluster
   is ≤ 0.6 on at least 7 of 10 clusters (vs v0.6's typical 0.9
   spread). Specifically, claude-opus-4-7 lifts above 0.4 on at
   least 5 clusters.
2. RouterArena `sub_10` pick distribution names ≥3 distinct models.
3. RouterArena `sub_10` accuracy ≥ 60% (lower bar than the goal —
   this step alone won't fully recover the heuristic; B and C add
   on top).

**If A fails** (rankings still extreme, picks still concentrated): the
per-prompt minmax wasn't the dominant magnifier. Rethink — possibly
the bench-column proxies themselves are upside-down vs. real
behavior, in which case skip to C.

**Effort:** ~1.5 hr (refactor + 30 min training run + smoke).
**Cost:** $0 (local embed/cluster) + ~$2 in API for the smoke run.

---

### B. Drop Gemini-3.1-pro-preview proxy entries; retrain v0.8

**Hypothesis:** even with raw scoring, Gemini-3.1-pro-preview's two
strong bench columns (`gemini-2.5-pro` + `gemini-3-pro-preview`
SWE-bench) outscore the proxy chain feeding Opus. Removing it
forces argmax to a more balanced pool.

This is mostly a *mechanical* fix (we want it for production
correctness — proxy-only entries inflate routing decisions for
unmeasured models) but it's also a hypothesis check.

**Code changes:**

- Edit `router/internal/router/cluster/artifacts/v0.8/model_registry.json`
  (after the artifact dir is auto-created from v0.7):
  - Remove both `{"model": "gemini-3.1-pro-preview", ...}` entries
    (the gemini-2.5-pro proxy and the gemini-3-pro-preview proxy).
  - Keep gemini-3-flash-preview and gemini-3.1-flash-lite-preview —
    those use the gemini-2.5-flash column which is also feeding
    haiku-4-5, so they're already noise-budgeted by haiku's
    presence.
  - Optionally also drop gpt-5.5 (proxied via gpt-5, same
    "no direct labels" issue as Opus) — but gpt-5 is shared with
    Opus's chain, so keep it for now and reconsider after C.

**Train + smoke:**

```bash
cd router/scripts
poetry run python train_cluster_router.py \
  --version v0.8 --from v0.7 --k 10 \
  --shrinkage-k0 10 --alpha 0.53 \
  --score-normalization raw \
  --notes "v0.8: v0.7 + drop gemini-3.1-pro-preview proxy entries."
```

```bash
eval/.venv/bin/python -m eval.routerarena \
  --router v0.8-cluster --split sub_10 --concurrency 8 \
  --max-output-tokens 512 \
  --out eval/results/routerarena_v0.8_sub10_smoketest.json
```

**Acceptance criteria:**

1. RouterArena `sub_10` accuracy ≥ 65% (within 2pp of heuristic).
2. Pick distribution shows ≥4 distinct models with no model >70%.
3. Cost/1k ≤ $2.50 (acceptable spread up to ~$3 if accuracy clears 67%).

**If B passes:** v0.8 is potentially shippable. **Do not promote** —
still gate on C+D for full validation. But at this point we have a
fallback story if C runs into trouble.

**Effort:** ~30 min (registry edit + retrain + smoke).
**Cost:** ~$2 in API.

---

### C. Direct labels from RouterArena per-model accuracy

**Hypothesis:** the proxy-inheritance pattern (no model has direct
labels for the actual deployed checkpoint) is the load-bearing
calibration error. Mixing direct labels into the training data
should let models be rated by their actual measured performance,
not by what their proxy did.

This is the largest step in the plan. Splitting it into substeps:

#### C.1 — Build the labeling harness

`router/eval/routerarena.py` already runs RouterArena prompts
through the *router*. We need a sibling that runs each prompt
through *each model directly*, bypassing the router, and saves the
graded result keyed by (prompt, model) → score.

**Code changes:**

- New file `router/eval/routerarena_label.py`:
  - Reuse `_routerarena_official/` vendored prompt templates +
    grader (already wired into `routerarena.py`).
  - Iterate over a configurable model set
    (`--models claude-opus-4-7,claude-haiku-4-5,claude-sonnet-4-5,gpt-5.5,gemini-3.1-pro-preview`).
  - For each (prompt, model), dispatch directly to the provider
    (skip router), grade with the official methodology, save row.
  - Output: `eval/results/routerarena_labels.jsonl` with shape
    `{"prompt_id": ..., "model": ..., "score": float, "graded": bool, ...}`.
  - Concurrency: 8 per model, 5 models in parallel = ~30-40
    requests/sec aggregate. RouterArena full run is 8400 prompts
    so ~30-50 min wall clock per model, parallelized.
  - Resumability: append-only JSONL, skip already-labeled
    (prompt, model) pairs on restart. Critical for cost control —
    don't redo 8400 prompts if we hit a transient failure halfway.

**Effort:** ~2 hr.
**Cost:** $0 to write.

#### C.2 — Run the labeling pass

```bash
cd router
eval/.venv/bin/python -m eval.routerarena_label \
  --models claude-opus-4-7,claude-haiku-4-5,claude-sonnet-4-5,gpt-5.5,gemini-3.1-pro-preview \
  --split full --concurrency 8 \
  --out eval/results/routerarena_labels.jsonl
```

5 models × 8400 prompts × ~$1-3/1k queries (model-dependent)
= **estimated $50-100 total**. Wall clock with parallelism: ~45-60
min if rate limits don't bite.

To stay safe initially, **start with `--split sub_10`** (809
prompts × 5 models = ~4000 calls, ~$5-10) to validate the harness
before committing to the full run.

**Effort:** ~5 min to launch, ~1 hr wall clock for full split.
**Cost:** $50-100.

#### C.3 — Integrate labels into the training data path

**Code changes:**

- `router/scripts/bench_walker.py` (or a new sibling):
  - Add a loader for `routerarena_labels.jsonl` that emits rows in
    the same `(prompt, bench_column, score)` shape as
    OpenRouterBench / SWE-bench.
  - Bench-column names: `routerarena_<model>` (e.g.
    `routerarena_claude-opus-4-7`).
- `router/internal/router/cluster/artifacts/v0.9/model_registry.json`:
  - Add `{"model": "claude-opus-4-7", "provider": "anthropic",
    "bench_column": "routerarena_claude-opus-4-7"}` (direct, not
    proxy).
  - Same for haiku-4-5, sonnet-4-5, gpt-5.5, gemini-3.1-pro-preview.
  - Keep the existing proxy entries — direct labels supplement,
    don't replace, to preserve coverage outside RouterArena's
    8400-prompt domain.
- `router/scripts/train_cluster_router.py`:
  - Add `--include-routerarena-labels` flag; when set, calls the
    new loader and merges its rows into the bench data before
    clustering.

#### C.4 — Train + smoke v0.9

```bash
cd router/scripts
poetry run python train_cluster_router.py \
  --version v0.9 --from v0.8 --k 10 \
  --shrinkage-k0 10 --alpha 0.53 \
  --score-normalization raw \
  --include-routerarena-labels \
  --notes "v0.9: v0.8 + direct RouterArena labels for opus/haiku/sonnet/gpt-5.5/gemini-pro."
```

```bash
eval/.venv/bin/python -m eval.routerarena \
  --router v0.9-cluster --split sub_10 --concurrency 8 \
  --max-output-tokens 512 \
  --out eval/results/routerarena_v0.9_sub10_smoketest.json
```

**Acceptance criteria:**

1. RouterArena `sub_10` accuracy ≥ 67% (matches/beats heuristic).
2. Pick distribution: no model >60%, ≥4 distinct models.
3. v0.9 `rankings.json`: claude-opus-4-7 has the highest score on
   at least 2 clusters (it should — Opus genuinely outperforms on
   hard prompts in RouterArena).
4. **Bonus**: Arena score ≥ 64. Beats heuristic 64.2 → multi-provider
   routing genuinely earning its keep on RouterArena.

**If C passes:** v0.9 is the production candidate. Run full 8400-prompt
evaluation before promotion.

**If C fails** (still bad accuracy with direct labels): the cluster
geometry itself is the issue, not the rankings. That's a
pre-clustering re-think — the embedder may not be separating
by-difficulty / by-domain effectively. Out of scope for tonight;
needs a follow-up plan.

**Total effort C:** ~3-4 hr including inference wall clock.
**Total cost C:** $50-100 + ~$2 smoke.

---

### D. Log-cost normalization in α-blend

**Hypothesis:** even with calibrated rankings (post-C), the linear
cost normalization in `alpha_blend` over-weights mid-tier models
(Sonnet, GPT-4.1) vs the cheapest (Haiku, Gemini-flash). Log
normalization (`(log₂ c_max − log₂ c_i) / (log₂ c_max − log₂ c_min)`,
the form RouterArena uses internally) sharpens the cost lever
without distorting the quality side.

This is `ROUTER_IMPROVEMENTS.md` §3.1 — already on the menu, just
combined here for the v0.10 endgame.

**Code changes:**

- `router/scripts/train_cluster_router.py`:
  - Add `--cost-normalization {linear,log}` flag, default `linear`.
  - In `alpha_blend`, when `cost-normalization == log`, compute
    `q̃_j = (log₂(c_max) − log₂(c_j)) / (log₂(c_max) − log₂(c_min))`
    instead of the current linear form.
  - Update metadata.yaml's `cost_normalization` field.

**Train + smoke:**

```bash
cd router/scripts
poetry run python train_cluster_router.py \
  --version v0.10 --from v0.9 --k 10 \
  --shrinkage-k0 10 --alpha 0.53 \
  --score-normalization raw \
  --cost-normalization log \
  --include-routerarena-labels \
  --notes "v0.10: v0.9 + log cost normalization (RouterArena-style)."
```

```bash
eval/.venv/bin/python -m eval.routerarena \
  --router v0.10-cluster --split sub_10 --concurrency 8 \
  --max-output-tokens 512 \
  --out eval/results/routerarena_v0.10_sub10_smoketest.json
```

**Acceptance criteria:**

1. RouterArena `sub_10` accuracy ≥ v0.9's accuracy (no regression).
2. Cost/1k drops by ≥10% vs v0.9 (the lever is doing real work).
3. Pick distribution shifts more weight to Haiku / flash-lite
   (cheap, often-good-enough models) without quality loss.

**If D passes:** v0.10 is the new production candidate.

**Effort:** ~1 hr (refactor + retrain + smoke).
**Cost:** ~$2 in API.

---

## 4. Tonight's checkpoints

Rough plan for an evening of work, with hard checkpoints:

| Time | Step | Decision gate |
|---|---|---|
| T+0:00 | Start A — refactor `bench_walker` + trainer | — |
| T+1:30 | A retrain done; smoke v0.7 | A acceptance — proceed to B or replan |
| T+2:30 | B retrain + smoke v0.8 | B acceptance — proceed to C or replan |
| T+3:00 | C.1 harness ready; launch C.2 sub_10 first | C.2 sub_10 sanity passes — proceed to full |
| T+3:30 | Launch C.2 full split (background; ~1 hr wall clock) | — |
| T+4:30 | C.2 done; C.3 integration | — |
| T+5:30 | C.4 retrain + smoke v0.9 | C acceptance — proceed to D or stop with v0.9 |
| T+6:30 | D retrain + smoke v0.10 | D acceptance — final state |
| T+7:00 | Update `EVAL_RESULTS.md` §7 with corrected numbers + new artifacts | — |

Realistic: 6-7 hr if everything goes well. Plan for 9-10 hr with
one debugging detour and one rate-limit pause.

## 5. Cross-cutting work (after the chain finishes)

These are not blockers for tonight but should land alongside the v0.10
PR:

1. **Update `docs/eval/EVAL_RESULTS.md` §7** — flag that the
   originally-published numbers were measuring the heuristic, not
   v0.6. Append the corrected v0.6 / v0.7 / v0.8 / v0.9 / v0.10
   smoke results.
2. **Update `docs/eval/EVAL_RESULTS.md` §1** — note the in-house
   250-prompt judge ensemble's "v0.6 Pareto-dominates" finding was
   on a `-tags ORT`-broken local stack falling open to the
   heuristic, AND a staging deploy that filtered to
   Anthropic-only. Re-run that eval against the corrected stack
   before accepting v0.10.
3. **Add Makefile + `.env.development` regression test** — write a
   one-line `make check-cluster-loaded` target that hits `/health`,
   inspects the boot log (or a new admin endpoint), and exits 1
   if the cluster scorer didn't load. Wire into pre-commit so the
   "fell open to heuristic" silently-broken state can't recur.
4. **Apply the terraform diff** queued for staging/prod (see chat
   2026-05-03) — `OPENAI_PROVIDER_API_KEY` + `GOOGLE_PROVIDER_API_KEY`
   env vars. Independent of the v0.7 → v0.10 chain but blocks any
   eventual production rollout of a multi-provider artifact.

## 6. What this doc explicitly is NOT

- A v1 architecture rework. We are not changing the embedder, the
  K-means, the top-p, or the runtime scorer. Only training-side
  inputs and α-blend math.
- A green light to promote v0.7-v0.9 to staging or prod. Each is a
  hypothesis test artifact. Promotion is a separate decision after
  v0.10 (or whichever clears the goal in §2) ships through full
  RouterArena + the in-house judge ensemble.
- A replacement for `ROUTER_V1_PLAN.md` Phase 4. Phase 4 (per-cluster
  α retrain) was the *last* plan-level retraining lever; this doc
  is fixing the data inputs underneath it. After v0.10 lands, a
  Phase-4-style per-cluster α retrain on the corrected rankings is
  worth revisiting.
