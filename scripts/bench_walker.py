"""Shared walker for the extracted OpenRouterBench cache.

Layout after download_bench.sh + tar -xzf:
    bench-release/<benchmark_name>/<bench_model_name>/<benchmark>-<model>-<ts>.json
each JSON is shaped:
    {
      "model_name": str, "dataset_name": str, "counts": int,
      "records": [{"index": int, "prompt": str, "score": float, ...}, ...]
    }

This module deduplicates prompts across (benchmark, bench_model) combos
and aggregates raw scores into per-(prompt, deployed_model) means.
**Multiple deployed models may share a bench column** — e.g. proxy
entries for claude-opus-4-7 (no native bench column) are scored using
gpt-5's data alongside the gpt-5 deployed entry that's also routable.
`load_bench` accepts a list-valued mapping
{bench_column: [deployed_model, ...]} so it can copy the same bench
score to every deployed entry that references that column.

Used by inspect_bench.py / sweep_cluster_k.py / train_cluster_router.py
to keep the loading semantics consistent across all three.
"""

from __future__ import annotations

import json
import sys
from collections import defaultdict
from pathlib import Path
from typing import Dict, Iterable, List, Tuple


def discover_bench_root(cache_dir: Path) -> Path:
    """Locate the extracted bench root. download_bench.sh extracts the
    HF tarball into <cache>/bench-release/. Falls back to <cache>/bench/
    for parity with the plan-doc's older example.
    """
    candidate = cache_dir / "bench-release"
    if candidate.is_dir():
        return candidate
    fallback = cache_dir / "bench"
    if fallback.is_dir():
        return fallback
    sys.exit(
        f"ERROR: no bench-release/ or bench/ directory under {cache_dir} — "
        "did download_bench.sh finish (including tar -xzf)?"
    )


def iter_bench_files(bench_root: Path) -> Iterable[Tuple[str, str, Path]]:
    """Yield (benchmark_name, bench_model_name, json_path).

    Handles both layouts present in the OpenRouterBench tarball:
      * <benchmark>/<bench_model>/<*.json>          (flat — e.g. arenahard*)
      * <benchmark>/<split>/<bench_model>/<*.json>  (nested — e.g.
        livecodebench/test/<model>, mmlupro/test_1000/<model>)
    The intermediate split directory is treated as part of the benchmark
    name so different splits are not silently merged.
    """
    for benchmark_dir in sorted(bench_root.iterdir()):
        if not benchmark_dir.is_dir():
            continue
        # Detect layout: if any first-level child contains JSON files
        # directly, treat it as a model directory (flat layout). Otherwise
        # descend one more level (nested layout).
        children = sorted(p for p in benchmark_dir.iterdir() if p.is_dir())
        if not children:
            continue
        flat = any(any(c.glob("*.json")) for c in children)
        if flat:
            for model_dir in children:
                for json_path in sorted(model_dir.glob("*.json")):
                    yield benchmark_dir.name, model_dir.name, json_path
            continue
        # Nested: <benchmark>/<split>/<model>/*.json
        for split_dir in children:
            for model_dir in sorted(p for p in split_dir.iterdir() if p.is_dir()):
                for json_path in sorted(model_dir.glob("*.json")):
                    bench_name = f"{benchmark_dir.name}/{split_dir.name}"
                    yield bench_name, model_dir.name, json_path


def load_bench(
    cache_dir: Path,
    bench_to_deployed: Dict[str, List[str]],
    score_normalization: str = "minmax",
) -> Tuple[List[str], Dict[str, Dict[str, float]]]:
    """Walk the extracted bench cache. Returns:
      * deduped list of unique prompt strings (insertion-order stable
        for reproducibility across runs)
      * {prompt: {deployed_model: per-prompt score}}

    Three-stage pipeline:

    Stage A — collect raw per-(prompt, bench_column) means. We average
    *within* a single bench column (multiple submissions on the same
    SWE-bench instance, repeat runs, etc.) but never across columns at
    this stage. The output is keyed by the bench column name as it
    appears on disk, NOT by deployed model.

    Stage B — per-prompt cross-column rescaling. The behavior here is
    selected by `score_normalization`:

      * "minmax" (default, v0.5+): per-prompt min-max across columns.
        Maximally discriminative — every prompt's best column lands at
        1.0 and worst at 0.0. **Magnifies tiny gaps**: a 7-point bench
        gap between two columns becomes the maximal 1.0-point ranking
        gap. Single-column / constant-across-columns prompts → 0.5.

      * "raw": skip the per-prompt rescale entirely. Stage A's column
        means flow straight into Stage C. Preserves the original bench
        gap shape, so models with smaller absolute differences from the
        leader stay close to the leader. In practice all current bench
        columns produce values in [0, 1] (OpenRouterBench is continuous
        0–1; SWE-bench / LCB are binary 0/1) so the downstream
        per-cluster aggregation still operates on a roughly [0, 1]
        scale, but this mode does not formally guarantee that.

      * "zscore": per-prompt z-score across columns ((s - mean) / std),
        clipped to [-3, 3] then linearly mapped to [0, 1] via
        (z + 3) / 6. A middle ground: scales by per-prompt spread
        without making 7-point gaps maximal. Single-column /
        constant-across-columns prompts → 0.5 (std == 0 fallback).

    Stage C — map columns to deployed models via `bench_to_deployed`.
    If multiple columns map to the same deployed model AND both cover
    the same prompt, we average the Stage-B-output scores. In practice
    this is rare because SWE-bench prompt strings and OpenRouterBench
    prompt strings don't overlap, so the two columns feeding e.g.
    claude-opus-4-7 contribute disjoint per-prompt evidence.
    """
    if score_normalization not in ("minmax", "raw", "zscore"):
        raise ValueError(
            f"score_normalization must be 'minmax', 'raw', or 'zscore'; "
            f"got {score_normalization!r}"
        )
    bench_root = discover_bench_root(cache_dir)

    seen_prompts: set = set()
    prompts: List[str] = []
    # Stage A: per-(prompt, bench_column) raw sums + counts.
    col_sums: Dict[str, Dict[str, float]] = defaultdict(lambda: defaultdict(float))
    col_counts: Dict[str, Dict[str, int]] = defaultdict(lambda: defaultdict(int))

    for _benchmark_name, bench_model_name, json_path in iter_bench_files(bench_root):
        if bench_model_name not in bench_to_deployed:
            # Skip files for bench columns no deployed model uses.
            continue
        try:
            with json_path.open("r", encoding="utf-8") as f:
                doc = json.load(f)
        except (json.JSONDecodeError, OSError) as err:
            print(f"WARNING: skipping {json_path}: {err}", file=sys.stderr)
            continue

        for rec in doc.get("records") or []:
            prompt = rec.get("prompt") or rec.get("origin_query")
            score = rec.get("score")
            if not isinstance(prompt, str) or not prompt:
                continue
            if not isinstance(score, (int, float)):
                continue
            if prompt not in seen_prompts:
                seen_prompts.add(prompt)
                prompts.append(prompt)
            col_sums[prompt][bench_model_name] += float(score)
            col_counts[prompt][bench_model_name] += 1

    # Stage A finalize: per-(prompt, column) raw mean.
    col_means: Dict[str, Dict[str, float]] = {}
    for prompt, sums in col_sums.items():
        col_means[prompt] = {
            col: sums[col] / col_counts[prompt][col]
            for col in sums
            if col_counts[prompt][col] > 0
        }

    # Stage B: per-prompt rescaling across columns. Branch on
    # score_normalization. See the docstring for the trade-offs.
    col_normalized: Dict[str, Dict[str, float]] = {}
    if score_normalization == "raw":
        # Passthrough: emit raw column means. No single-column collapse
        # to 0.5 — a prompt with one column keeps that column's raw
        # score because that's the only signal we have for it.
        col_normalized = col_means
    elif score_normalization == "minmax":
        # Per-prompt min-max across columns. Single-column prompts → 0.5
        # (no relative signal). Constant-across-columns prompts → 0.5
        # too: nothing to discriminate, but the prompt still anchors a
        # cluster.
        for prompt, by_col in col_means.items():
            if len(by_col) <= 1:
                col_normalized[prompt] = {col: 0.5 for col in by_col}
                continue
            vals = list(by_col.values())
            v_min = min(vals)
            v_max = max(vals)
            v_range = v_max - v_min
            if v_range <= 0:
                col_normalized[prompt] = {col: 0.5 for col in by_col}
                continue
            col_normalized[prompt] = {
                col: (s - v_min) / v_range for col, s in by_col.items()
            }
    else:  # "zscore"
        # Per-prompt z-score across columns, clipped to [-3, 3] and
        # linearly mapped to [0, 1]. std == 0 (constant or single
        # column) collapses to 0.5, matching minmax's neutral fallback.
        for prompt, by_col in col_means.items():
            if len(by_col) <= 1:
                col_normalized[prompt] = {col: 0.5 for col in by_col}
                continue
            vals = list(by_col.values())
            mean = sum(vals) / len(vals)
            variance = sum((v - mean) ** 2 for v in vals) / len(vals)
            std = variance ** 0.5
            if std <= 0:
                col_normalized[prompt] = {col: 0.5 for col in by_col}
                continue
            col_normalized[prompt] = {
                col: (max(-3.0, min(3.0, (s - mean) / std)) + 3.0) / 6.0
                for col, s in by_col.items()
            }

    # Stage C: project columns onto deployed models. Average across
    # columns when a deployed model has multiple columns covering the
    # same prompt; in practice this only fires when SWE-bench and
    # OpenRouterBench prompt strings collide, which they don't today.
    means: Dict[str, Dict[str, float]] = {}
    for prompt, by_col in col_normalized.items():
        deployed_sums: Dict[str, float] = defaultdict(float)
        deployed_counts: Dict[str, int] = defaultdict(int)
        for col, s in by_col.items():
            for deployed in bench_to_deployed.get(col, ()):
                deployed_sums[deployed] += s
                deployed_counts[deployed] += 1
        if deployed_sums:
            means[prompt] = {
                m: deployed_sums[m] / deployed_counts[m]
                for m in deployed_sums
            }

    return prompts, means
