"""Diagnostic walk over the downloaded OpenRouterBench cache.

Confirms that the model columns we need at P0 — gpt-5, gemini-2.5-pro,
claude-sonnet-4, gemini-2.5-flash — are populated, and prints
per-benchmark row counts and per-model coverage. Run this *first*,
before sweep_cluster_k.py or train_cluster_router.py: a missing column
means re-clustering won't matter because the matrix won't have
anywhere to score against.

Bench layout after extraction:
    bench-release/<benchmark_name>/<model_name>/<benchmark>-<model>-<timestamp>.json
where each JSON has shape:
    {
      "model_name": str, "dataset_name": str, "counts": int,
      "records": [{"index": int, "prompt": str, "score": float, ...}, ...]
    }

Usage:
    cd router/scripts && poetry run python inspect_bench.py [--cache .bench-cache]

Output is plain text on stdout; non-zero exit code if a load-bearing
column has zero rows.
"""

from __future__ import annotations

import argparse
import json
import sys
from collections import Counter, defaultdict
from pathlib import Path
from typing import Dict, Iterable, Tuple

# Bench columns that the multi-provider deployed set in
# model_registry.json relies on. If any of these have zero rows,
# train_cluster_router will emit a rankings.json missing those columns
# and the Go scorer will refuse to boot. Catch the gap here, not at
# the Go layer. Update this list whenever the registry's bench_column
# field set changes.
REQUIRED_MODELS: Tuple[str, ...] = (
    # OpenRouterBench-direct columns (already populated at v0.2).
    "gpt-5",
    "gpt-4.1",
    "gemini-2.5-pro",
    "gemini-2.5-flash",
    "claude-sonnet-4",
    # v0.3-direct columns from public-data ingestion (SWE-bench
    # experiments). If any of these have zero rows after
    # `python -m scripts.ingest.fetch_all`, the alias map didn't match
    # any source-side model name — extend `ingest/model_aliases.py` and
    # re-run ingestion. claude-haiku-4-5 stays on its v0.2 proxy
    # (gemini-2.5-flash) until a public benchmark with per-instance
    # haiku scores ships.
    "claude-sonnet-4-5",
    "claude-opus-4-5",
    "gemini-3-pro-preview",
)


def discover_bench_dirs(cache_dir: Path) -> Path:
    """Locate the extracted bench root. download_bench.sh extracts
    bench-release.tar.gz into <cache>/bench-release/.
    """
    candidate = cache_dir / "bench-release"
    if candidate.is_dir():
        return candidate
    # Older layouts used `bench/`; keep the fallback path for parity
    # with the plan-doc inline example.
    fallback = cache_dir / "bench"
    if fallback.is_dir():
        return fallback
    raise SystemExit(
        f"ERROR: no bench-release/ or bench/ directory under {cache_dir} — "
        "did download_bench.sh finish (including tar -xzf)?"
    )


from bench_walker import iter_bench_files as _iter_bench_files


def iter_bench_files(bench_root: Path) -> Iterable[Tuple[str, str, Path]]:
    """Wrapper around bench_walker.iter_bench_files so this script keeps
    a single import boundary while sharing the layout-detection logic
    with sweep_cluster_k.py and train_cluster_router.py.
    """
    yield from _iter_bench_files(bench_root)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--cache",
        type=Path,
        default=Path(__file__).resolve().parent / ".bench-cache",
        help="Path to the bench cache directory (output of download_bench.sh).",
    )
    parser.add_argument(
        "--no-strict",
        dest="strict",
        action="store_false",
        help="Don't exit non-zero when a REQUIRED_MODELS column is empty.",
    )
    parser.set_defaults(strict=True)
    args = parser.parse_args()

    if not args.cache.exists():
        print(f"ERROR: {args.cache} does not exist; run scripts/download_bench.sh first", file=sys.stderr)
        return 2

    bench_root = discover_bench_dirs(args.cache)
    print(f"Bench root: {bench_root}")
    print()

    # Aggregate per-bench / per-model row counts.
    per_bench: Dict[str, int] = defaultdict(int)
    per_model: Counter[str] = Counter()
    per_bench_model: Dict[str, Counter[str]] = defaultdict(Counter)
    files_walked = 0

    for benchmark_name, model_name, json_path in iter_bench_files(bench_root):
        files_walked += 1
        try:
            with json_path.open("r", encoding="utf-8") as f:
                doc = json.load(f)
        except (json.JSONDecodeError, OSError) as err:
            print(f"WARNING: skipping {json_path.relative_to(bench_root)}: {err}", file=sys.stderr)
            continue
        records = doc.get("records") or []
        n = len(records)
        per_bench[benchmark_name] += n
        per_model[model_name] += n
        per_bench_model[benchmark_name][model_name] += n

    print(f"Walked {files_walked} JSON files")
    print()

    # Output table 1: per-benchmark row totals.
    print("=== per-benchmark row counts ===")
    for benchmark_name in sorted(per_bench):
        print(f"  {benchmark_name:30s} {per_bench[benchmark_name]:>8d}")
    print()

    # Output table 2: required-model coverage.
    print("=== required-model coverage ===")
    missing = []
    for model in REQUIRED_MODELS:
        rows = per_model.get(model, 0)
        per_bench_present = sum(1 for b in per_bench if per_bench_model[b].get(model, 0) > 0)
        flag = "  ← LOAD-BEARING" if rows == 0 else ""
        print(
            f"  {model:30s} rows={rows:>6d}  "
            f"benchmarks={per_bench_present}/{len(per_bench)}{flag}"
        )
        if rows == 0:
            missing.append(model)
    print()

    # Output table 3: full per-model row counts (top 20). Useful for
    # picking new proxies if any of REQUIRED_MODELS turn out underspecified.
    print("=== all models with row counts (top 20) ===")
    for model, count in per_model.most_common(20):
        print(f"  {model:40s} {count:>8d}")
    print()

    if missing:
        print(f"FAIL: {len(missing)} required model(s) missing rows: {missing}")
        if args.strict:
            return 1
    else:
        print("OK: all required models present.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
