"""Sweep K-means cluster counts to choose K for train_cluster_router.py.

The AvengersPro paper used K=60 with N=8 models on ~2,600 prompts. We
have N=3 deployed models at P0, so K=60 likely produces many cells with
identical "Opus > Sonnet > Haiku" rankings — defeating the routing
premise. This script runs K-means at K∈{10, 20, 40, 60} on
OpenRouterBench prompts re-embedded with the *INT8-quantized* Jina v2
ONNX (the same model the router will use at runtime — embedder parity
between training and inference is load-bearing) and reports for each K
the percentage of (cluster, model)-rankings that are *distinct* — i.e.
have a different top-1 model than the global "Opus first" ordering.

Pick the smallest K with ≥80% distinct top-1 cells (per
router/docs/plans/archive/CLUSTER_ROUTING_PLAN.md decisions table). Update
router/docs/plans/archive/CLUSTER_ROUTING_PLAN.md's "Cluster count K" row with the chosen value.

Usage:
    cd router/scripts && poetry run python sweep_cluster_k.py \
        [--cache .bench-cache] [--ks 10,20,40,60] [--sample-size 2600] [--seed 42]

Output: stdout summary table; no on-disk artifacts (this is a
diagnostic, not a runtime input). Run before train_cluster_router.py.
"""

from __future__ import annotations

import argparse
import json
import sys
from collections import Counter, defaultdict
from pathlib import Path
from typing import Dict, List, Tuple

import numpy as np
import onnxruntime as ort
from sklearn.cluster import KMeans
from tokenizers import Tokenizer

from bench_walker import load_bench

ROUTER_ROOT = Path(__file__).resolve().parents[1]
ASSETS_DIR = ROUTER_ROOT / "internal/router/cluster/assets"
CLUSTER_DIR = ROUTER_ROOT / "internal/router/cluster"
ARTIFACTS_DIR = CLUSTER_DIR / "artifacts"
LATEST_POINTER = ARTIFACTS_DIR / "latest"
EMBED_DIM = 768


def read_latest() -> str:
    """Return the version named in artifacts/latest. Mirror of
    train_cluster_router.read_latest; sweep_cluster_k uses the same
    pointer so a sweep is run against the same registry the next
    training run will use."""
    if not LATEST_POINTER.exists():
        sys.exit(
            f"ERROR: {LATEST_POINTER} is missing. Run train_cluster_router.py "
            f"once to establish a baseline, or recreate the pointer manually "
            f"(e.g. echo v0.2 > {LATEST_POINTER})."
        )
    raw = LATEST_POINTER.read_text().strip()
    if not raw:
        sys.exit(f"ERROR: {LATEST_POINTER} is empty.")
    return raw

# Jina v2 base-code's max input length. Runtime tail-truncates prompts
# to ~256 tokens for latency; we mirror that here so cluster geometry
# trains on the same shape it'll see in production.
MAX_TOKENS = 256


def load_embedder(assets_dir: Path, use_coreml: bool = False):
    """Load the INT8-quantized ONNX + tokenizer the runtime uses.
    Returns (session, tokenizer, input_names, output_name).

    See train_cluster_router.load_embedder for the CoreML parity caveat;
    sweep_cluster_k only computes K candidates and never writes the
    artifact, so CoreML is safe to use here regardless of the production
    execution provider.
    """
    onnx_path = assets_dir / "model.onnx"
    tokenizer_path = assets_dir / "tokenizer.json"
    if not onnx_path.exists():
        sys.exit(f"ERROR: {onnx_path} missing — run scripts/download_from_hf.py")
    if not tokenizer_path.exists():
        sys.exit(f"ERROR: {tokenizer_path} missing — run scripts/download_from_hf.py")

    sess_opts = ort.SessionOptions()
    sess_opts.graph_optimization_level = ort.GraphOptimizationLevel.ORT_ENABLE_ALL
    if use_coreml:
        provider_list = ["CoreMLExecutionProvider", "CPUExecutionProvider"]
    else:
        provider_list = ["CPUExecutionProvider"]
    sess = ort.InferenceSession(str(onnx_path), sess_opts, providers=provider_list)
    active = sess.get_providers()
    print(f"  ONNX execution providers: requested={provider_list} active={active}")
    if use_coreml and "CoreMLExecutionProvider" not in active:
        print("  WARNING: --coreml requested but CoreMLExecutionProvider not active; falling back to CPU.")

    tok = Tokenizer.from_file(str(tokenizer_path))
    tok.enable_truncation(max_length=MAX_TOKENS)
    tok.enable_padding(length=MAX_TOKENS)

    input_names = {inp.name for inp in sess.get_inputs()}
    output_name = sess.get_outputs()[0].name
    return sess, tok, input_names, output_name


def embed_batch(
    sess: ort.InferenceSession,
    tok: Tokenizer,
    input_names: set,
    output_name: str,
    texts: List[str],
) -> np.ndarray:
    """Embed a batch via the BERT-style ONNX. Mean-pool over the
    attention mask, then L2-normalize to match
    sentence-transformers and the Go runtime contract.
    """
    encs = tok.encode_batch(texts)
    input_ids = np.array([e.ids for e in encs], dtype=np.int64)
    attention_mask = np.array([e.attention_mask for e in encs], dtype=np.int64)
    feed: Dict[str, np.ndarray] = {"input_ids": input_ids, "attention_mask": attention_mask}
    if "token_type_ids" in input_names:
        feed["token_type_ids"] = np.zeros_like(input_ids)

    last_hidden = sess.run([output_name], feed)[0]  # (B, T, H)
    # Mean-pool over the masked tokens.
    mask = attention_mask[:, :, None].astype(last_hidden.dtype)
    summed = (last_hidden * mask).sum(axis=1)
    counts = mask.sum(axis=1).clip(min=1)
    pooled = summed / counts
    # L2 normalize.
    norms = np.linalg.norm(pooled, axis=1, keepdims=True)
    norms = np.where(norms == 0, 1.0, norms)
    return (pooled / norms).astype(np.float32)


def load_registry(version: str) -> Tuple[List[Dict[str, object]], Dict[str, List[str]]]:
    """Read model_registry.json from a versioned artifact directory and
    return:
      * the flat list of deployed entries
      * the bench_column → [deployed_model, ...] map used by load_bench.
    Multiple deployed entries may reference the same bench column
    (e.g. proxy entries that reuse a frontier column's scores).
    """
    registry_path = ARTIFACTS_DIR / version / "model_registry.json"
    if not registry_path.exists():
        sys.exit(f"ERROR: {registry_path} is missing.")
    raw = registry_path.read_text()
    parsed = json.loads(raw)
    entries = parsed.get("deployed_models", [])
    if not entries:
        sys.exit("model_registry.json: deployed_models is empty")
    if not isinstance(entries, list):
        sys.exit("model_registry.json: deployed_models must be a JSON array")
    bench_to_deployed: Dict[str, List[str]] = defaultdict(list)
    for i, e in enumerate(entries):
        if not isinstance(e, dict):
            sys.exit(f"model_registry.json: deployed_models[{i}] is not an object")
        for required in ("model", "provider", "bench_column"):
            if not e.get(required):
                sys.exit(f"model_registry.json: deployed_models[{i}] missing {required!r}")
        bench_to_deployed[e["bench_column"]].append(e["model"])
    return entries, dict(bench_to_deployed)


def evaluate_k(
    embeddings: np.ndarray,
    score_matrix: Dict[str, Dict[str, float]],
    prompts: List[str],
    deployed_models: List[str],
    k: int,
    seed: int,
) -> Dict[str, float]:
    """Run K-means at this K, aggregate per-(cluster, model) means,
    and report what % of cluster cells have a non-default top-1.

    "Default" = the global majority top-1 model when scores are
    averaged across all prompts. If every cluster's top-1 equals the
    global top-1, the cluster scorer is effectively always-pick-the-
    best-model — same as routing without clustering.
    """
    km = KMeans(n_clusters=k, n_init=10, random_state=seed)
    labels = km.fit_predict(embeddings)

    # Per-(cluster, model) sum + count → mean.
    cell_sum: Dict[int, Dict[str, float]] = defaultdict(lambda: defaultdict(float))
    cell_n: Dict[int, Dict[str, int]] = defaultdict(lambda: defaultdict(int))
    for idx, prompt in enumerate(prompts):
        cl = int(labels[idx])
        for m, s in score_matrix.get(prompt, {}).items():
            cell_sum[cl][m] += s
            cell_n[cl][m] += 1

    # Compute global per-model means as the "default ranking" baseline.
    global_sum: Dict[str, float] = defaultdict(float)
    global_n: Dict[str, int] = defaultdict(int)
    for cl, ms in cell_sum.items():
        for m, total in ms.items():
            global_sum[m] += total
            global_n[m] += cell_n[cl][m]
    global_mean = {m: (global_sum[m] / global_n[m]) for m in global_sum if global_n[m] > 0}
    global_top1 = max(global_mean, key=global_mean.get) if global_mean else None

    distinct_cells = 0
    total_cells = 0
    cluster_top1: Counter[str] = Counter()
    for cl, ms in cell_sum.items():
        cell_mean = {m: (ms[m] / cell_n[cl][m]) for m in ms if cell_n[cl][m] > 0}
        if not cell_mean:
            continue
        total_cells += 1
        top = max(cell_mean, key=cell_mean.get)
        cluster_top1[top] += 1
        if top != global_top1:
            distinct_cells += 1

    return {
        "k": k,
        "cells": total_cells,
        "distinct_cells": distinct_cells,
        "distinct_pct": (distinct_cells / total_cells * 100.0) if total_cells > 0 else 0.0,
        "global_top1": global_top1,
        "cluster_top1_distribution": dict(cluster_top1),
        "inertia": float(km.inertia_),
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cache", type=Path, default=Path(__file__).resolve().parent / ".bench-cache")
    parser.add_argument("--assets", type=Path, default=ASSETS_DIR)
    parser.add_argument(
        "--ks",
        type=lambda s: [int(x) for x in s.split(",")],
        default=[10, 20, 40, 60],
    )
    parser.add_argument("--sample-size", type=int, default=0,
                        help="If >0, randomly subsample the prompt set to this size for faster sweeps.")
    parser.add_argument("--seed", type=int, default=42)
    parser.add_argument("--batch-size", type=int, default=32)
    parser.add_argument("--coreml", action="store_true",
                        help="Use Apple CoreML ONNX provider for ~3-5x faster sweeps on M-series Macs. Safe here — sweep doesn't write production artifacts.")
    parser.add_argument("--version", default=None,
                        help=("Versioned registry to sweep against (e.g. v0.2). "
                              "Defaults to artifacts/latest. Use --version v0.X "
                              "when sweeping a candidate registry that hasn't "
                              "been promoted yet."))
    args = parser.parse_args()

    if not args.cache.exists():
        sys.exit(f"ERROR: {args.cache} missing — run scripts/download_bench.sh first")

    version = args.version or read_latest()
    print(f"Sweeping against registry artifacts/{version}/model_registry.json")
    entries, bench_to_deployed = load_registry(version)
    deployed_models = sorted({e["model"] for e in entries})
    print(f"Bench column → deployed mapping: {dict(bench_to_deployed)}")
    print(f"Deployed model set: {deployed_models}")

    print(f"Loading bench from {args.cache} ...")
    prompts, score_matrix = load_bench(args.cache, bench_to_deployed)
    print(f"  {len(prompts)} unique prompts; {len(score_matrix)} have ≥1 deployed-model score")

    if args.sample_size > 0 and args.sample_size < len(prompts):
        rng = np.random.default_rng(args.seed)
        idxs = rng.choice(len(prompts), size=args.sample_size, replace=False)
        prompts = [prompts[i] for i in idxs]
        print(f"  subsampled to {len(prompts)} prompts (seed={args.seed})")

    print(f"Loading embedder from {args.assets} ...")
    sess, tok, input_names, output_name = load_embedder(args.assets, use_coreml=args.coreml)

    print(f"Embedding {len(prompts)} prompts (batch_size={args.batch_size}) ...")
    chunks = []
    for i in range(0, len(prompts), args.batch_size):
        batch = prompts[i : i + args.batch_size]
        chunks.append(embed_batch(sess, tok, input_names, output_name, batch))
        if (i // args.batch_size) % 20 == 0:
            print(f"  batch {i // args.batch_size + 1}/{(len(prompts) + args.batch_size - 1) // args.batch_size}")
    embeddings = np.vstack(chunks)
    print(f"  embeddings shape: {embeddings.shape}")

    print()
    print(f"=== K sweep (seed={args.seed}, n_init=10) ===")
    print(f"{'K':>4}  {'cells':>6}  {'distinct':>8}  {'%distinct':>10}  {'global_top1':40s}  {'inertia':>10}")
    print("-" * 100)
    rows = []
    for k in args.ks:
        if k > len(prompts):
            print(f"  K={k} skipped (more than prompts={len(prompts)})")
            continue
        result = evaluate_k(embeddings, score_matrix, prompts, deployed_models, k, args.seed)
        rows.append(result)
        print(
            f"{result['k']:>4}  {result['cells']:>6}  {result['distinct_cells']:>8}  "
            f"{result['distinct_pct']:>9.1f}%  {str(result['global_top1']):40s}  {result['inertia']:>10.1f}"
        )

    # Recommendation: smallest K with ≥80% distinct.
    qualifying = [r for r in rows if r["distinct_pct"] >= 80.0]
    print()
    if qualifying:
        chosen = min(qualifying, key=lambda r: r["k"])
        print(f"RECOMMENDATION: K={chosen['k']} ({chosen['distinct_pct']:.1f}% distinct cells)")
        print()
        print("Cluster top-1 distribution at recommended K:")
        for m, c in sorted(chosen["cluster_top1_distribution"].items(), key=lambda kv: -kv[1]):
            print(f"  {m:40s} {c:>4} clusters")
    elif rows:
        best = max(rows, key=lambda r: r["distinct_pct"])
        print(
            f"WARNING: no K reached ≥80% distinct cells. Best was K={best['k']} at {best['distinct_pct']:.1f}%."
        )
        print("Consider widening the K range or revisiting the N-model subset (the geometry may not")
        print("encode enough between-model variation with only 3 deployed Anthropic models).")
    else:
        print("WARNING: no K values were evaluated (empty K range or all evaluations failed).")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
