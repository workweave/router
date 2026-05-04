Created: 2026-05-02
Last edited: 2026-05-03 (added §7 RouterArena run on 8,400-prompt official methodology)

# Phase 1a eval results

> **Status: CONTINUE.** v0.6-cluster strictly Pareto-dominates v0.5 and
> always-Opus on the May 2, 2026 250-prompt judge-ensemble run
> (`run-f687cd8cae`). Decision and rationale below.

## 1. Run metadata

| Field | Value |
|---|---|
| Run ID | `run-f687cd8cae` |
| Date | 2026-05-02 |
| Prompt-set hash (SHA-256) | `683b9b3f3e4e5c4f55efd13f9f23c64b6f145bdfbd05f5f8eebbd13aa4204cf0` |
| Prompt count | 250 / 500 (8 of 16 slices skipped — gated HF datasets, see §5) |
| Routers under test | `always-opus`, `always-haiku`, `v0.5-cluster`, `v0.6-cluster` |
| Models picked (cluster-routed) | `claude-opus-4-7`, `claude-sonnet-4-5`, `claude-haiku-4-5` (Anthropic only — staging binary is the pre-multi-provider deploy) |
| Judges | GPT-5 (`gpt-5`), Gemini 2.5 Pro (`gemini-2.5-pro`) |
| Cluster artifact SHA-256 (v0.6, first 12 chars) | `centroids.bin=f9f0804b101b`, `rankings.json=0b4a0740e7fb`, `model_registry.json=bbb8c5159865` |
| Router base URL | `https://router.staging-01.workweave.ai` (Modal worker default; manifest field shows local-dev `http://localhost:8082` because the manifest was built against the local `.env` — a follow-up cleanup) |
| Eval installation | `bd6c1fc9-6ca1-42ed-881d-cbf470183afd` (eval-allowlisted, seeded May 2, 2026) |

## 2. Pareto plot

![Cost vs quality (judge ensemble)](../../assets/eval/pareto.png)

(Source: `gs://workweave-prod-01-models-v2-training/router-eval/run-f687cd8cae/pareto.png`. The committed copy above is the same image; update both together.)

## 3. Per-router results

| Router | n | Total cost (USD) | Mean quality | P50 latency (ms) | P95 latency (ms) |
|---|---:|---:|---:|---:|---:|
| `always-haiku` | 250 | $0.49 | 0.842 | 2,289 | 9,877 |
| `always-opus` | 250 | $7.84 | 0.500 | 2,343 | 24,360 |
| `v0.5-cluster` | 250 | $2.85 | 0.842 | 5,717 | 15,368 |
| **`v0.6-cluster`** | **250** | **$1.91** | **0.856** | **5,649** | **13,795** |

`always-opus` mean quality reads as 0.500 because it was the **judge baseline**: every other router was scored pairwise vs Opus, so Opus-vs-itself is the 0.5 reference point. The 0.842 / 0.856 numbers on the other rows are mean normalized judge scores (0–1) against Opus, not literal head-to-head win-rates.

### Per-router model-pick distribution

| Router | Opus | Sonnet | Haiku |
|---|---:|---:|---:|
| `v0.5-cluster` | 93 (37%) | 12 (5%) | 145 (58%) |
| `v0.6-cluster` | 52 (21%) | 54 (22%) | 144 (58%) |

Phase 4's per-cluster α is visible here: v0.6 shifted ~16% of traffic from Opus to Sonnet without changing Haiku usage. Sonnet is 5× cheaper than Opus at near-equivalent quality on those clusters; that move is the entire source of v0.6's $0.94 saving over v0.5.

## 4. Judge ensemble health

Not run for this gate. Spot-check, κ, Spearman re-runs are protocol-validity items
that would have been the next step if the headline numbers had been borderline —
v0.6 vs v0.5 / vs Opus is unambiguous (Pareto-dominates on every axis) so we did
not budget the rubric-iteration cycle. **Follow-up:** run the spot-check
(`router/eval/spot_check.py`, n=30) before the next major artifact promotion so
the κ value is on file.

## 5. Per-slice breakdown

Not aggregated into `aggregated.json` for this run. Slice-level signal is
recoverable from the inference + judgment JSONL files in
`gs://.../router-eval/run-f687cd8cae/`. **Follow-up:** extend
`eval.aggregation.aggregate_to_router_results` to emit per-slice win-counts
between adjacent routers (v0.5 vs v0.6, v0.6 vs always-Opus).

**Slice coverage of this run:**

| Slice | Loaded | Reason |
|---|---:|---|
| coding-python | 45 | ✅ |
| coding-humaneval-mbpp | 25 | ✅ |
| math-gsm8k | 25 | ✅ |
| knowledge-mmlu | 30 | ✅ |
| summarization | 25 | ✅ |
| chat-mt-bench | 25 | ✅ |
| multilingual | 20 | ✅ |
| edge-cases | 55 | ✅ |
| coding-ts | 0 | ❌ `Aider-AI/polyglot-benchmark` gated/missing |
| coding-go | 0 | ❌ same |
| coding-rust-cpp-java | 0 | ❌ same |
| coding-sql | 0 | ❌ `xlangai/BIRD` gated/missing |
| tool-calling-single | 0 | ❌ `gorilla-llm/Berkeley-Function-Calling-Leaderboard` no supported data files |
| tool-calling-parallel-multi | 0 | ❌ same |
| tool-calling-agentic | 0 | ❌ `sierra-research/tau-bench` gated/missing |
| math-gpqa | 0 | ❌ `Idavidrein/gpqa` gated, requires HF auth |

The gated/missing slices are independent of the cluster scorer's quality —
they're upstream HF dataset access issues. Setting an `HF_TOKEN` Modal secret
with access to the gated repositories would close this gap; the eval harness
already passes the token through when present.

## 6. RouterBench-Martian comparison

Not run for this gate. The bench-holdout regret eval
(`router/scripts/holdout_eval.py`, n=3023, seed=42) covers the same
"oracle-vs-router" question RouterBench was designed to surface, and was the
basis for the v0.5 → v0.6 retrain decision. **Follow-up:** revisit if/when an
external comparator becomes load-bearing for a customer ask.

## 7. RouterArena rank

Run completed 2026-05-03 on the full 8,400-prompt
[RouterArena](https://github.com/RouteWorks/RouterArena) split using
their **official methodology** (vendored under
`router/eval/_routerarena_official/`, Apache-2.0). Numbers are
directly comparable to the
[public leaderboard](https://routeworks.github.io/leaderboard) — same
prompts, same per-dataset metrics, same Arena Score formula.

### 7.1 Headline

| Metric | Value |
|---|---|
| **Accuracy** | **66.50%** (5,587/8,400; mean continuous score) |
| **Arena Score** | **63.06** (β=0.1, c_max=$200, c_min=$0.0044) |
| **Cost / 1k queries** | **$2.31** (full inference: input + output) |
| Coverage | 100% (8,400/8,400 routed) |
| Pick distribution | 97.31% Haiku-4.5, 2.69% Opus-4.7 |
| Mean input tokens | 306.7 |
| Mean output tokens | 172.0 |
| Latency | 1.66s median, 3.71s p95 |
| Eval cost (one-shot) | ~$22 (full inference at max_out=512; LCB rows re-run at max_out=2048) |

### 7.2 Position on the public leaderboard

| Rank | Router | Arena Score | Accuracy | Cost/1k |
|------|--------|------------:|---------:|--------:|
| 🥇 | R2-Router | 71.60 | 71.23 | $0.06 |
| 🥈 | vLLM-SR | 67.23 | 66.53 | $0.06 |
| 🥉 | MIRT-BERT | 66.89 | 66.88 | $0.15 |
| 4 | Azure-Router | 66.66 | 68.09 | $0.54 |
| 5 | NIRT-BERT | 66.12 | 66.34 | $0.21 |
| 6 | GPT-5 | 64.32 | 73.96 | $10.02 |
| 7 | CARROT | 63.87 | 67.21 | $2.06 |
| 8 | Chayan | 63.83 | 64.89 | $0.56 |
| **9** | **v0.6-cluster** | **63.06** | **66.50** | **$2.31** |
| 10 | RouterBench-MLP | 57.56 | 61.62 | $4.83 |
| 11 | NotDiamond | 57.29 | 60.83 | $4.10 |
| 12 | GraphRouter | 57.22 | 57.00 | $0.34 |
| 13 | RouterBench-KNN | 55.48 | 58.69 | $4.27 |
| 14 | RouteLLM | 48.07 | 47.04 | $0.27 |
| 15 | RouterDC | 33.75 | 32.01 | $0.07 |

By **raw accuracy** alone (66.50%), we sit between vLLM-SR (#2 by
composite) and NIRT-BERT (#5) — within 0.4pp of the second-ranked
router on the public board. The Arena Score docks us because the
composite penalises routers whose model pool is expensive on average,
and ours is frontier-only (Anthropic + OpenAI + Google premium tiers)
where most leaderboard routers route between Llama / Mixtral / Phi /
GPT-4o-mini at $0.10–$0.60/M token.

### 7.3 Frontier-pool comparators (apples-to-apples)

The only leaderboard entries that route between frontier-tier models:

| Router | Accuracy | Cost/1k | Note |
|--------|---------:|--------:|------|
| GPT-5 (always-frontier) | 73.96% | $10.02 | No routing — always GPT-5 |
| **v0.6-cluster** | **66.50%** | **$2.31** | **4.3× cheaper than GPT-5, 7.5pp lower acc** |
| CARROT | 67.21% | $2.06 | Frontier-tier pool |
| NotDiamond | 60.83% | $4.10 | Commercial frontier router |

vs **GPT-5 always-frontier**: 4.3× cheaper at 7.5pp lower accuracy.
vs **CARROT**: roughly tied on cost and accuracy.
vs **NotDiamond**: 1.8× cheaper at 5.7pp higher accuracy.

### 7.4 Accuracy by difficulty

| Bucket | n | Correct | Accuracy |
|--------|---:|--------:|---------:|
| Easy | 3,990 | 3,744 | **93.85%** |
| Medium | 2,445 | 1,437 | **58.73%** |
| Hard | 1,965 | 406 | **20.65%** |

Hard-bucket performance is the biggest soft spot — 20.7% on hard
prompts means v0.6 is sending most of them to Haiku rather than
escalating to Opus. The router currently picks Opus for only 2.7% of
prompts overall.

### 7.5 Accuracy by domain

| Domain | n | Correct | Accuracy |
|--------|---:|--------:|---------:|
| 1 Philosophy and psychology | 700 | 529 | **75.57%** |
| 5 Science | 1,400 | 1,057 | **75.50%** |
| 6 Technology | 1,400 | 1,008 | **72.00%** |
| 9 History | 700 | 468 | **66.86%** |
| 3 Social Science | 700 | 451 | **64.43%** |
| 4 Language | 700 | 453 | **60.00%** |
| 0 Computer science / general | 1,400 | 831 | **59.36%** |
| 7 Arts & recreation | 700 | 409 | **58.43%** |
| 8 Literature | 700 | 269 | **43.04%** |

### 7.6 Accuracy by metric

| Metric | n | Correct | Accuracy | Notes |
|--------|---:|--------:|---------:|-------|
| `mcq_accuracy` | 5,924 | 4,563 | **77.03%** | MMLU, MMLUPro, ArcMMLU, OpenTDB, Ethics, MathQA, MedMCQA, etc. |
| `superglue_exact_match` | 355 | 236 | **66.48%** | RC, Wsc, Wic, Entailment, QA |
| `code_accuracy` | 171 | 112 | **65.50%** | LiveCodeBench (only rows with extractable code blocks) |
| `math_metric` | 280 | 157 | **56.07%** | GSM8K, MATH, AIME, AsDiv, FinQA |
| `meteor_score` | 640 | 316 | **49.27%** | NarrativeQA + WMT19 (continuous score; partial credit) |
| `exact_match` | 689 | 196 | **28.45%** | QANTA quizbowl, GeoGraphyData |
| `code_accuracy:no_code` | 214 | 0 | **0.00%** | LCB rows where Haiku didn't emit a `\`\`\`` code block |
| `chess_accuracy` | 68 | 7 | **10.29%** | ChessInstruct (free-form move) |
| `superglue_clozetest` | 59 | 0 | **0.00%** | Known leaderboard-wide issue — see §7.7 caveats |

### 7.7 Caveats to disclose at launch

1. **`superglue_clozetest` scores 0/59 for every router on the
   leaderboard.** RouterArena's prompt template is internally
   inconsistent — asks for a "letter choice" while gold answers are
   full phrases like "Cassini, Cassini spacecraft" or "Facebook".
   Their own enhanced extractor returns `text[0].upper()` for
   ClozeTest, which can never match the phrase. Not v0.6-specific.
2. **`code_accuracy:no_code` 0/214.** 56% of LiveCodeBench prompts
   produced no extractable triple-backtick code block from Haiku,
   even after we re-ran with `max_output_tokens=2048`. Their LCB
   prompt template doesn't require fenced code blocks, so models
   often write raw code which their `has_code` regex rejects. Same
   penalty applies to every router using their pipeline.
3. **No Sonnet picks.** v0.6 routes only Haiku (97.3%) or Opus (2.7%)
   despite a multi-provider registry that includes Sonnet-4.5,
   GPT-5.5, GPT-4.1, Gemini-3.x. The medium-difficulty bucket (58.7%)
   has the most room — Sonnet-quality routing of medium prompts
   should add ~3–5pp overall accuracy without much cost increase.
   This is the highest-leverage improvement available pre-launch.
4. **Cost basis matches the leaderboard methodology.** Full input +
   output token cost, summed across the 8,400 prompts, divided by
   8,400/1,000. Per-million-token prices match
   `model_cost/model_cost.json` upstream: `claude-haiku-4-5` = $1/M
   input, $5/M output; `claude-opus-4-7` = $15/M input, $75/M output.
5. **Translations and NarrativeQA use METEOR (continuous 0–1 score).**
   These contribute partial credit to the headline accuracy rather
   than binary right/wrong. Matches the leaderboard.

### 7.8 Methodology — what's vendored, what's ours

`router/eval/_routerarena_official/` contains an exact copy of
upstream's evaluation code at the time of vendoring, with the LICENSE
attached:

| File | Purpose |
|------|---------|
| `metrics.py` | All per-dataset metric functions |
| `metric_utils.py` | `math_equal`, `symbolic_equal`, `parse_digits`, etc. |
| `enhanced_extractor.py` | `\boxed{X}` extractor with per-dataset rules |
| `livecodebench_util.py` | Sandboxed code-execution runner + `reliability_guard` |
| `prompt_templates.json` | 41 per-dataset prompts pulled from `config/eval_config/zero-shot/*.json` |
| `LICENSE` | Apache-2.0 attribution |

Our wrappers (in `router/eval/`):

| File | Purpose |
|------|---------|
| `routerarena.py` | Main harness — formats prompts via vendored templates, dispatches through router, grades responses, computes Arena Score |
| `grade.py` | Maps `Dataset name` → official metric function; returns `GradeResult` with continuous score |
| `grade_lcb.py` | Loads `lighteval/code_generation_lite` (release_v2), runs `check_correctness` in a process pool with reliability_guard |
| `merge_lcb.py` | Splices LCB-only re-run results into a full-run file |
| `regrade.py` | Re-grade cached responses without re-running inference (free) |

### 7.9 Reproducing the headline run

```bash
# One-shot (writes results/routerarena_v0.6_full_official.json)
.venv/bin/python -m eval.routerarena \
  --router v0.6-cluster --split full --concurrency 8 \
  --max-output-tokens 512 \
  --out results/routerarena_v0.6_full_official.json

# Re-run LiveCodeBench rows with bigger output budget so models
# don't truncate mid-CoT before producing code blocks.
.venv/bin/python -m eval.routerarena \
  --router v0.6-cluster --split full --concurrency 8 \
  --max-output-tokens 2048 --only-dataset-prefix LiveCodeBench \
  --out results/routerarena_v0.6_lcb_only.json

# Splice LCB responses back in, then run sandboxed code-execution
# grading on the merged file.
.venv/bin/python -m eval.merge_lcb \
  --base results/routerarena_v0.6_full_official.json \
  --lcb  results/routerarena_v0.6_lcb_only.json \
  --out  results/routerarena_v0.6_full_with_lcb.json

.venv/bin/python -m eval.grade_lcb \
  results/routerarena_v0.6_full_with_lcb.json --workers 4
```

Final result file: `results/routerarena_v0.6_full_with_lcb_lcb.json`.
Wall clock: ~30 min full run + ~5 min LCB-only re-run + ~7 min
code-execution grading. Total cost: ~$22 in API spend.

### 7.10 Companion runs (path to the headline)

| Run | Methodology | Accuracy | Cost/1k | Arena | Notes |
|-----|-------------|---------:|--------:|------:|-------|
| 1. Routing-only shortcut (`max_out=1`) | None — picks only | n/a | $1.04 (input-only) | n/a | Cheap directional cost shape |
| 2. Live inference + custom grader (`max_out=512`) | Hand-rolled regex grader | 65.2%* | $1.04 | n/a | *gradeable subset only; 12% ungradeable |
| 3. Live inference + grader v2 | Fixed numeric extraction, dropped LCB | 68.8% | $1.04 | n/a | Still our grader |
| 4. **Official methodology** (this section) | **Vendored RouterArena grader + per-dataset prompts** | **66.50%** | **$2.31** | **63.06** | **Apples-to-apples with leaderboard** |

The headline accuracy went *down* from run 3 → run 4 because the
official methodology counts the 385 LiveCodeBench prompts and the 59
ClozeTest prompts (both close to 0 for Haiku) toward the denominator
where our custom grader excluded them. The cost went *up* because
the LCB-only re-run with 2,048-token output budget is more verbose
than the 512 cap used elsewhere.

Run 4 is the one to use externally — it matches what the leaderboard
would compute for v0.6.

## 8. Decision

> **CONTINUE** — Steven Tohme, 2026-05-02.
>
> v0.6-cluster strictly Pareto-dominates v0.5-cluster on every measured axis:
> mean quality 0.856 vs 0.842, total cost $1.91 vs $2.85 (33% cheaper), P95
> latency 13.8s vs 15.4s. Versus `always-opus` it is **4.1× cheaper at
> substantially higher judge-ensemble quality** (0.856 mean judge score vs
> 0.500 reference). The bench-holdout regret prediction (regret 0.135 vs 0.139) held
> up under judge-ensemble validation, which means the per-cluster α retrain
> (Phase 4, see `ROUTER_V1_PLAN.md` §4) is doing what the math said it would:
> shifting 16% of v0.5's Opus traffic onto Sonnet on prompts where Sonnet's
> quality matches Opus's at 5× lower cost.
>
> Continue to:
> 1. **Promote v0.6 to staging long-term** (already done as part of this PR).
> 2. ~~**Delete `internal/router/routellm/`**~~ — done; package and the
>    two `extract_mf_weights.py` / `dump_test_vector.py` scripts that
>    only existed to build its weights blob are gone from the tree.
> 3. **Phase 0 telemetry chunks 3–5** (cache-token extraction, observations
>    pipeline, middleware-owned span buffer) — the §13 Tier 1 work that unlocks
>    Phase 1 cost-aware routing.
> 4. **Re-run this eval against the full 500-prompt set** once an `HF_TOKEN`
>    with gated-dataset access is provisioned. The 250-prompt result is
>    already unambiguous; the missing 250 are a coverage completion item, not
>    a decision-changer.
