Created: 2026-05-03
Last edited: 2026-05-03

# Router v0.7+ calibration — decision log

> Working log paired with
> [`ROUTER_V0_7_CALIBRATION_FIX.md`](ROUTER_V0_7_CALIBRATION_FIX.md).
> Updated as we make non-trivial decisions during execution. Each
> entry: what we decided, why, and what we learned/changed as a
> result. Newest at the bottom.

## 2026-05-03

### D1 — Land the trainer flag for raw scoring (Step A code)

- **What.** Added `--score-normalization {minmax,raw,zscore}` to
  `train_cluster_router.py`; plumbed through `bench_walker.load_bench`.
  Default stays `minmax` for back-compat. Replaced two hardcoded
  metadata strings with a `_normalization_label` helper.
- **Why.** Test the hypothesis that v0.6's per-prompt minmax was the
  dominant magnifier of Opus's collapsed scores.
- **Files.** `router/scripts/bench_walker.py` (lines 83–117 docstring;
  159–217 three-branch Stage B). `router/scripts/train_cluster_router.py`
  (helper near `next_version`; CLI flag near `--shrinkage-k0`; load_bench
  call site; two metadata sites).

### D2 — Train v0.7 with `--score-normalization raw --no-promote`

- **Command.** `train_cluster_router.py --version v0.7 --from v0.6
  --k 10 --shrinkage-k0 10 --alpha 0.53 --score-normalization raw
  --no-promote`. `--no-promote` keeps `latest` at v0.6 so customer
  traffic stays on the prior artifact while we evaluate.
- **Outcome.** v0.7 written: 15,354 unique prompts, 78/80
  cluster-model cells observed (only 2 backfilled), inertia 7253.
  metadata.yaml records `score_normalization: raw_bench_column_means`.
  `latest` correctly stayed at v0.6.

### D3 — v0.7 rankings analysis: Step A passed criterion 1 in spirit, failed in practice

- **What.** Static argmax over `rankings.json` (runtime is argmax,
  per `scorer.go:274`). Full 8-model set: v0.7 wins distribution is
  `gpt-4.1: 8, gpt-5.5: 2`. Concentration shifted from
  gemini-3.1-pro-preview (v0.6) to **gpt-4.1** (v0.7) — different
  model, same single-winner problem.
- **Decomposing the α-blend** for gpt-4.1 cluster 0:
  `x = 0.53·1.000 (q_norm) + 0.47·0.873 (1-c_norm) = 0.940`.
  vs Opus on the same cluster:
  `x = 0.53·0.498 + 0.47·0.000 = 0.264`. Opus loses on **both axes**.

### D4 — Diagnose the gpt-4.1 dominance: bench coverage bias

- **Smoking gun (per-column raw means + provenance):**

  | Column | n prompts | mean | benchmarks |
  |---|---|---|---|
  | **gpt-4.1** | 2,599 | **0.672** | AIME, GPQA, HumanEval, LiveCodeBench, LiveMathBench, MMLU-Pro (6 hard reasoning/code only) |
  | gpt-5 | 17,668 | 0.544 | full 37-bench mix incl. SWE-bench, arenahard, BFCL, SimpleQA, HLE |
  | claude-sonnet-4 | 21,571 | 0.487 | full 18-bench mix |
  | gemini-2.5-pro | 15,597 | 0.597 | full 17-bench mix |
  | gemini-2.5-flash | 16,695 | 0.408 | full 36-bench mix |

- **Root cause.** gpt-4.1 was only benched on a curated subset of
  hard reasoning/coding tasks. Its mean of 0.672 isn't "gpt-4.1 is
  25% better than gpt-5" — it's "gpt-4.1 never got its average
  dragged down by easier/binary prompts because it wasn't tested
  on them." Per-cluster, this manifests as systematically high
  q_norm for gpt-4.1.
- **Implication.** v0.6's per-prompt minmax was inadvertently fixing
  the column-coverage bias by rescaling per prompt to "rank among
  columns that ran this prompt." Step A's `raw` mode killed the
  magnification but re-exposed the underlying coverage bias.
  Per-prompt zscore (already implemented in D1) is a cleaner middle
  ground — kills coverage bias without magnifying tiny gaps.

### D5 — v0.8 design: drop gemini-2.5-pro entry + zscore normalization

- **What.** v0.8 = v0.7 minus the `gemini-3.1-pro-preview` entry
  pointing at `gemini-2.5-pro` (12,848 prompts), keeping the entry
  pointing at `gemini-3-pro-preview` (1,406 prompts, SWE-bench
  preview-vs-preview). Train with `--score-normalization zscore`
  instead of `raw`.
- **Why drop gemini-2.5-pro.** Coverage drops 9x. With
  `shrinkage_k0=10`, most per-cluster cells flip from
  well-observed to shrinkage-dominated (pulled toward global prior).
  Targeted way to dethrone gemini-3.1-pro-preview without removing
  it as a routing target.
- **Why zscore over raw.** Per-prompt z-score scales by per-prompt
  spread, so column-bias gets normalized out (gpt-4.1's elevated
  baseline rescales to ~0 std on every prompt) without minmax's
  pathological "7-point gap → maximal 1.0 rank gap" magnification.
- **Caveat.** zscore alone might not fully suppress gpt-4.1
  (cost-side bonus survives). If v0.8 still concentrates, follow-ups
  are: log-cost normalization (Step D), drop gpt-4.1 entry, or
  Step C direct labels.

### D6 — Train v0.8 (zscore + drop gemini-2.5-pro entry)

- **Command.** `train_cluster_router.py --version v0.8 --k 10
  --shrinkage-k0 10 --alpha 0.53 --score-normalization zscore
  --no-promote` (no `--from`; registry pre-placed at v0.8/).
- **Outcome.** 88,797 score rows (vs v0.7's 101,645 — the 13k
  reduction is the dropped gemini-2.5-pro coverage). 78/80 cells
  observed, inertia 7253 (deterministic K-means, identical to v0.7).

### D7 — v0.8 rankings analysis: significant progress, new dominator

- **Spread:** v0.8 has spread ≤ 0.6 on **9/10** clusters (vs v0.7's
  3/10 and v0.6's 5/10). Tighter clusters mean less single-winner
  pressure.

- **Argmax winners (top-1, full 8 models):**
  - v0.6: `{gpt-5.5: 5, gemini-3.1-pro-preview: 4, opus: 1}` — 3 winners
  - v0.7: `{gpt-4.1: 8, gpt-5.5: 2}` — 2 winners
  - v0.8: `{gpt-5.5: 8, gemini-3.1-pro-preview: 1, sonnet-4-5: 1}` — 3 winners

- **Top-p=4 sum (closer to runtime behavior):** v0.8's leader margin
  is tighter than v0.7's: gpt-5.5 (7.94) vs gemini-3.1-pro (6.75)
  vs gemini-flash-lite (5.77) vs gemini-flash (5.65) vs haiku (5.55).
  Top-5 range compressed to 2.4pts (vs v0.7's 3.4pts).

- **What worked:**
  - **gpt-4.1 neutralized.** v0.7 blended 0.940 across 8 clusters →
    v0.8 blended 0.41–0.64. zscore wiped out gpt-4.1's coverage-bias
    advantage exactly as predicted.
  - **Opus stabilized.** v0.6 had 0.0–1.0 swings; v0.7 had 0.0–0.53;
    v0.8 has Opus consistently at ~0.50 across all clusters.
    Per-prompt zscore restored its q_norm to "above average for the
    columns that ran this prompt" rather than the suppressed raw
    scoring.
  - **gemini-3.1-pro-preview narrowed.** v0.6's flat 0.83–0.90 band
    became 0.41–0.92 in v0.8 — same model, but per-cluster
    differentiation. Coverage drop forced the shrinkage prior to
    pull some cells toward the global mean.

- **What didn't.** **gpt-5.5 emerged as the new top concentration.**
  Wins 8/10 clusters at a suspiciously flat 0.845. Its proxy is
  gpt-5 (14,660 prompts, broad coverage), so under zscore it stays
  competitive across many clusters. The plan's Step B explicitly
  flagged this as a follow-up: "Optionally also drop gpt-5.5
  (proxied via gpt-5, same 'no direct labels' issue as Opus)."

- **Decision points after smoke:**
  - If smoke shows v0.8 ≥ heuristic: ship-quality progress; consider
    follow-ups in priority order (gpt-5.5 trim → log-cost → Step C).
  - If smoke is concentrated on gpt-5.5 with weak accuracy: drop
    gpt-5.5 entry next (analogous registry trim) and retrain v0.9.

### D8 — Smoked v0.7 and v0.8 against local make dev

- **Setup.** `eval/.env` sets `ROUTER_BASE_URL=http://localhost:8082`,
  so smokes hit local make dev (verified by sending the same prompt
  with `x-weave-cluster-version` v0.6/v0.7/v0.8 and getting three
  different model decisions, ruling out the staging-fallback
  hypothesis).
- **Results (sub_10, 809 prompts):**

  | metric | v0.6 (full split, prior smoke) | v0.7 sub_10 | v0.8 sub_10 |
  |---|---|---|---|
  | accuracy | 0.688 | **0.559** | **0.773** |
  | arena score | — | 55.1 | **65.1** |
  | pick distribution | haiku-heavy (heuristic-fallback era) | 100% gemini-3.1-pro-preview | 99% opus-4-7, 0.7% sonnet-4-5 |
  | cost / 1k queries | $2.17 | $1.10 | **$13.32** |
  | median latency ms | 2,537 | 5,344 | 2,567 |

- **Apparent surface story.** v0.8 *beats* the heuristic baseline
  (67.3%) at 77.3% accuracy. But the cost ($13.32/1k) and the 99%
  opus concentration are red flags — that's not really diversified
  routing.

### D9 — Found the real load-bearing bug: runtime double-counts duplicate registry entries

- **What.** The Go scorer in `internal/router/cluster/scorer.go`
  builds `s.models` from the full filtered candidates list with
  duplicates kept. Then per-cluster scoring is:
  ```go
  for _, k := range topClusters {
      row := s.rankings[k]
      for _, m := range s.models {     // iterates duplicates
          scores[m] += row[m]          // map key collapses → 2× count
      }
  }
  ```
  Models with N registry entries get **N× weight** in argmax.

- **Per-version weights (count of registry entries per model):**

  | model | v0.6 / v0.7 | v0.8 |
  |---|---|---|
  | gemini-3.1-pro-preview | ×2 | **×1** |
  | claude-opus-4-7 | ×2 | ×2 |
  | claude-sonnet-4-5 | ×2 | ×2 |
  | everyone else | ×1 | ×1 |

- **Confirms all observed routing behavior:**
  - v0.6/v0.7 ant+goog: gem-pro ×2 outranks everyone → 100% gem-pro ✓
  - v0.8 ant+goog: gem-pro back to ×1; opus ×2 still has its boost →
    99% opus ✓ (sonnet ×2 wins on cluster 4 prompts where opus
    crashes to 0.016, hence the 6/809 sonnet picks)

- **Re-framing of v0.6's broken state.** The plan doc attributed
  v0.6's gemini-pro dominance to "per-prompt minmax magnifies tiny
  bench gaps" combined with proxy entries. The minmax part is real
  but **secondary** — the dominant driver is the runtime weighting
  bug. Even if we'd fixed the bench math perfectly, the registry
  duplicates would have skewed argmax toward the most-duplicated
  model.

- **Fix options.**
  - **F1.** Fix the runtime: dedupe `s.models` and `s.candidates` in
    `NewScorer` so each model is iterated once per cluster. The
    cleanest fix because `bench_walker.load_bench` already averages
    multiple bench columns into one rankings.json score per model.
    Risk: changes routing for v0.1–v0.6 production behavior on
    redeploy, but staging-only verification protects customers.
  - **F2.** Edit registries to single-entry per model. Workaround
    that doesn't touch Go code. Less clean — requires keeping the
    deployed_models list dedup'd as a discipline.
  - **F3.** Treat duplicate weighting as intentional. Document and
    use deliberately. Means future registry edits need to think
    about "how many entries do I want this model to have?" — an
    awkward routing knob.

- **Recommendation: F1.** It's the principled fix and the
  rankings.json already represents per-model averages correctly.
  The downside (v0.6 routing changes on production deploy) is
  exactly what the eval harness + version pinning is for.

- **Implication for v0.7/v0.8 smoke results.** Both runs were
  measuring duplicate-weighted argmax, not pure scorer behavior.
  The "v0.8 beats heuristic at 77.3%" headline is real but not
  reproducible after the F1 fix — we'd need to re-smoke
  post-runtime-fix to know v0.8's true behavior.

### D10 — Land F1 (runtime dedup fix)

- **Patch.** `internal/router/cluster/scorer.go` `NewScorer`: after
  the existing `sort.SliceStable(candidates, byModel)`, dedupe
  adjacent duplicates in place. Both `s.candidates` and `s.models`
  flow from the deduped slice. Updated the godoc on the
  `candidates` field to explain why dedup is load-bearing.
- **Test.** New `TestScorer_DedupesDuplicateRegistryEntries`:
  fixture has opus listed twice (proxy + closer-family proxy, the
  v0.6+ pattern) with rankings opus=0.35, haiku=0.6. Without dedup
  opus would accumulate to 0.70 and beat haiku; with dedup haiku
  wins at 0.6. Direct assertion on `s.models` count + end-to-end
  argmax check.
- **Verification.** `go test -tags no_onnx ./internal/router/cluster/...`
  passes (the new test + every regression test). No retrain needed —
  bench_walker already produces one rankings.json row per model;
  the bug was purely at iteration time.

### D11 — Post-fix smoke results (v0.6, v0.7, v0.8 vs heuristic)

All three smokes ran in parallel against the fixed runtime, same
RouterArena sub_10 split (809 prompts).

| metric | v0.6 post | v0.7 post | v0.8 post | heuristic baseline |
|---|---|---|---|---|
| accuracy | 0.611 | 0.615 | **0.689** | 0.673 |
| arena_score | 57.95 | 60.11 | 61.92 | 64.2 |
| cost / 1k | $3.32 | $1.03 | $7.32 | (presumably lower) |
| median latency ms | 5,238 | 1,139 | 3,533 | — |

Pick distributions (post-fix):

- v0.6: gemini-3.1-pro-preview 563 (70%), gpt-5.5 245 (30%), haiku 1
- v0.7: gpt-4.1 602 (74%), gemini-3.1-pro-preview 207 (26%)
- v0.8: gpt-5.5 808 (99%), haiku 1

#### What the dedup fix unmasked

- **v0.6's true behavior is a gem-pro / gpt-5.5 split, not 100%
  gem-pro.** The previously-reported 99.26% gem-pro number was the
  duplicate-counting bug masquerading as "decisive routing."

- **v0.7's true behavior is gpt-4.1-dominant.** Raw scoring exposes
  exactly the column-coverage bias I diagnosed in D4 (gpt-4.1's
  6-bench-only 0.672 mean). The bug was hiding this.

- **v0.8's true behavior is gpt-5.5-dominant.** Removing the
  gem-pro 2nd registry entry (D5) didn't matter while opus had ×2
  weight; once dedup is in, gpt-5.5's broad gpt-5 proxy coverage
  wins under zscore normalization.

#### Score-normalization comparison (pre-fix masking removed)

minmax (v0.6 baseline): 61.1% accuracy
raw (v0.7 trainer flag): 61.5% accuracy
zscore (v0.8 trainer flag): **68.9% accuracy**

zscore is the winning normalization mode by ~7-8 pp. This validates
D5's choice and the plan's Step A→A' progression (raw was a
necessary intermediate to expose the column-bias problem; zscore
is the principled fix).

#### Plan acceptance check (v0.8)

From `ROUTER_V0_7_CALIBRATION_FIX.md` §2 goals:
- Accuracy ≥ 65%: **PASS** at 68.9% (also beats heuristic 67.3%)
- Arena ≥ 60: **PASS** at 61.9 (still below heuristic 64.2)
- ≥3 distinct models, no >85% concentration: **FAIL** — 99% on gpt-5.5

Stretch (≥68%, Arena ≥65, beat heuristic on multi-provider):
- 68.9% ≥ 68% ✓
- Arena 61.9 < 65 ✗
- Multi-provider beat: partial (beats heuristic accuracy, not
  arena)

#### Re-framed structural diagnosis

The duplicate-counting bug was masking a different issue: **proxy
entries with broad bench coverage create systematic concentration**
under whatever score normalization wins. minmax made gem-pro
concentration extreme; raw shifts to gpt-4.1 concentration; zscore
shifts to gpt-5.5 concentration. The bench column with the broadest
coverage wins regardless of math because top-p=4 sum smooths
per-cluster variation.

The principled fix is direct labels (Step C). Cheaper interim
moves: registry trim of more proxy entries (Step B redux) or
log-cost normalization (Step D, would re-weight toward cheaper
models).

### D12 — Wrote `scripts/simulate_routing.py` to A/B normalization tweaks for free

- **What.** New helper that embeds RouterArena prompts via the same
  Jina v2 INT8 ONNX as the trainer, then replays the runtime's
  top-p=4 sum + argmax against each artifact's `rankings.json` —
  no LLM calls, ~1 min wall clock for sub_10 (809 prompts). Supports
  three transform families:
  - `baseline` — argmax over all models in rankings
  - `exclude=<m1>+<m2>+...` — drop named models from candidates
    (simulates a registry-trim retrain)
  - `zscore_per_model` — for each model, rescale per-cluster scores
    to mean 0 / std 1 across the K=10 clusters (kills the
    "consistent decent across clusters" pattern that compounds with
    top-p=4 sum)
- **Why.** Lets us compare normalization candidates against the
  same prompt set that drives RouterArena, without paying for any
  LLM calls. Each variant runs in seconds after embeddings are
  cached.
- **Caveat.** Sim simplifies: skips the heuristic short-prompt
  fallback in scorer.go and uses Python's INT8 ONNX path (vs Go's
  hugot path). Directional findings are reliable; exact % can drift
  ~5–10 pp vs a real smoke. The new patched-runtime smokes confirm
  baseline-variant routing matches reality on the dominant winner.

### D13 — Simulation: `zscore_per_model` is the winner

Variants tested against post-fix sub_10 prompts:

| version | variant | distinct | top-3 picks |
|---|---|---|---|
| v0.6 | baseline | 2 | gem-pro 96%, gpt-5.5 4% |
| v0.6 | **zscore_per_model** | **7** | **gem-pro 60%, haiku 14%, gpt-5.5 12%** |
| v0.6 | exclude=gpt-5.5 | 1 | gem-pro 100% |
| v0.7 | baseline | 2 | gpt-4.1 95%, gem-pro 5% |
| v0.7 | **zscore_per_model** | **7** | **gpt-4.1 37%, haiku 36%, gem-pro 9%** |
| v0.7 | exclude=gpt-5.5 | 2 | gpt-4.1 95%, gem-pro 5% |
| v0.8 | baseline | 2 | gpt-5.5 98%, gem-pro 2% |
| v0.8 | **zscore_per_model** | **6** | **haiku 46%, gpt-4.1 34%, opus 10%** |
| v0.8 | exclude=gpt-5.5 | 2 | gem-pro 94%, gem-flash-lite 6% |
| v0.8 | exclude=gpt-5.5+gpt-4.1+opus-4-7 | 2 | gem-pro 94%, gem-flash-lite 6% |

#### Findings

1. **`exclude` variants confirm the structural problem.** Dropping
   the dominant winner just shifts concentration to the next
   broadest-coverage proxy. v0.8 minus gpt-5.5 → 94% gem-pro. Even
   excluding {gpt-5.5, gpt-4.1, opus} → still 94% gem-pro. The
   "broad bench coverage wins under top-p=4 sum" pattern is robust
   to single-model trims.

2. **`zscore_per_model` cleanly diversifies.** All three versions
   jump from 2 → 6-7 distinct winners; no model exceeds 60%. v0.7
   and v0.8 in particular split nearly evenly between the top 2
   models (gpt-4.1 and haiku). This is what the plan's diversity
   acceptance criterion was actually asking for.

3. **The zscore mechanism is interpretable.** After per-model
   z-score, every model has mean 0 across clusters; argmax picks
   the model whose z-score *peaks* on each prompt's top-p clusters
   rather than the model with the highest absolute score. Models
   that were "consistent decent" (high mean, low variance) get
   their z-scores flattened to 0 — they no longer dominate. Models
   that are "differentially good on cluster X" keep their high
   z-score on X.

4. **Concern: zscore loses absolute quality information.** A
   genuinely-better model that's consistently above-average gets
   the same z-score baseline as a genuinely-worse model that's
   consistently below-average. So while diversification clearly
   wins, accuracy is a separate question — only an actual smoke
   tells us whether picking opus-on-cluster-where-opus-peaks beats
   picking gpt-5.5-everywhere on RouterArena scoring.

#### Decision

Bake `zscore_per_model` into the trainer as a post-α-blend step,
train v0.9, smoke against RouterArena sub_10. ~30 min retrain +
$2 smoke. The simulation gives high confidence in routing
diversity; the smoke is needed for accuracy.

### D14 — Bake `--per-model-zscore` into the trainer; train v0.9

- **Trainer change.** Added `per_model_zscore()` helper next to
  `alpha_blend` and `--per-model-zscore` CLI flag. When set, the
  α-blended rankings get the per-model z-score post-process before
  rankings.json is written. Both metadata sites (rankings.json
  meta + metadata.yaml `training`) gain a `per_model_zscore: bool`
  field. Implementation parity with
  `scripts/simulate_routing.py::apply_zscore_per_model` so sim
  results carry through to trained artifacts.
- **v0.9 training.** Copied v0.8's registry into v0.9/, ran
  `train_cluster_router.py --version v0.9 --k 10 --shrinkage-k0 10
  --alpha 0.53 --score-normalization zscore --per-model-zscore
  --no-promote`. ~17 min wall clock. Output confirms "Per-model
  z-score post-process across clusters ..." step ran. 78/80 cells
  observed, metadata records `per_model_zscore: true`, `latest`
  stayed at v0.6.
- **Simulator confirms the prediction exactly.** v0.9 baseline on
  sub_10 = 6 distinct winners, haiku 46% / gpt-4.1 34% / opus 10%,
  matching the D13 simulation of (v0.8 + zscore_per_model
  post-process). The trainer transform is mathematically equivalent
  to the simulation transform.

### D15 — v0.9 smoke: complete win across all plan goals

| metric | v0.6 post | v0.7 post | v0.8 post | **v0.9 post** | heuristic |
|---|---|---|---|---|---|
| accuracy | 0.611 | 0.615 | 0.689 | **0.705** | 0.673 |
| arena | 57.95 | 60.11 | 61.92 | **63.96** | 64.2 |
| cost/1k | $3.32 | $1.03 | $7.32 | **$5.73** | — |
| median ms | 5,238 | 1,139 | 3,533 | **1,885** | — |
| distinct | 3 | 2 | 2 | **5** | 1 |

**Pick distribution (v0.9):** haiku 459 (57%), opus 268 (33%),
gpt-5.5 46 (6%), gpt-4.1 30 (4%), sonnet 6 (1%).

**Routing × difficulty:**
- easy (n=389): haiku 66%, opus 28%, gpt-5.5 4%, gpt-4.1 2%
- medium (n=233): opus 51%, haiku 41%, gpt-5.5 5%, gpt-4.1 3%, sonnet 1%
- hard (n=187): haiku 58%, opus 23%, gpt-5.5 10%, gpt-4.1 8%, sonnet 2%

**Accuracy × difficulty:**
- easy 376/389 (96.7%) — best of all four versions
- medium 149/233 (63.8%) — best of all four versions
- hard 43/187 (24.4%) — between v0.8 (29.3%) and v0.7 (21.8%)

**Acceptance check vs `ROUTER_V0_7_CALIBRATION_FIX.md` §2 goals:**

Primary:
- Accuracy ≥ 65%: PASS at **70.5%** (+3.2pp over heuristic)
- Arena ≥ 60: PASS at **63.96** (within 0.24pt of heuristic)
- ≥3 distinct, no >85% concentration: PASS at **5 distinct, max 57%**

Stretch:
- Accuracy ≥ 68%: PASS at 70.5%
- Arena ≥ 65: near-miss at 63.96
- Beat heuristic on multi-provider: PASS on accuracy (+3.2pp),
  near-miss on arena (-0.24pt)

**Why v0.9 won despite erasing absolute-quality differences:**

The original concern about per-model z-score was that erasing
absolute quality means "the consistently great model loses to the
spiky-good model." Empirically that didn't bite: the new routing
matches haiku to easy prompts and opus to medium prompts where
they actually excel, instead of always-gpt-5.5. Net win because:
1. Cost drops (haiku $0.8/1k vs gpt-5.5 $5/1k) on the 60% of
   prompts haiku handles
2. Easy/medium accuracy goes up (haiku is genuinely good on easy,
   opus on medium)
3. The hard-difficulty regression vs v0.8 (-4.9pp) is more than
   offset by easy (+2.1pp) and medium (+6.2pp) gains

### D16 — _pending_

(Decide what to ship: v0.9 is the new accuracy + arena leader and
hits all primary plan goals. Remaining work options:
1. Promote v0.9 to `latest` and ship the F1 dedup fix + v0.9
   together as one PR.
2. Run full 8400-prompt smoke to confirm sub_10 generalizes before
   promotion.
3. Push for the stretch arena goal: try `--per-cluster-zscore`
   variant or apply Step D log-cost normalization on top of v0.9.
4. Update `docs/eval/EVAL_RESULTS.md` §7 with v0.9 numbers
   alongside the corrected v0.6/v0.7/v0.8 results.)
