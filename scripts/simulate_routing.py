"""Simulate cluster-scorer routing distribution against RouterArena
prompts WITHOUT making any LLM calls.

Embeds prompts via the same Jina v2 INT8 ONNX the trainer uses, then
replays the runtime's top-p=4 cluster sum + argmax against each
artifact's rankings.json. Outputs pick distribution per variant so we
can compare normalization tweaks (e.g., per-model z-score post-process,
candidate filters) cheaply before paying for a full RouterArena smoke.

Usage:

    cd router/scripts
    poetry run python simulate_routing.py \\
        --versions v0.6,v0.7,v0.8 \\
        --split sub_10 \\
        --variants baseline,zscore_per_model,exclude=gpt-5.5,exclude=gpt-5.5+gpt-4.1

Variants:
  - ``baseline``                 — argmax over all models in rankings.json
  - ``exclude=<m1>+<m2>+...``    — argmax over the candidate set with the
                                   listed model names removed
  - ``zscore_per_model``         — post-process rankings.json: for each
                                   model, rescale per-cluster scores to
                                   mean 0 / std 1 across the K clusters
                                   (forces per-model variance instead of
                                   "consistent winner" patterns)
"""

from __future__ import annotations

import argparse
import json
import struct
import sys
from collections import Counter
from pathlib import Path
from typing import Dict, List, Tuple

import numpy as np

# Reuse the trainer's embedder loader so we use the same Jina ONNX +
# tokenizer as training (matches runtime cluster geometry exactly).
sys.path.insert(0, str(Path(__file__).resolve().parent))
from train_cluster_router import (  # noqa: E402
    ARTIFACTS_DIR,
    ASSETS_DIR,
    EMBED_DIM,
    embed_batch,
    load_embedder,
)


def read_centroids(path: Path) -> np.ndarray:
    """Inverse of train_cluster_router.write_centroids. Returns a
    (K, dim) float32 numpy array, L2-normalized as the trainer wrote
    them."""
    blob = path.read_bytes()
    if blob[:4] != b"CRT1":
        sys.exit(f"ERROR: {path} bad magic {blob[:4]!r}; expected b'CRT1'")
    version, k, dim = struct.unpack("<III", blob[4:16])
    if version != 1:
        sys.exit(f"ERROR: {path} version={version} unsupported")
    if dim != EMBED_DIM:
        sys.exit(f"ERROR: {path} dim={dim} != EMBED_DIM={EMBED_DIM}")
    flat = np.frombuffer(blob[16:], dtype="<f4")
    if flat.size != k * dim:
        sys.exit(f"ERROR: {path} truncated; expected {k * dim} floats, got {flat.size}")
    return flat.reshape(k, dim).astype(np.float32, copy=True)


def load_rankings(path: Path) -> Dict[int, Dict[str, float]]:
    raw = json.loads(path.read_text())["rankings"]
    return {int(k): dict(v) for k, v in raw.items()}


def top_p_clusters(prompt_vec: np.ndarray, centroids: np.ndarray, p: int) -> List[int]:
    """Cosine similarity (centroids + prompt are L2-normalized) → top-p
    indices. Mirrors scorer.go::topPNearest."""
    sims = centroids @ prompt_vec
    if p >= centroids.shape[0]:
        return list(range(centroids.shape[0]))
    return list(np.argpartition(-sims, p)[:p])


def argmax_route(
    prompt_vec: np.ndarray,
    centroids: np.ndarray,
    rankings: Dict[int, Dict[str, float]],
    candidate_models: List[str],
    p: int,
) -> str:
    """Replay scorer.go: top-p clusters → uniform sum of ranking rows
    over candidate_models → argmax. candidate_models is the deduped,
    filtered list (matches s.models in the patched runtime)."""
    top = top_p_clusters(prompt_vec, centroids, p)
    scores = {m: 0.0 for m in candidate_models}
    for k in top:
        row = rankings[k]
        for m in candidate_models:
            scores[m] += row.get(m, 0.0)
    return max(scores, key=scores.get)


def apply_zscore_per_model(rankings: Dict[int, Dict[str, float]]) -> Dict[int, Dict[str, float]]:
    """For each model, rescale its per-cluster scores so they have mean
    0 and std 1 across the K clusters. Tests the hypothesis that
    'consistent decent across clusters' is what causes single-model
    concentration under top-p=4 sum: forcing per-model zero-mean kills
    that pattern. Std=0 (perfectly flat model) → leave at 0."""
    cluster_ids = sorted(rankings.keys())
    all_models = sorted({m for row in rankings.values() for m in row})
    out: Dict[int, Dict[str, float]] = {k: {} for k in cluster_ids}
    for m in all_models:
        vals = np.array([rankings[k].get(m, 0.0) for k in cluster_ids], dtype=np.float64)
        mean = vals.mean()
        std = vals.std()
        if std <= 0:
            for k in cluster_ids:
                out[k][m] = 0.0
        else:
            zs = (vals - mean) / std
            for i, k in enumerate(cluster_ids):
                out[k][m] = float(zs[i])
    return out


def parse_variants(spec: str) -> List[Tuple[str, dict]]:
    """Parse --variants string into (name, params) tuples. Examples:
        baseline                            → ("baseline", {})
        zscore_per_model                    → ("zscore_per_model", {})
        exclude=gpt-5.5+gpt-4.1             → ("exclude", {"models": ["gpt-5.5","gpt-4.1"]})
    """
    variants = []
    for token in spec.split(","):
        token = token.strip()
        if not token:
            continue
        if "=" in token:
            name, val = token.split("=", 1)
            if name == "exclude":
                variants.append(("exclude", {"models": val.split("+")}))
            else:
                sys.exit(f"ERROR: unknown parameterized variant {name!r}")
        else:
            variants.append((token, {}))
    return variants


def load_prompts(split: str, n: int | None) -> List[str]:
    """Reuses the harness's HF parquet path; the file is cached locally
    after the first smoke."""
    from huggingface_hub import hf_hub_download
    import pandas as pd

    SPLIT_TO_FILE = {
        "sub_10": "data/sub_10-00000-of-00001.parquet",
        "full": "data/full-00000-of-00001.parquet",
    }
    if split not in SPLIT_TO_FILE:
        sys.exit(f"ERROR: unknown split {split!r}; expected one of {list(SPLIT_TO_FILE)}")
    parquet_path = hf_hub_download("RouteWorks/RouterArena", SPLIT_TO_FILE[split], repo_type="dataset")
    df = pd.read_parquet(parquet_path)
    if n is not None and n < len(df):
        df = df.iloc[:n]
    # The Question column is the prompt; matches eval/grade.py's mapping.
    return df["Question"].astype(str).tolist()


def candidates_for_variant(
    rankings: Dict[int, Dict[str, float]],
    registry_models: List[str],
    variant_name: str,
    variant_params: dict,
) -> Tuple[Dict[int, Dict[str, float]], List[str]]:
    """Apply variant transforms and return (effective_rankings,
    candidate_model_list)."""
    effective_rankings = rankings
    candidates = list(registry_models)

    if variant_name == "baseline":
        pass
    elif variant_name == "exclude":
        excluded = set(variant_params["models"])
        candidates = [m for m in candidates if m not in excluded]
        if not candidates:
            sys.exit(
                f"ERROR: variant exclude={'+'.join(variant_params['models'])} "
                f"removed every model from the registry; argmax has no candidates."
            )
    elif variant_name == "zscore_per_model":
        effective_rankings = apply_zscore_per_model(rankings)
    else:
        sys.exit(f"ERROR: unknown variant {variant_name!r}")
    return effective_rankings, candidates


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--versions", default="v0.6,v0.7,v0.8")
    parser.add_argument("--split", default="sub_10", choices=["sub_10", "full"])
    parser.add_argument("--variants", default="baseline,zscore_per_model")
    parser.add_argument("--n", type=int, default=None, help="cap prompt count (debug)")
    parser.add_argument("--top-p", type=int, default=4)
    parser.add_argument("--batch-size", type=int, default=32)
    parser.add_argument("--assets", type=Path, default=ASSETS_DIR)
    parser.add_argument("--coreml", action="store_true")
    args = parser.parse_args()

    versions = [v.strip() for v in args.versions.split(",") if v.strip()]
    variants = parse_variants(args.variants)

    print(f"Loading {args.split} prompts ...", file=sys.stderr)
    prompts = load_prompts(args.split, args.n)
    print(f"  {len(prompts)} prompts", file=sys.stderr)

    print(f"Loading embedder from {args.assets} ...", file=sys.stderr)
    sess, tok, input_names, output_name = load_embedder(args.assets, use_coreml=args.coreml)

    print(f"Embedding {len(prompts)} prompts (batch={args.batch_size}) ...", file=sys.stderr)
    chunks = []
    for i in range(0, len(prompts), args.batch_size):
        batch = prompts[i : i + args.batch_size]
        chunks.append(embed_batch(sess, tok, input_names, output_name, batch))
        if (i // args.batch_size) % 10 == 0:
            print(f"  {i}/{len(prompts)}", file=sys.stderr)
    embeddings = np.vstack(chunks)
    # L2-normalize prompt vectors so cosine = dot, matching the runtime.
    norms = np.linalg.norm(embeddings, axis=1, keepdims=True)
    norms = np.where(norms == 0, 1.0, norms)
    embeddings = embeddings / norms
    print(f"  embeddings shape: {embeddings.shape}", file=sys.stderr)

    # Per-version artifact load
    version_data = {}
    for v in versions:
        d = ARTIFACTS_DIR / v
        centroids = read_centroids(d / "centroids.bin")
        rankings = load_rankings(d / "rankings.json")
        registry = json.loads((d / "model_registry.json").read_text())
        # Dedupe by Model (matches the patched runtime).
        seen = set()
        models = []
        for e in registry["deployed_models"]:
            if e["model"] in seen:
                continue
            seen.add(e["model"])
            models.append(e["model"])
        models = sorted(models)
        version_data[v] = (centroids, rankings, models)
        print(
            f"  {v}: K={centroids.shape[0]}, models={len(models)} "
            f"({', '.join(models)})",
            file=sys.stderr,
        )

    # Run sims and tabulate
    print()
    header = f"{'version':<6} | {'variant':<28} | {'distinct':<8} | top-3 picks (% of {len(prompts)})"
    print(header)
    print("-" * len(header))
    for v in versions:
        centroids, rankings, models = version_data[v]
        for vname, vparams in variants:
            eff_rankings, candidates = candidates_for_variant(rankings, models, vname, vparams)
            picks = Counter()
            for vec in embeddings:
                m = argmax_route(vec, centroids, eff_rankings, candidates, args.top_p)
                picks[m] += 1
            distinct = len(picks)
            top3 = [
                f"{m.replace('claude-','c-').replace('gemini-','g-').replace('-preview','-prv')[:18]}={100 * c / len(prompts):.0f}%"
                for m, c in picks.most_common(3)
            ]
            label = vname if not vparams else f"{vname}={'+'.join(vparams.get('models', []))}"
            print(f"{v:<6} | {label:<28} | {distinct:<8} | {', '.join(top3)}")

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
