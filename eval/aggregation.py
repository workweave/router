"""Aggregation: inference + judgment rows → per-router RouterResult.

Pure: no I/O, no GCS, no Modal. Both the cloud harness (modal_app.py)
and the local harness (compare.py) call this with the same row types
and get the same RouterResults out.
"""

from __future__ import annotations

from collections import defaultdict
from statistics import median

from eval.types import InferenceRow, JudgmentRow, RouterResult


def aggregate_to_router_results(
    inference_rows: list[InferenceRow],
    judgment_rows: list[JudgmentRow],
    *,
    baseline_router: str,
) -> list[RouterResult]:
    """Build per-router RouterResult tuples ready for the Pareto plot.

    The judging protocol is comparative: every candidate is scored
    against the baseline (`always-opus` in the standard config) and the
    baseline itself is never judged. To keep the baseline on the Pareto
    plot we fix its `mean_quality` at 0.5 — the by-construction tie
    point of an A/B-style rubric — instead of the misleading 0.0 that
    falls out of "no judgments found".
    """
    by_router_inf: dict[str, list[InferenceRow]] = defaultdict(list)
    for r in inference_rows:
        by_router_inf[r.router].append(r)

    # Median ensemble score per (prompt, candidate_router) — across judges.
    per_pair: dict[tuple[str, str], list[float]] = defaultdict(list)
    for j in judgment_rows:
        per_pair[(j.prompt_id, j.candidate_router)].append(j.score)

    out: list[RouterResult] = []
    for router_name, rows in by_router_inf.items():
        latencies = [r.latency_ms for r in rows]
        latencies.sort()
        cost = sum(r.cost_usd for r in rows)
        picks: dict[str, int] = defaultdict(int)
        for r in rows:
            if r.model_used:
                picks[r.model_used] += 1
        if router_name == baseline_router:
            mean_quality = 0.5
        else:
            scores = []
            for r in rows:
                judgments = per_pair.get((r.prompt_id, router_name), [])
                if judgments:
                    scores.append(median(judgments))
            mean_quality = (sum(scores) / len(scores)) if scores else 0.0
        out.append(
            RouterResult(
                router=router_name,  # type: ignore[arg-type]
                n_prompts=len(rows),
                total_cost_usd=cost,
                mean_quality=mean_quality,
                reference_pass_rate=None,
                p50_latency_ms=latencies[len(latencies) // 2] if latencies else 0,
                p95_latency_ms=latencies[int(0.95 * (len(latencies) - 1))] if latencies else 0,
                model_picks=dict(picks),
            )
        )
    return out
