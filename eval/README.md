# router/eval — Phase 1a eval harness

The Phase 1a go/no-go gate for the cluster-routing project. Produces a
cost-vs-quality Pareto plot + per-router table comparing the multi-
provider cluster (`v0.2-cluster`) vs the heuristic vs always-Opus /
Sonnet / Haiku on 500 public-benchmark prompts, judged by a GPT-5 +
Gemini 2.5 Pro ensemble.

`v0.1-cluster` is the legacy 3-Anthropic-only cluster, kept as a
historical label. By default it points at `ROUTER_BASE_URL` (same
deployment as v0.2). Override `ROUTER_BASE_URL_V01` to pin it at a
frozen deployment carrying the legacy artifact when you want a
side-by-side comparison.

The full eval also includes `v0.2-cluster-last-user`: same staging
deployment, same cluster scorer, but the harness sends
`x-weave-embed-last-user-message: true` so the scorer's input is the
most recent user-typed prompt rather than the legacy concatenated
system+messages stream. Lets us A/B feature-extraction shapes on a
single deployment without redeploying with a new env var. The override
is gated on `installation.is_eval_allowlisted` exactly like
`x-weave-disable-cluster`.

For the design rationale and where this fits in the broader project,
see [`docs/plans/archive/CLUSTER_ROUTING_PLAN.md`](../docs/plans/archive/CLUSTER_ROUTING_PLAN.md)
Phase 1a (lines ~1149–1169, ~1318–1338).

## Run a smoke

Validates secrets mount, GCS write, output parses end-to-end. ~10
prompts, 3 routers, 1 judge.

```bash
cd router/eval
poetry install
poetry run modal run modal_app.py --smoke
```

> **Import path note:** The eval package expects `router/` on the
> Python path (Modal handles this via `.add_local_python_source("eval")`).
> For local `python -m eval.*` commands, run from `router/` or set
> `PYTHONPATH=router/`.

## Run the full eval

500 prompts × 5 routers (one baseline + 4 candidates) × 2 judges on
each candidate-vs-baseline pair. Cost ~$250–600 per run.

```bash
poetry run modal run modal_app.py
```

Outputs go to
`gs://workweave-prod-01-models-v2-training/router-eval/<run_id>/`:

- `prompts.jsonl` — frozen 500-prompt set
- `manifest.json` — model versions, judge versions, prompt-set hash
- `inference/<prompt_id>__<router>.jsonl` — one per inference call
- `judgments/<prompt_id>__<judge>__<router>.jsonl` — one per judgment
- `aggregated.json` — per-router results
- `pareto.png` — the Pareto plot
- `eval_results.md` — candidate body for `docs/eval/EVAL_RESULTS.md`

## Run the spot-check (κ verification)

The protocol-validity gate. After a full run, score 30 hand-validated
rows yourself; the CLI reports Cohen's κ between you and the ensemble.
**κ ≥ 0.6 is the bar — if lower, fix the rubric and re-run.**

The harness writes inference and judgment rows into per-prompt files
under `inference/` and `judgments/` subdirectories, so concatenate them
into single JSONL files for the spot-check CLI. Run `poetry run` from
inside `router/eval/` so the project's virtualenv is picked up.

```bash
mkdir -p /tmp/router-eval
gsutil cp gs://.../router-eval/<run_id>/prompts.jsonl /tmp/router-eval/prompts.jsonl
gsutil cp -r gs://.../router-eval/<run_id>/inference /tmp/router-eval/inference
gsutil cp -r gs://.../router-eval/<run_id>/judgments /tmp/router-eval/judgments
cat /tmp/router-eval/inference/*.jsonl > /tmp/router-eval/inference.jsonl
cat /tmp/router-eval/judgments/*.jsonl > /tmp/router-eval/judgments.jsonl

cd router/eval
poetry run python -m eval.spot_check \
    --prompts   /tmp/router-eval/prompts.jsonl \
    --inference /tmp/router-eval/inference.jsonl \
    --judgments /tmp/router-eval/judgments.jsonl \
    --n 30 --out /tmp/router-eval/spot_check.jsonl
```

## RouterBench-Martian cross-validation

Re-uses the staging deployment to score v0.1 against RouterBench's
405k pre-computed inferences. Time-boxed at 1 day; if the schema
mapping is too loose, document and skip.

```bash
poetry run python -c "from eval.routerbench import evaluate; print(evaluate(n_rows=200))"
```

Output: `RouterBenchResult` (mean picked / oracle / uniform scores +
coverage). Render a second Pareto via `eval.pareto.render_plot` and
embed in `EVAL_RESULTS.md`.

## RouterArena submission

Manual. Submit v0.1 via the
[`RouteWorks/RouterArena`](https://github.com/RouteWorks/RouterArena)
GitHub flow. Their leaderboard runs the eval; we just paste the
returned rank+score into `EVAL_RESULTS.md`.

## Adding a new benchmark loader

Drop a new file in `benchmarks/`, decorate the class with `@register`,
and reference it from `slice_plan.SLICES` (or call it ad-hoc from a
script). The Phase 1b real-traffic loader is one new file.

```python
# benchmarks/my_loader.py
from typing import ClassVar
from eval.benchmarks import BenchmarkLoader, register
from eval.types import BenchmarkPrompt

@register
class MyLoader(BenchmarkLoader):
    name: ClassVar[str] = "my-loader"

    def load(self, n: int, seed: int) -> list[BenchmarkPrompt]:
        ...  # return n prompts deterministically per seed
```

Then add to `__init__.py`'s side-effect imports and reference
`my-loader` from `slice_plan.SLICES`.

## Testing

```bash
poetry run pytest
```

Covers rubric parsing, ensemble median + disagreement flag, Pareto
math, benchmark registry, routing.py header shape, reference grading.
No external services required (httpx mocked via `respx`).

## Configuration (`.env` / Modal secrets)

Local: copy `.env.example` to `.env`. Modal: each Modal function
loads secrets via `modal.Secret.from_name(...)` in `modal_app.py`.

| Var | Used by | Modal secret name |
|---|---|---|
| `ANTHROPIC_API_KEY` | always-{opus,sonnet,haiku} inference | `anthropic-api-key` |
| `OPENAI_API_KEY` | always-{gpt55,gpt55-mini,gpt-4.1} inference + GPT-5 judge | `openai-api-key` |
| `GOOGLE_API_KEY` | always-gemini3-{pro,flash,flash-lite} inference + Gemini 2.5 Pro judge | `google-api-key` |
| `ROUTER_BASE_URL` | staging-routed clients | (env-only; e.g. `https://router-staging.workweave.ai`) |
| `ROUTER_EVAL_API_KEY` | staging-routed clients | `weave-router-eval-key` |
| `EVAL_GCS_PREFIX` | result writers | (defaults to prod-01 bucket) |
| `HF_TOKEN` | benchmark download (optional; raises HF rate limits) | — |
| GCP credentials | inference, judging, aggregation (GCS writes) | `gcp-credentials` |

The eval API key MUST be associated with an installation whose
`is_eval_allowlisted` column is `true`. Seed it with
`wv mr seed-key -e staging-01 --eval-allowlist --name router-eval` (Weave CLI)
(prints the token to use as `ROUTER_EVAL_API_KEY`). The override
middleware checks this flag per request — see
`router/internal/server/middleware/eval_override.go`. No env-var or
redeploy is needed to allowlist a new installation.
