"""train_per_cluster_alpha.py — Phase 4: per-cluster α retrain.

Reuses an existing version's cluster geometry (centroids), refits the
α blend independently per cluster to land on a tighter Pareto. Each
cluster ends up with its own α_k chosen by minimizing a regret + λ·cost
penalty on the training split — easy clusters drift toward α_k → 0
(cheap-favoring) where many models tie on quality; hard clusters stay
near α_k → 1 (quality-favoring) where the expensive model actually
matters.

Doesn't re-cluster: K-means geometry is inherited from ``--from``. The
only artifacts that change are rankings.json (per-cluster α-blend rows)
and metadata.yaml (records each α_k for provenance). centroids.bin and
model_registry.json are copied byte-for-byte from the parent so the
runtime serves the same prompt-to-cluster mapping.

Usage:
    cd router/scripts

    # Auto-bump from latest, λ_cost = 0.0 (default; matches shipped v0.6)
    poetry run python train_per_cluster_alpha.py --from v0.5

    # Explicit target, no promotion (compare against parent first)
    poetry run python train_per_cluster_alpha.py \\
        --from v0.5 --version v0.6 --no-promote

    # Sweep λ_cost to explore the Pareto
    for L in 0.0 0.02 0.05 0.10; do
        poetry run python train_per_cluster_alpha.py \\
            --from v0.5 --version v0.6-l$L --lambda-cost $L --no-promote
    done

After training, validate with:
    poetry run python holdout_eval.py --versions v0.5,v0.6

Tuning λ_cost:
    0.00  default; pure quality (≈ global α=1.0). Matches the shipped
          v0.6 artifact (regret 0.135) — each cluster picks its
          quality-best model regardless of cost.
    0.02  light cost weight; near-Pareto-optimal at high quality.
    0.05  empirically regressed regret to 0.156 — keep for the
          Pareto sweep but don't ship as default.
    0.10+ aggressive; shifts substantial traffic to Haiku / Flash-Lite
          on easy clusters, accepts some quality regression on hard
          clusters.

Caveats:
  * The fit is on bench scores — same caveat as ``holdout_eval.py``:
    in-distribution because cell aggregation runs on the same prompts
    regret is later measured against. Treat absolute regret as
    optimistic; the cluster-vs-cluster comparison is fair because each
    α_k is fit on the same train slice and evaluated on the same held-
    out slice.
  * The score normalization, shrinkage prior, and embedder match
    train_cluster_router.py exactly so the only thing that varies vs
    the parent bundle is the α value per cluster row.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import shutil
import sys
import time
from collections import defaultdict
from pathlib import Path
from typing import Dict, List, Optional, Tuple

import numpy as np

from bench_walker import load_bench
from holdout_eval import load_centroids_bin, split_holdout
from train_cluster_router import (
    ARTIFACTS_DIR,
    ASSETS_DIR,
    DEFAULT_ALPHA,
    DEFAULT_COST_PER_1K_INPUT,
    DEFAULT_SHRINKAGE_K0,
    EMBED_DIM,
    LATEST_POINTER,
    MAX_TOKENS,
    _today_iso,
    aggregate_cells,
    embed_batch,
    load_embedder,
    load_registry,
    next_version,
    parse_version,
    read_latest,
    shrink_to_prior,
    write_metadata_yaml,
    write_rankings,
)


# 21 candidate α values: 0.00, 0.05, …, 1.00. Fine enough to find the
# right tradeoff per cluster, coarse enough that the inner loop stays
# in the millisecond range across all clusters.
ALPHA_GRID = np.linspace(0.0, 1.0, 21)

# Default cost penalty weight in the per-cluster objective:
#   loss(α_k) = mean_regret(α_k) + λ_cost · mean_cost_usd_per_prompt(α_k)
#
# Set to 0.0 to match the shipped v0.6 artifact (regret 0.135). The
# λ_cost=0.05 sweep regressed regret to 0.156 (see ROUTER_V1_PLAN.md
# §4), so the empirical winner is pure quality. Tune via --lambda-cost
# if you want to sweep the Pareto.
DEFAULT_LAMBDA_COST = 0.0

# Same default top-p the runtime scorer uses. Centroids inherited from
# the parent already use this assumption; included here for the metadata
# block so v0.6's bundle is self-describing.
DEFAULT_TOP_P = 4


def estimate_input_tokens(prompt: str) -> int:
    """Match holdout_eval.py's chars/4 proxy. Used only to compute the
    cost term inside the per-cluster objective, where consistency with
    the validator matters more than absolute accuracy."""
    return max(1, len(prompt) // 4)


def alpha_blend_cluster(
    cell_means: Dict[str, float],
    cost_per_1k: Dict[str, float],
    alpha: float,
    deployed_models: List[str],
) -> Dict[str, float]:
    """Single-cluster α blend matching ``train_cluster_router.alpha_blend``
    exactly — duplicated here because we need to call it once per
    candidate α_k inside the fit loop, and the multi-cluster version
    rebuilds normalization bounds for every cluster row each call.
    """
    q_vals = [cell_means[m] for m in deployed_models if m in cell_means]
    if not q_vals:
        return {}
    q_min, q_max = min(q_vals), max(q_vals)
    q_range = q_max - q_min
    c_vals = [cost_per_1k[m] for m in deployed_models if m in cell_means]
    c_min, c_max = min(c_vals), max(c_vals)
    c_range = c_max - c_min

    blended: Dict[str, float] = {}
    for m in deployed_models:
        if m not in cell_means:
            continue
        q_norm = (cell_means[m] - q_min) / q_range if q_range > 0 else 0.0
        c_norm = (cost_per_1k[m] - c_min) / c_range if c_range > 0 else 0.0
        blended[m] = alpha * q_norm + (1.0 - alpha) * (1.0 - c_norm)
    return blended


def fit_cluster_alpha(
    cluster_prompts: List[str],
    scores: Dict[str, Dict[str, float]],
    cell_means: Dict[str, float],
    cost_per_1k: Dict[str, float],
    deployed_models: List[str],
    lambda_cost: float,
) -> Tuple[float, float, float, Optional[str]]:
    """Sweep α over ALPHA_GRID, return (best_α, mean_regret, mean_cost,
    chosen_model). chosen_model is the argmax model under best_α — when
    the cluster has training prompts to score against, that's the
    actual pick the runtime would emit; when the cluster is empty (no
    train prompts assigned to it), we still produce a valid α_k from
    the cell aggregates but report regret/cost as 0 because there's
    nothing to evaluate against.

    Falls back to (DEFAULT_ALPHA, 0, 0, None) when cell_means is empty
    (no observed models in the cluster after shrinkage). Caller should
    log this — it implies a centroid that the bench can't score.
    """
    if not cell_means:
        return DEFAULT_ALPHA, 0.0, 0.0, None

    # Sort once; argmax tie-breaking matches the Go scorer (lex order
    # over the candidate list).
    sorted_candidates = sorted(deployed_models)

    # Pre-compute per-prompt oracle quality + cost-per-prompt-per-model
    # so the inner sweep is just an argmax + lookup loop.
    train_data: List[Tuple[Dict[str, float], float, Dict[str, float]]] = []
    for p in cluster_prompts:
        per_model = scores.get(p, {})
        if not per_model:
            continue
        oracle = max(per_model.values())
        cost_for_prompt = {
            m: cost_per_1k[m] * estimate_input_tokens(p) / 1000.0
            for m in deployed_models
        }
        train_data.append((per_model, oracle, cost_for_prompt))

    if not train_data:
        # Cluster has prompts but none with bench scores. Fall back to
        # the global default α; metadata.yaml will record the cluster's
        # n_train as 0 so the operator can spot it.
        return DEFAULT_ALPHA, 0.0, 0.0, None

    best_alpha = DEFAULT_ALPHA
    best_loss = float("inf")
    best_regret = 0.0
    best_cost = 0.0
    best_model: Optional[str] = None

    for alpha in ALPHA_GRID:
        blended = alpha_blend_cluster(cell_means, cost_per_1k, alpha, deployed_models)
        if not blended:
            continue
        # Argmax with deterministic lex tiebreak.
        chosen = sorted_candidates[0]
        chosen_score = float("-inf")
        for m in sorted_candidates:
            if m in blended and blended[m] > chosen_score:
                chosen, chosen_score = m, blended[m]

        total_regret = 0.0
        total_cost = 0.0
        for per_model, oracle, cost_for_prompt in train_data:
            achieved = per_model.get(chosen, 0.0)
            total_regret += oracle - achieved
            total_cost += cost_for_prompt[chosen]
        n = len(train_data)
        mean_regret = total_regret / n
        mean_cost = total_cost / n
        loss = mean_regret + lambda_cost * mean_cost

        if loss < best_loss:
            best_loss = loss
            best_alpha = float(alpha)
            best_regret = mean_regret
            best_cost = mean_cost
            best_model = chosen

    return best_alpha, best_regret, best_cost, best_model


def embed_with_cache(
    prompts: List[str],
    assets_dir: Path,
    batch_size: int,
) -> np.ndarray:
    """Reuse the cache key shape from holdout_eval.py so a rerun of
    train + validate hits the same .embedding-cache entry. Eliminates
    the ~3 minute embedding step on warm runs."""
    cache_dir = Path(__file__).resolve().parent / ".embedding-cache"
    cache_dir.mkdir(exist_ok=True)
    h = hashlib.sha256()
    for p in prompts:
        h.update(p.encode("utf-8"))
        h.update(b"\0")
    for asset_name in ("model.onnx", "tokenizer.json"):
        asset_path = assets_dir / asset_name
        if asset_path.exists():
            stat = asset_path.stat()
            h.update(f"{asset_name}:{stat.st_size}-{int(stat.st_mtime)}".encode())
    cache_path = cache_dir / f"{h.hexdigest()[:16]}.npy"

    if cache_path.exists():
        t = time.monotonic()
        embeddings = np.load(cache_path)
        ms = (time.monotonic() - t) * 1000
        print(f"  cache hit: {cache_path.name} ({embeddings.shape} in {ms:.0f}ms)")
        return embeddings

    print(f"  cache miss; embedding {len(prompts)} prompts → {cache_path.name}")
    sess, tok, input_names, output_name = load_embedder(
        assets_dir, use_coreml=False, dynamic_padding=True
    )
    chunks: List[np.ndarray] = []
    n_batches = (len(prompts) + batch_size - 1) // batch_size
    t = time.monotonic()
    for i in range(0, len(prompts), batch_size):
        batch = prompts[i : i + batch_size]
        chunks.append(embed_batch(sess, tok, input_names, output_name, batch))
        b = i // batch_size + 1
        if b == 1 or b % 10 == 0 or b == n_batches:
            print(f"    batch {b}/{n_batches}", flush=True)
    embeddings = np.vstack(chunks)
    ms = (time.monotonic() - t) * 1000
    np.save(cache_path, embeddings)
    print(f"  embedded in {ms:.0f}ms ({ms / max(1, len(prompts)):.1f}ms/prompt)")
    return embeddings


def main() -> int:
    parser = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument(
        "--cache", type=Path, default=Path(__file__).resolve().parent / ".bench-cache"
    )
    parser.add_argument("--assets", type=Path, default=ASSETS_DIR)
    parser.add_argument(
        "--from",
        dest="parent",
        required=True,
        help="Parent version to inherit centroids + registry from (e.g. v0.5).",
    )
    parser.add_argument(
        "--version",
        default=None,
        help="Target artifact version directory. Defaults to auto-bump of artifacts/latest.",
    )
    parser.add_argument(
        "--lambda-cost",
        type=float,
        default=DEFAULT_LAMBDA_COST,
        help=f"Cost penalty weight in the per-cluster objective. Default {DEFAULT_LAMBDA_COST}.",
    )
    parser.add_argument(
        "--holdout-frac",
        type=float,
        default=0.2,
        help="Fraction of prompts reserved for holdout (not used to fit α_k).",
    )
    parser.add_argument("--seed", type=int, default=42)
    parser.add_argument("--batch-size", type=int, default=32)
    parser.add_argument(
        "--no-promote",
        action="store_true",
        help="Skip updating artifacts/latest. Default promotes.",
    )
    parser.add_argument(
        "--notes", default="", help="Free-form changelog text for metadata.yaml."
    )
    args = parser.parse_args()

    if not (0.0 < args.holdout_frac < 1.0):
        sys.exit("--holdout-frac must be in (0, 1)")
    if args.lambda_cost < 0:
        sys.exit("--lambda-cost must be >= 0")

    target_version = args.version or next_version()
    parse_version(target_version)
    target_dir = ARTIFACTS_DIR / target_version
    # Refuse to write into an existing version dir — committed
    # artifacts are immutable bundles. Re-running into an existing
    # version would silently mutate rankings.json / metadata.yaml
    # while keeping the version label, defeating the per-version
    # comparison the eval harness relies on.
    if target_dir.exists() and any(target_dir.iterdir()):
        sys.exit(
            f"ERROR: artifact dir {target_dir} already exists and is non-empty. "
            f"Bump --version or remove the directory manually if you intended to retrain."
        )
    target_dir.mkdir(parents=True, exist_ok=True)

    parent_dir = ARTIFACTS_DIR / args.parent
    if not parent_dir.exists():
        sys.exit(f"ERROR: parent version dir {parent_dir} missing")
    parent_centroids = parent_dir / "centroids.bin"
    parent_registry = parent_dir / "model_registry.json"
    if not parent_centroids.exists() or not parent_registry.exists():
        sys.exit(
            f"ERROR: {args.parent} bundle is incomplete (need centroids.bin + model_registry.json)"
        )

    # Copy registry + centroids into the new bundle. Refuse to clobber
    # an existing registry — operators editing a registry mid-train
    # should land that as a separate commit so the diff is clear.
    target_registry = target_dir / "model_registry.json"
    if target_registry.exists():
        if target_registry.read_text() != parent_registry.read_text():
            sys.exit(
                f"ERROR: {target_registry} exists and differs from parent's. "
                f"Remove it manually if you intended to inherit."
            )
    else:
        target_registry.write_text(parent_registry.read_text())
        print(f"Copied registry from {args.parent} → {target_version}")

    target_centroids = target_dir / "centroids.bin"
    shutil.copyfile(parent_centroids, target_centroids)
    print(f"Copied centroids from {args.parent} → {target_version}")

    print(
        f"Phase 4 retrain: {args.parent} → {target_version} (λ_cost={args.lambda_cost})"
    )
    entries, bench_to_deployed = load_registry(target_dir)
    deployed_models = sorted({e["model"] for e in entries})
    deployed_to_provider = {e["model"]: e["provider"] for e in entries}
    if any(m not in DEFAULT_COST_PER_1K_INPUT for m in deployed_models):
        missing = [m for m in deployed_models if m not in DEFAULT_COST_PER_1K_INPUT]
        sys.exit(
            f"ERROR: missing cost values for {missing}. Update train_cluster_router.py first."
        )
    cost_per_1k = {m: DEFAULT_COST_PER_1K_INPUT[m] for m in deployed_models}

    centroids = load_centroids_bin(target_centroids)
    K = centroids.shape[0]
    print(f"  inherited K={K} centroids from {args.parent}")

    print(f"Loading bench from {args.cache} ...")
    prompts, scores = load_bench(args.cache, bench_to_deployed)
    print(
        f"  {len(prompts)} prompts, {sum(len(v) for v in scores.values())} (prompt, model) cells"
    )

    train_idx, held_idx = split_holdout(prompts, args.holdout_frac, args.seed)
    train_prompts = [prompts[i] for i in train_idx]
    print(
        f"  split: {len(train_prompts)} train / {len(held_idx)} holdout (frac={args.holdout_frac}, seed={args.seed})"
    )

    print("Embedding train prompts ...")
    train_embeddings = embed_with_cache(train_prompts, args.assets, args.batch_size)

    # Cluster assignment via cosine (centroids are unit-norm post-train).
    sims = train_embeddings @ centroids.T
    train_labels = np.argmax(sims, axis=1)

    print("Aggregating per-(cluster, model) cells on train slice ...")
    raw_means, raw_counts = aggregate_cells(
        train_labels, train_prompts, scores, deployed_models, K
    )
    cells = shrink_to_prior(
        raw_means, raw_counts, deployed_models, k0=DEFAULT_SHRINKAGE_K0
    )

    train_prompts_by_cluster: Dict[int, List[str]] = defaultdict(list)
    for i, p in enumerate(train_prompts):
        train_prompts_by_cluster[int(train_labels[i])].append(p)

    print()
    print(
        f"Sweeping α per cluster (grid size {len(ALPHA_GRID)}, λ_cost={args.lambda_cost}) ..."
    )
    print(
        f"{'cluster':>7}  {'n_train':>7}  {'α_k':>5}  {'regret':>7}  {'cost($)':>9}  {'pick':<25}"
    )
    per_cluster_alphas: Dict[int, float] = {}
    per_cluster_blended: Dict[int, Dict[str, float]] = {}
    for k in range(K):
        cluster_train_prompts = train_prompts_by_cluster.get(k, [])
        cell_means = cells.get(k, {})
        alpha_k, regret_k, cost_k, model_k = fit_cluster_alpha(
            cluster_train_prompts,
            scores,
            cell_means,
            cost_per_1k,
            deployed_models,
            args.lambda_cost,
        )
        # Round to 10 decimals so np.linspace artifacts like
        # 0.15000000000000002 don't end up serialized into rankings.json
        # / metadata.yaml. ALPHA_GRID has step 0.05, far above this.
        per_cluster_alphas[k] = round(alpha_k, 10)
        per_cluster_blended[k] = alpha_blend_cluster(
            cell_means, cost_per_1k, alpha_k, deployed_models
        )
        print(
            f"{k:>7}  {len(cluster_train_prompts):>7}  {alpha_k:>5.2f}  "
            f"{regret_k:>7.3f}  ${cost_k:>8.4f}  {model_k or '-':<25}"
        )

    # α distribution summary so a quick eyeball over many clusters is
    # easy without re-reading the per-row table.
    alpha_arr = np.array(list(per_cluster_alphas.values()))
    print()
    print(
        f"α_k distribution: min={alpha_arr.min():.2f} median={np.median(alpha_arr):.2f} "
        f"max={alpha_arr.max():.2f} mean={alpha_arr.mean():.2f} std={alpha_arr.std():.2f}"
    )
    cheap_clusters = int((alpha_arr <= 0.30).sum())
    expensive_clusters = int((alpha_arr >= 0.70).sum())
    middle_clusters = K - cheap_clusters - expensive_clusters
    print(
        f"  cheap-tier (α_k ≤ 0.30): {cheap_clusters}  middle: {middle_clusters}  "
        f"quality-tier (α_k ≥ 0.70): {expensive_clusters}"
    )

    print()
    print(f"Writing rankings + metadata to {target_dir} ...")
    rankings_path = target_dir / "rankings.json"
    write_rankings(
        per_cluster_blended,
        rankings_path,
        meta={
            "router_version": "weave-router-v0.1-phase4",
            "embedder_model": "jina-v2-base-code-int8",
            "alpha_strategy": "per-cluster",
            "per_cluster_alphas": {
                str(k): float(v) for k, v in per_cluster_alphas.items()
            },
            "lambda_cost": args.lambda_cost,
            "shrinkage_k0": DEFAULT_SHRINKAGE_K0,
            "score_normalization": "per_prompt_minmax_across_bench_columns",
            "top_p": DEFAULT_TOP_P,
            "k": K,
            "seed": args.seed,
            "n_train_prompts": len(train_prompts),
            "training_data_mix": {"d1": 1.0, "d2": 0.0, "d3": 0.0},
            "cost_per_1k_input_usd": cost_per_1k,
            "parent_version": args.parent,
        },
    )

    deployed_providers = sorted({e["provider"] for e in entries})
    metadata = {
        "version": target_version,
        "parent": args.parent,
        "status": "latest" if not args.no_promote else "candidate",
        "promoted_date": _today_iso(),
        "embedder": {
            "model": "jina-v2-base-code-int8",
            "embed_dim": EMBED_DIM,
            "max_tokens": MAX_TOKENS,
        },
        "training": {
            "k": K,
            "top_p": DEFAULT_TOP_P,
            "alpha_strategy": "per-cluster",
            "per_cluster_alphas": {
                str(k): float(v) for k, v in per_cluster_alphas.items()
            },
            "lambda_cost": args.lambda_cost,
            "shrinkage_k0": DEFAULT_SHRINKAGE_K0,
            "score_normalization": "per_prompt_minmax_across_bench_columns",
            "seed": args.seed,
            "n_train_prompts": len(train_prompts),
            "holdout_frac": args.holdout_frac,
            "training_data_mix": {"d1": 1.0, "d2": 0.0, "d3": 0.0},
        },
        "deployed_providers": deployed_providers,
        "deployed_models": deployed_models,
        "cost_per_1k_input_usd": cost_per_1k,
        "changelog": args.notes
        or (
            f"Phase 4 per-cluster α retrain (centroids inherited from "
            f"{args.parent}, λ_cost={args.lambda_cost}). Easy clusters "
            f"drift α_k → 0 (cheap-tier picks); hard clusters stay near "
            f"α_k → 1 (quality-tier picks). Re-run "
            f"holdout_eval.py to verify Pareto vs {args.parent}."
        ),
    }
    write_metadata_yaml(target_dir / "metadata.yaml", metadata)

    if not args.no_promote:
        LATEST_POINTER.write_text(target_version + "\n")
        print(f"Promoted: latest → {target_version}")
    else:
        print(f"--no-promote: latest still points at {read_latest()}")

    print()
    print("Done. Validate with:")
    print(
        f"  poetry run python holdout_eval.py --versions {args.parent},{target_version}"
    )
    print(f"  cd .. && go test -tags onnx_integration ./internal/router/cluster/...")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
