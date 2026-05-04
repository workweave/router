"""One-off diagnostic: per-deployed-model bench observation counts for v0.6.

Why: v0.6's rankings.json gives Opus 0.0–0.3 quality on 9 of 10 clusters
while Gemini-3.1-pro-preview gets 0.85+. The hypothesis is that Opus has
sparse bench coverage and shrinkage_k0=10 is collapsing it toward the
prior. This script verifies that hypothesis by counting how many bench
records each deployed model has globally and per-bench-column. No
embedder, no clustering — runs in seconds.
"""

from __future__ import annotations

import sys
from collections import Counter, defaultdict
from pathlib import Path

SCRIPT_DIR = Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPT_DIR))

from bench_walker import load_bench  # noqa: E402
from train_cluster_router import load_registry, ARTIFACTS_DIR  # noqa: E402


def main() -> int:
    version_dir = ARTIFACTS_DIR / "v0.6"
    cache_dir = SCRIPT_DIR / ".bench-cache"

    entries, bench_to_deployed = load_registry(version_dir)
    deployed = sorted({e["model"] for e in entries})

    print(f"Registry v0.6: {len(deployed)} deployed models, "
          f"{len(bench_to_deployed)} bench columns")
    print()
    print("Bench-column → deployed-model mapping:")
    for col, models in sorted(bench_to_deployed.items()):
        print(f"  {col:<30} -> {models}")
    print()

    print(f"Loading bench from {cache_dir} ...")
    prompts, scores = load_bench(cache_dir, bench_to_deployed)
    print(f"  {len(prompts)} unique prompts, "
          f"{sum(len(v) for v in scores.values())} (prompt, model) score rows")
    print()

    per_model: Counter = Counter()
    for prompt, model_scores in scores.items():
        for m in model_scores:
            per_model[m] += 1

    print("Global observation count per deployed model:")
    print(f"  {'model':<35} {'count':>8} {'% of prompts':>14}")
    total = len(prompts)
    for m in sorted(deployed, key=lambda x: -per_model[x]):
        n = per_model[m]
        pct = 100.0 * n / total if total else 0.0
        bar = "#" * int(pct / 2)
        print(f"  {m:<35} {n:>8} {pct:>13.1f}%  {bar}")
    print()

    # Bench-column → model mapping for the cells in question.
    # Per-column counts aren't available after Stage C consolidation, so we
    # show the global per-model count (computed above) alongside the columns
    # that feed it — enough to spot "model M only has K observations across
    # all its bench columns" without re-walking the bench parquet.
    print("Bench columns feeding key models (with global per-model count):")
    print()
    for target_model in ["claude-opus-4-7", "gemini-3.1-pro-preview", "claude-sonnet-4-5", "gpt-5.5"]:
        cols_for_model = [c for c, ms in bench_to_deployed.items() if target_model in ms]
        n = per_model[target_model]
        print(f"  {target_model} (global count: {n}):")
        for c in cols_for_model:
            print(f"    bench_column={c}")
        print()

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
