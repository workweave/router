"""Tier 1 — held-out bench regret simulator.

Cheap iteration loop for the cluster router. Splits the bench cache 80/20
by deterministic prompt-hash, simulates routing on the 20% held-out
slice for every committed artifact version (plus always-X baselines),
and reports per-router achieved score / oracle score / regret / cost
proxy. Costs nothing — no live inference, no judges, just embeds +
argmaxes against the same bench labels we trained on.

Use this between LLM-judge eval runs:

    poetry run python holdout_eval.py                # all versions, default split
    poetry run python holdout_eval.py --versions v0.4,v0.5
    poetry run python holdout_eval.py --holdout-frac 0.2 --seed 42

Caveats — read these before quoting numbers anywhere:

  * The "held-out" prompts ARE in the bench data the rankings cells
    were aggregated from. Centroids are fit on all prompts; rankings
    cells are means over all prompts in each cluster. So the 20%
    split here was technically seen by training. This is an
    *in-distribution* check, not a true generalization test. For a
    proper holdout you'd retrain v0.X with the 80% split feeding the
    cell aggregation. The relative comparison between routers
    (always-opus vs v0.4 vs v0.5) is fair because each is evaluated
    on the same prompts; absolute regret numbers are slightly
    optimistic.

  * Cost proxy uses ``cost_per_1k_input_usd × len(prompt) / 4`` — the
    /4 is the rough chars-per-token rule. Output cost is ignored
    (the trainer doesn't track it). Good enough for ranking routers
    by cost; don't read absolute dollar figures off the table.

  * Bench score per (prompt, model) is the per-prompt-normalized
    value from ``bench_walker.load_bench`` — same numbers the trainer
    sees, so this measures "did the router pick the model the
    bench labels say is best on this prompt?", which is exactly the
    routing-decision-quality question we want for fast iteration.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import struct
import sys
import time
from collections import defaultdict
from pathlib import Path
from typing import Dict, List, Tuple

import numpy as np

from bench_walker import load_bench
from train_cluster_router import (
    ARTIFACTS_DIR,
    ASSETS_DIR,
    DEFAULT_COST_PER_1K_INPUT,
    EMBED_DIM,
    embed_batch,
    load_embedder,
    load_registry,
    read_latest,
)


def split_holdout(prompts: List[str], holdout_frac: float, seed: int) -> Tuple[List[int], List[int]]:
    """Hash-bucket each prompt to a uniform [0, 1) draw and assign it to
    train (< 1 - holdout_frac) or held-out. Hashing keyed by both the
    prompt text and the seed so two seeds give independent splits while
    each seed is reproducible.
    """
    train: List[int] = []
    held: List[int] = []
    for i, prompt in enumerate(prompts):
        h = hashlib.sha256(f"{seed}::{prompt}".encode("utf-8")).digest()
        # First 8 bytes as uint64 → uniform draw
        bucket = int.from_bytes(h[:8], "big") / 2**64
        (held if bucket < holdout_frac else train).append(i)
    return train, held


def load_centroids_bin(path: Path) -> np.ndarray:
    """Read centroids.bin in the format ``train_cluster_router.write_centroids``
    emits and the Go runtime parses. Returns (K, dim) float32 array.
    """
    with path.open("rb") as f:
        raw = f.read()
    if raw[:4] != b"CRT1":
        sys.exit(f"{path}: bad magic {raw[:4]!r}; want b'CRT1'")
    version, k, dim = struct.unpack("<III", raw[4:16])
    if version != 1:
        sys.exit(f"{path}: unsupported centroids version {version}")
    if dim != EMBED_DIM:
        sys.exit(f"{path}: dim {dim} != EMBED_DIM {EMBED_DIM}")
    expected = 16 + k * dim * 4
    if len(raw) != expected:
        sys.exit(f"{path}: size mismatch (got {len(raw)}, want {expected})")
    arr = np.frombuffer(raw[16:], dtype="<f4").reshape(k, dim)
    return arr.copy()


def load_artifact_bundle(version: str) -> Tuple[np.ndarray, Dict[int, Dict[str, float]], List[Dict], dict]:
    """Centroids, rankings, registry entries, and the cost dict from
    metadata.yaml (falls back to DEFAULT_COST_PER_1K_INPUT when the
    metadata predates the cost section)."""
    vdir = ARTIFACTS_DIR / version
    if not vdir.exists():
        sys.exit(f"version {version}: directory {vdir} missing")
    centroids = load_centroids_bin(vdir / "centroids.bin")
    rankings_raw = json.loads((vdir / "rankings.json").read_text())
    rankings = {int(k): v for k, v in rankings_raw["rankings"].items()}
    cost = rankings_raw.get("meta", {}).get("cost_per_1k_input_usd")
    entries, _ = load_registry(vdir)
    if not cost:
        cost = {e["model"]: DEFAULT_COST_PER_1K_INPUT.get(e["model"], 1.0) for e in entries}
    return centroids, rankings, entries, cost


def simulate_cluster_route(
    embeddings: np.ndarray,
    centroids: np.ndarray,
    rankings: Dict[int, Dict[str, float]],
    candidate_models: List[str],
    top_p: int,
) -> List[str]:
    """Mirror the Go scorer: top-p nearest centroids by cosine, sum
    the rankings rows uniformly across them, argmax over candidates.
    """
    # Both centroids and embeddings are L2-normalized at the trainer's
    # write step / embed step, so dot product == cosine similarity.
    sims = embeddings @ centroids.T  # (N, K)
    top_idx = np.argpartition(-sims, kth=min(top_p, sims.shape[1] - 1), axis=1)[:, :top_p]
    picks: List[str] = []
    for row_top in top_idx:
        # Sum rankings across the top-p clusters for each candidate.
        agg = {m: 0.0 for m in candidate_models}
        for k in row_top:
            row = rankings[int(k)]
            for m in candidate_models:
                if m in row:
                    agg[m] += row[m]
        # Argmax with deterministic tie-breaking (insertion order on
        # candidate_models, which we sort upstream).
        best_m, best_s = candidate_models[0], float("-inf")
        for m in candidate_models:
            if agg[m] > best_s:
                best_m, best_s = m, agg[m]
        picks.append(best_m)
    return picks


def estimate_input_tokens(prompt: str) -> int:
    """Rough chars/4 ≈ tokens. Accurate enough for cost ranking; off
    by ~10-20% in absolute dollar terms but consistent across routers
    so the comparative cost column is fair."""
    return max(1, len(prompt) // 4)


def evaluate_router(
    name: str,
    pick_fn,
    held_prompts: List[str],
    scores: Dict[str, Dict[str, float]],
    cost_per_1k: Dict[str, float],
) -> dict:
    """Apply ``pick_fn(prompt) -> model_name`` to every held-out prompt
    and aggregate achieved score / regret / cost. Prompts where the
    picked model has no bench score on this prompt contribute 0 to the
    achieved sum (and inflate regret); they're counted in the
    ``coverage`` field so a router that picks a model with weak bench
    coverage gets penalized correctly rather than skipped.
    """
    total_achieved = 0.0
    total_oracle = 0.0
    total_cost = 0.0
    n_covered = 0
    n = len(held_prompts)
    for prompt in held_prompts:
        m = pick_fn(prompt)
        per_model = scores.get(prompt, {})
        if not per_model:
            continue
        oracle = max(per_model.values())
        achieved = per_model.get(m, 0.0)
        total_oracle += oracle
        total_achieved += achieved
        total_cost += cost_per_1k.get(m, 1.0) * estimate_input_tokens(prompt) / 1000
        if m in per_model:
            n_covered += 1
    return {
        "router": name,
        "n_evaluated": n,
        "coverage": n_covered / n if n else 0.0,
        "mean_achieved": total_achieved / n if n else 0.0,
        "mean_oracle": total_oracle / n if n else 0.0,
        "mean_regret": (total_oracle - total_achieved) / n if n else 0.0,
        "mean_cost_usd": total_cost / n if n else 0.0,
    }


def render_table(rows: List[dict]) -> str:
    """Plain-text table sorted by mean_regret ascending (best routers
    first). Width-aware so versions like v0.10-cluster don't break
    alignment."""
    rows = sorted(rows, key=lambda r: r["mean_regret"])
    headers = ["router", "achieved", "oracle", "regret", "cost", "coverage"]
    fmt = [
        lambda r: r["router"],
        lambda r: f"{r['mean_achieved']:.3f}",
        lambda r: f"{r['mean_oracle']:.3f}",
        lambda r: f"{r['mean_regret']:.3f}",
        lambda r: f"${r['mean_cost_usd']*1000:.4f}/1k",
        lambda r: f"{r['coverage']*100:.0f}%",
    ]
    cols = [[h] + [f(r) for r in rows] for h, f in zip(headers, fmt)]
    widths = [max(len(c) for c in col) for col in cols]
    out = []
    out.append("  ".join(c.ljust(w) for c, w in zip(headers, widths)))
    out.append("  ".join("-" * w for w in widths))
    for i in range(len(rows)):
        out.append("  ".join(col[i + 1].ljust(w) for col, w in zip(cols, widths)))
    return "\n".join(out)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--cache", type=Path, default=Path(__file__).resolve().parent / ".bench-cache")
    parser.add_argument("--assets", type=Path, default=ASSETS_DIR)
    parser.add_argument(
        "--versions",
        default=None,
        help="Comma-separated artifact versions to evaluate. Default: every directory under artifacts/.",
    )
    parser.add_argument("--holdout-frac", type=float, default=0.2)
    parser.add_argument("--seed", type=int, default=42)
    parser.add_argument("--batch-size", type=int, default=32)
    parser.add_argument("--limit", type=int, default=0,
                        help="If >0, evaluate at most this many held-out prompts (smoke). Random sample.")
    args = parser.parse_args()

    if not (0.0 < args.holdout_frac < 1.0):
        sys.exit("--holdout-frac must be in (0, 1)")

    # Resolve versions. Each must own a model_registry.json — we use
    # the union of all versions' deployed-model sets as the always-X
    # candidate baseline pool, since some always-X targets only exist
    # in newer artifacts.
    if args.versions:
        versions = [v.strip() for v in args.versions.split(",") if v.strip()]
    else:
        versions = sorted(
            p.name for p in ARTIFACTS_DIR.iterdir()
            if p.is_dir() and (p / "model_registry.json").exists()
        )
    if not versions:
        sys.exit("no artifact versions found")

    print(f"Evaluating versions: {versions}")
    latest = read_latest()
    print(f"  latest pointer → {latest}")

    # Use the latest version's registry as the source-of-truth bench
    # column mapping. Each version's bench mapping should be a superset
    # of older ones in practice; if not, retrain the older one before
    # comparing.
    latest_dir = ARTIFACTS_DIR / latest
    _, bench_to_deployed = load_registry(latest_dir)
    print(f"Loading bench from {args.cache} ...")
    prompts, scores = load_bench(args.cache, bench_to_deployed)
    print(f"  {len(prompts)} prompts, {sum(len(v) for v in scores.values())} (prompt, model) cells")

    train_idx, held_idx = split_holdout(prompts, args.holdout_frac, args.seed)
    held_prompts = [prompts[i] for i in held_idx]
    print(f"  split: {len(train_idx)} train / {len(held_prompts)} held-out (frac={args.holdout_frac}, seed={args.seed})")

    if args.limit > 0 and args.limit < len(held_prompts):
        rng = np.random.default_rng(args.seed)
        sample_idx = rng.choice(len(held_prompts), size=args.limit, replace=False)
        held_prompts = [held_prompts[i] for i in sample_idx]
        print(f"  limit: subsampled to {len(held_prompts)} held-out prompts")

    # Embed held-out prompts once; reuse across all versions (centroid
    # geometry is per-version but the embedder is shared).
    # Disk cache. The embedder is a fixed input (committed
    # model.onnx + tokenizer.json) and the held-out prompt set is
    # deterministic on (holdout_frac, seed, bench cache contents).
    # Hash all three, store (N, 768) float32 under
    # scripts/.embedding-cache/<hash>.npy, and reload on subsequent
    # runs. INT8 BERT-base on M-series CPU is the bottleneck at
    # ~70ms/prompt; 3000 prompts × 70ms = 3.5 min per retrain
    # iteration. Caching collapses that to ~50ms after first run.
    cache_dir = Path(__file__).resolve().parent / ".embedding-cache"
    cache_dir.mkdir(exist_ok=True)
    cache_key = hashlib.sha256()
    for p in held_prompts:
        cache_key.update(p.encode("utf-8"))
        cache_key.update(b"\0")
    for asset_name in ("model.onnx", "tokenizer.json"):
        asset_path = args.assets / asset_name
        if asset_path.exists():
            stat = asset_path.stat()
            cache_key.update(
                f"{asset_name}:{stat.st_size}-{int(stat.st_mtime)}".encode()
            )
    cache_path = cache_dir / f"{cache_key.hexdigest()[:16]}.npy"

    if cache_path.exists():
        embed_start = time.monotonic()
        embeddings = np.load(cache_path)
        embed_ms = (time.monotonic() - embed_start) * 1000
        print(f"Embedding cache hit: {cache_path.name} "
              f"(loaded {embeddings.shape[0]} × {embeddings.shape[1]} in {embed_ms:.0f}ms)")
    else:
        print(f"Loading embedder from {args.assets} (cache miss; will write {cache_path.name}) ...")
        sess, tok, input_names, output_name = load_embedder(args.assets, use_coreml=False, dynamic_padding=True)
        print(f"Embedding {len(held_prompts)} prompts (batch_size={args.batch_size}) ...")
        chunks = []
        n_batches = (len(held_prompts) + args.batch_size - 1) // args.batch_size
        embed_start = time.monotonic()
        for i in range(0, len(held_prompts), args.batch_size):
            batch = held_prompts[i : i + args.batch_size]
            chunks.append(embed_batch(sess, tok, input_names, output_name, batch))
            b = i // args.batch_size + 1
            if b == 1 or b % 10 == 0 or b == n_batches:
                print(f"  batch {b}/{n_batches} done", flush=True)
        embed_ms = (time.monotonic() - embed_start) * 1000
        embeddings = np.vstack(chunks) if chunks else np.empty((0, EMBED_DIM), dtype=np.float32)
        per_prompt = embed_ms / max(1, len(held_prompts))
        np.save(cache_path, embeddings)
        print(f"  embedded {len(held_prompts)} prompts in {embed_ms:.0f}ms "
              f"({per_prompt:.1f}ms/prompt, {1000/per_prompt:.0f}/s); cached to "
              f"{cache_path.name}")

    # Pre-load every requested version's bundle once so the regret loop
    # and the routing-distribution print below share the same picks
    # without re-running simulate_cluster_route or re-reading rankings.
    version_bundles: Dict[str, Tuple[np.ndarray, list, list, Dict[str, float]]] = {}
    version_picks: Dict[str, List[str]] = {}
    for v in versions:
        centroids, rankings, entries, cost = load_artifact_bundle(v)
        candidate_models = sorted({e["model"] for e in entries})
        version_bundles[v] = (centroids, rankings, entries, cost)
        version_picks[v] = simulate_cluster_route(
            embeddings, centroids, rankings, candidate_models, top_p=4
        )

    # Build the always-X baseline pool from the union across requested
    # versions — every model that any version knows about. Cost dict
    # from the latest version (operators bumping prices retrain, so
    # latest is canonical).
    _, _, latest_entries, latest_cost = load_artifact_bundle(latest)
    all_models = sorted({
        e["model"]
        for v in versions
        for e in version_bundles[v][2]
    } | {e["model"] for e in latest_entries})

    rows: List[dict] = []

    # Always-X baselines.
    for m in all_models:
        rows.append(evaluate_router(
            f"always-{m}",
            lambda _p, m=m: m,
            held_prompts,
            scores,
            latest_cost,
        ))

    # Oracle (upper bound — picks the best model per prompt).
    rows.append(evaluate_router(
        "ORACLE",
        lambda p: max(scores.get(p, {"_": 0.0}), key=lambda k: scores[p][k]) if scores.get(p) else "_",
        held_prompts,
        scores,
        latest_cost,
    ))

    # Each cluster artifact version.
    for v in versions:
        _, _, entries, cost = version_bundles[v]
        candidate_models = sorted({e["model"] for e in entries})
        picks = version_picks[v]
        prompt_to_pick = {p: picks[i] for i, p in enumerate(held_prompts)}
        rows.append(evaluate_router(
            f"{v}-cluster",
            lambda p, ptp=prompt_to_pick, fallback=candidate_models[0]: ptp.get(p, fallback),
            held_prompts,
            scores,
            cost,
        ))

    # Surface routing-decision distribution per cluster version — useful
    # alongside the regret table to spot "v0.5 routes to gemini-3-flash
    # 80% of the time" before reading the score columns.
    print()
    print("Per-version routing-decision distribution (held-out picks):")
    for v in versions:
        picks = version_picks[v]
        counter: Dict[str, int] = defaultdict(int)
        for m in picks:
            counter[m] += 1
        total = sum(counter.values())
        rendered = ", ".join(
            f"{m}={counter[m]} ({100*counter[m]/total:.0f}%)"
            for m in sorted(counter, key=lambda k: -counter[k])
        )
        print(f"  {v}: {rendered}")

    print()
    print(f"Held-out regret (n={len(held_prompts)}, holdout_frac={args.holdout_frac}, seed={args.seed}):")
    print(render_table(rows))
    print()
    print("Lower regret = closer to oracle picks. Cost is per-prompt input-token")
    print("estimate (chars/4 × cost_per_1k); ranking is fair, absolute $ is approximate.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
