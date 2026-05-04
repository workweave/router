"""Compare two cluster-router artifact pairs (centroids.bin + rankings.json).

Use after retraining with a different ONNX execution provider (e.g.
--coreml) to confirm the geometry and per-cluster argmax decisions
still match the CPU-trained baseline. Reports:
  * per-centroid cosine distance (CPU vs other)
  * mean / max / p95 cosine distance across clusters
  * per-cluster top-1 winner divergence (the production-relevant signal)
  * inertia and meta diff

Usage:
    poetry run python compare_artifacts.py /path/to/baseline /path/to/candidate

Each path must contain `centroids.bin` and `rankings.json` produced by
train_cluster_router.py.
"""

from __future__ import annotations

import argparse
import json
import struct
import sys
from pathlib import Path
from typing import Dict, List, Tuple

import numpy as np

EMBED_DIM = 768


def load_centroids(path: Path) -> np.ndarray:
    raw = path.read_bytes()
    if raw[:4] != b"CRT1":
        sys.exit(f"{path}: bad magic {raw[:4]!r}")
    version, k, dim = struct.unpack("<III", raw[4:16])
    if version != 1:
        sys.exit(f"{path}: unsupported version {version}")
    if dim != EMBED_DIM:
        sys.exit(f"{path}: dim {dim} != EMBED_DIM {EMBED_DIM}")
    floats = np.frombuffer(raw[16:], dtype="<f4")
    return floats.reshape(k, dim).copy()


def load_rankings(path: Path) -> Tuple[Dict[str, Dict[str, float]], dict]:
    doc = json.loads(path.read_text())
    return doc["rankings"], doc.get("meta", {})


def per_cluster_cosine_distance(a: np.ndarray, b: np.ndarray) -> np.ndarray:
    """Centroids should be unit-norm at training time, but force-normalize
    here so this script doesn't silently report inflated drift if a future
    training pass forgets the L2 normalization step.
    """
    a = a / np.maximum(np.linalg.norm(a, axis=1, keepdims=True), 1e-12)
    b = b / np.maximum(np.linalg.norm(b, axis=1, keepdims=True), 1e-12)
    return 1.0 - (a * b).sum(axis=1)


def per_cluster_top1(rankings: Dict[str, Dict[str, float]]) -> Dict[str, str]:
    out: Dict[str, str] = {}
    for cl, scores in rankings.items():
        out[cl] = max(scores, key=scores.get)
    return out


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("baseline", type=Path)
    parser.add_argument("candidate", type=Path)
    args = parser.parse_args()

    base_c = load_centroids(args.baseline / "centroids.bin")
    cand_c = load_centroids(args.candidate / "centroids.bin")
    if base_c.shape != cand_c.shape:
        sys.exit(
            f"shape mismatch: baseline {base_c.shape} vs candidate {cand_c.shape}; "
            f"K or dim differs — can't compare"
        )

    base_r, base_meta = load_rankings(args.baseline / "rankings.json")
    cand_r, cand_meta = load_rankings(args.candidate / "rankings.json")

    print("=== centroid geometry ===")
    # K-means is rotation-invariant — re-running with a different embedder
    # path can produce the same partition with cluster-IDs permuted. Match
    # each baseline centroid to its nearest candidate by greedy cosine
    # similarity before reporting per-centroid drift, so a pure label
    # permutation doesn't read as catastrophic divergence.
    base_n = base_c / np.maximum(np.linalg.norm(base_c, axis=1, keepdims=True), 1e-12)
    cand_n = cand_c / np.maximum(np.linalg.norm(cand_c, axis=1, keepdims=True), 1e-12)
    sim = base_n @ cand_n.T
    used = set()
    perm: List[int] = []
    for i in range(base_c.shape[0]):
        order = np.argsort(-sim[i])
        for j in order:
            if int(j) not in used:
                perm.append(int(j))
                used.add(int(j))
                break
    perm_arr = np.asarray(perm)
    matched = cand_c[perm_arr]
    distances = per_cluster_cosine_distance(base_c, matched)
    is_identity = perm == list(range(base_c.shape[0]))
    print(f"  K = {base_c.shape[0]}, dim = {base_c.shape[1]}")
    print(f"  matched permutation: {perm}{'  (identity)' if is_identity else ''}")
    print(f"  per-cluster cosine distance after matching:")
    for i, d in enumerate(distances):
        print(f"    base[{i}] ↔ cand[{perm[i]}]: {d:.6f}")
    print(f"  mean cosine distance: {distances.mean():.6f}")
    print(f"  max  cosine distance: {distances.max():.6f}")
    print(f"  p95  cosine distance: {np.percentile(distances, 95):.6f}")
    print()

    print("=== per-cluster top-1 winner ===")
    # Compare baseline cluster i against the candidate cluster it matched
    # to in the permutation. Same logic as the geometry diff: a label
    # permutation alone shouldn't count as a divergence.
    base_top1 = per_cluster_top1(base_r)
    cand_top1 = per_cluster_top1(cand_r)
    diverged = 0
    total = 0
    for i in sorted(int(k) for k in base_top1):
        bk = str(i)
        ck = str(perm[i])
        b = base_top1.get(bk)
        c = cand_top1.get(ck)
        total += 1
        flag = "" if b == c else "  ← DIVERGES"
        if b != c:
            diverged += 1
        print(f"  base[{bk}] = {b:30s} cand[{ck}] = {c}{flag}")
    print()
    print(f"  diverged: {diverged} / {total}")
    print()

    print("=== meta diff ===")
    keys = sorted(set(base_meta) | set(cand_meta))
    for k in keys:
        b = base_meta.get(k)
        c = cand_meta.get(k)
        flag = "" if b == c else "  *"
        print(f"  {k}: baseline={b} candidate={c}{flag}")
    print()

    if diverged == 0 and distances.max() < 0.01:
        print("VERDICT: artifacts are functionally identical. CoreML is safe to ship.")
        return 0
    if diverged == 0:
        print(
            f"VERDICT: top-1 picks match per cluster, but centroid geometry "
            f"drifts up to {distances.max():.4f} cosine. Production behavior "
            f"is identical on K=1 clusters but boundary cases may flip; ship "
            f"with eyes open or stick with CPU."
        )
        return 0
    print(f"VERDICT: {diverged} cluster(s) flip top-1 winner. Stick with CPU.")
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
