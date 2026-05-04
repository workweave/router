"""RouterBench-Martian cross-validation.

`withmartian/routerbench` is a 405k-row HF dataset of pre-computed
inferences across 11 models on MMLU, HellaSwag, GSM8K, MBPP, MT-Bench,
ARC, WinoGrande, RAG. **No new inference cost.**

The Phase 1a contract:

  1. Map our 3 deployed Anthropic models to the 3 closest columns in
     their N-model score matrix. Direction-of-quality only — exact
     version match isn't possible.
  2. Run our v0.1 cluster scorer over their prompts (HTTP against
     staging) and capture the picked column.
  3. Compare the picked column's score to the oracle-best score in
     each row. Render a second Pareto plot.

Time-boxed at 1 day during week 2. If the schema mapping is too loose
to be meaningful, document and skip — emit `routerbench_skipped.json`
with the schema diff and mark as follow-up in EVAL_RESULTS.md.
"""

from __future__ import annotations

import asyncio
import json
import statistics
from dataclasses import dataclass
from pathlib import Path

# Best-effort version mapping. Update when Anthropic ships new versions
# OR when RouterBench refreshes columns. Keep the rationale inline so
# a reviewer can sanity-check.
COLUMN_MAP: dict[str, str] = {
    # ours -> RouterBench column id
    "claude-opus-4-7": "claude-3-opus-20240229",
    "claude-sonnet-4-5": "claude-3-5-sonnet-20240620",
    "claude-haiku-4-5": "claude-3-haiku-20240307",
}


@dataclass
class RouterBenchResult:
    n_rows: int
    mean_picked_score: float
    mean_oracle_score: float
    mean_uniform_score: float  # average across the 3 mapped columns; reference
    coverage: float           # fraction of rows where v0.1 routed without error


async def evaluate_routerbench(
    *,
    n_rows: int = 500,
    output_path: Path | None = None,
) -> RouterBenchResult:
    """Lazy import — keeps tests fast and avoids forcing the `datasets`
    install on contributors who only need the Pareto plotter."""
    from datasets import load_dataset  # type: ignore[import-untyped]

    from eval.routing import route

    ds = load_dataset("withmartian/routerbench", split="train", streaming=True)
    rows = []
    if n_rows > 0:
        for row in ds:
            rows.append(row)
            if len(rows) >= n_rows:
                break

    picked_scores: list[float] = []
    oracle_scores: list[float] = []
    uniform_scores: list[float] = []
    errors = 0

    columns = list(COLUMN_MAP.values())

    for row in rows:
        per_model = {col: row.get(col) for col in columns if col in row}
        if not per_model:
            errors += 1
            continue
        # The dataset format isn't fully unified across slices; fall
        # back to the first numeric value we find per column.
        per_model_score = {col: _coerce_score(v) for col, v in per_model.items()}
        per_model_score = {k: v for k, v in per_model_score.items() if v is not None}
        if not per_model_score:
            errors += 1
            continue

        prompt = _extract_prompt(row)
        if not prompt:
            errors += 1
            continue

        # `route()` already converts known transport failures (HTTPError,
        # timeout) into a RoutedResult with `error` set. Letting any
        # other exception propagate is intentional — it signals a real
        # bug in the eval harness or the staging router that should
        # surface, not be quietly tallied as a row-level error.
        res = await route(router="v0.2-cluster", prompt=prompt)
        if res.error:
            errors += 1
            continue
        picked_col = COLUMN_MAP.get(res.model_used)
        if not picked_col or picked_col not in per_model_score:
            errors += 1
            continue
        picked_scores.append(per_model_score[picked_col])
        oracle_scores.append(max(per_model_score.values()))
        uniform_scores.append(statistics.mean(per_model_score.values()))

    result = RouterBenchResult(
        n_rows=len(rows),
        mean_picked_score=_safe_mean(picked_scores),
        mean_oracle_score=_safe_mean(oracle_scores),
        mean_uniform_score=_safe_mean(uniform_scores),
        coverage=(len(rows) - errors) / len(rows) if rows else 0.0,
    )
    if output_path is not None:
        output_path.write_text(json.dumps(result.__dict__, indent=2))
    return result


def _safe_mean(xs: list[float]) -> float:
    return float(statistics.mean(xs)) if xs else 0.0


def _coerce_score(v) -> float | None:
    if isinstance(v, (int, float)):
        return float(v)
    if isinstance(v, dict):
        for k in ("score", "accuracy", "value"):
            if k in v and isinstance(v[k], (int, float)):
                return float(v[k])
    return None


def _extract_prompt(row: dict) -> str:
    """RouterBench rows include either `prompt` or `question` shaped
    fields. Pick whichever is available; empty string signals skip."""
    for k in ("prompt", "question", "input"):
        v = row.get(k)
        if isinstance(v, str) and v.strip():
            return v
    return ""


# Synchronous facade for callers that don't want to think about asyncio.
def evaluate(*, n_rows: int = 500, output_path: Path | None = None) -> RouterBenchResult:
    return asyncio.run(evaluate_routerbench(n_rows=n_rows, output_path=output_path))
