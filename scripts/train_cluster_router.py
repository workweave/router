"""Train the cluster router from OpenRouterBench (Path A: re-cluster +
re-aggregate scores; no new model evals).

Pipeline:
  1. Load deployed-model rows from the bench cache (mirrors
     inspect_bench.py / sweep_cluster_k.py filters).
  2. Re-embed prompts with the INT8-quantized Jina v2 ONNX (same
     inference path as the runtime, so cluster geometry trains on the
     same vectors it'll see in production).
  3. K-means at the chosen K (output of sweep_cluster_k.py).
  4. Per-(cluster, model) raw quality means.
  5. Min-max-normalize per cluster, blend with min-max-normalized cost
     per the paper's α formula:
        x_ji = α · p~_ji + (1-α) · (1 - q~_ji)
     where α=0.53 (paper Table 3 knee).
  6. Map bench-model names → deployed-model names via the registry,
     emit centroids.bin + rankings.json + metadata.yaml under
     ``internal/router/cluster/artifacts/<version>/``.

Versioning model
----------------
Each call writes to one frozen artifact directory:
``internal/router/cluster/artifacts/<version>/`` (e.g. v0.3). The
training script never overwrites a previous version; the input registry
is read from the same directory so each version is a self-contained,
comparable bundle. Promoting a version to production updates the
``artifacts/latest`` pointer file so the Go runtime serves it by default.

By default a new version is auto-bumped from ``artifacts/latest`` (e.g.
latest=v0.2 → v0.3). Provide ``--version v0.X`` to write to a specific
directory; pass ``--from v0.2`` to reuse another version's
model_registry.json without re-creating one for the new directory.

Usage:
    # Auto-bump from latest, copy registry from latest's dir, train, promote
    cd router/scripts && poetry run python train_cluster_router.py --k 40

    # Explicit version, parent registry from v0.2, no promotion
    cd router/scripts && poetry run python train_cluster_router.py \\
        --version v0.3 --from v0.2 --k 40 --no-promote

    # Dry run: skip artifact writes, just print blended cells
    cd router/scripts && poetry run python train_cluster_router.py --k 40 --dry-run --sample-size 200
"""

from __future__ import annotations

import argparse
import json
import os
import struct
import sys
from collections import defaultdict
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
MAX_TOKENS = 256


# --------------------------------------------------------------------
# Version management
#
# Each artifact bundle lives at ARTIFACTS_DIR / <version> / and contains
# four files: model_registry.json, rankings.json, centroids.bin,
# metadata.yaml. The latest pointer file at ARTIFACTS_DIR / "latest"
# names the version the Go runtime serves by default.
# --------------------------------------------------------------------


def read_latest() -> str:
    """Return the version named in artifacts/latest (e.g. "v0.2"). The
    pointer must exist; this script is the authority on what `latest`
    means and refuses to guess."""
    if not LATEST_POINTER.exists():
        sys.exit(
            f"ERROR: {LATEST_POINTER} is missing. The runtime needs "
            f"this file to resolve the default cluster version. "
            f"Recreate it manually with the version you intended to "
            f"promote (e.g. echo v0.2 > {LATEST_POINTER})."
        )
    raw = LATEST_POINTER.read_text().strip()
    if not raw:
        sys.exit(f"ERROR: {LATEST_POINTER} is empty.")
    return raw


def list_versions() -> List[str]:
    """All committed version directories, sorted lexicographically."""
    if not ARTIFACTS_DIR.exists():
        return []
    return sorted(p.name for p in ARTIFACTS_DIR.iterdir() if p.is_dir())


def parse_version(v: str) -> Tuple[int, int]:
    """Split "v0.3" → (0, 3). Used by next_version() to bump the minor.
    Aborts on malformed strings so the next-version arithmetic can
    assume integer minors."""
    if not v.startswith("v"):
        sys.exit(f"ERROR: version {v!r} must start with 'v' (e.g. v0.3)")
    parts = v[1:].split(".")
    if len(parts) != 2:
        sys.exit(f"ERROR: version {v!r} must be 'v<major>.<minor>' (e.g. v0.3)")
    try:
        return int(parts[0]), int(parts[1])
    except ValueError:
        sys.exit(f"ERROR: version {v!r} has non-integer parts")


def next_version() -> str:
    """Auto-bump the minor of artifacts/latest. v0.2 → v0.3."""
    cur = read_latest()
    major, minor = parse_version(cur)
    return f"v{major}.{minor + 1}"


_NORMALIZATION_LABELS = {
    "minmax": "per_prompt_minmax_across_bench_columns",
    "raw": "raw_bench_column_means",
    "zscore": "per_prompt_zscore_across_bench_columns",
}


def _normalization_label(mode: str) -> str:
    """Map a `--score-normalization` choice to the descriptive string we
    write into metadata.yaml + rankings.json. Keeping the v0.5+ minmax
    label unchanged means historical artifacts still diff cleanly."""
    return _NORMALIZATION_LABELS[mode]


def write_metadata_yaml(out_path: Path, meta: dict) -> None:
    """Emit metadata.yaml without depending on PyYAML — every field we
    write is a primitive (string, int, float) or a flat list/map of
    primitives, so a hand-rolled emitter is enough and we avoid adding
    a runtime dep just for this."""
    lines: List[str] = []

    def emit(key: str, value, indent: int = 0) -> None:
        pad = "  " * indent
        if isinstance(value, dict):
            lines.append(f"{pad}{key}:")
            for k, v in value.items():
                emit(k, v, indent + 1)
        elif isinstance(value, list):
            lines.append(f"{pad}{key}:")
            for item in value:
                lines.append(f"{pad}  - {item}")
        elif isinstance(value, str) and "\n" in value:
            lines.append(f"{pad}{key}: |")
            for chunk in value.rstrip("\n").split("\n"):
                lines.append(f"{pad}  {chunk}")
        elif isinstance(value, str):
            lines.append(f"{pad}{key}: {json.dumps(value)}")
        elif value is None:
            lines.append(f"{pad}{key}: null")
        elif isinstance(value, bool):
            lines.append(f"{pad}{key}: {'true' if value else 'false'}")
        else:
            lines.append(f"{pad}{key}: {value}")

    for k, v in meta.items():
        emit(k, v)
    out_path.write_text("\n".join(lines) + "\n")
    print(f"  wrote {out_path}")

# Default per-1k-token cost values used in the α-blend. These live in
# the trainer rather than at runtime because the runtime scorer is a
# pure argmax — no per-request cost lookup, by design (paper §3 and
# router/docs/plans/archive/CLUSTER_ROUTING_PLAN.md).
#
# Numbers are USD per 1k INPUT tokens. Sourced from each vendor's
# public pricing page on 2026-04-30 — see
# `model_registry.json::meta.frontier_pricing_source_date`. Bump these
# when a vendor changes pricing and rerun training. Add an entry
# whenever model_registry.json adds a new deployed model; the script
# aborts if a registry model has no cost here.
DEFAULT_COST_PER_1K_INPUT = {
    # Anthropic
    "claude-opus-4-7":   15.00,
    "claude-sonnet-4-5":  3.00,
    "claude-haiku-4-5":   0.80,

    # OpenAI: GPT-5.5 family (April 2026 release; doubled the GPT-5 line)
    "gpt-5.5":             5.00,    # $5/1M input
    "gpt-5.5-pro":        30.00,    # $30/1M input
    "gpt-5.5-mini":        0.50,    # projected; $0.50/1M
    "gpt-5.5-nano":        0.15,    # projected; $0.15/1M

    # OpenAI: GPT-5.4 family
    "gpt-5.4":             3.00,
    "gpt-5.4-pro":        20.00,
    "gpt-5.4-mini":        0.40,
    "gpt-5.4-nano":        0.10,

    # OpenAI: GPT-5 (pre-5.5 sticker; still on price list at $2.50/1M)
    "gpt-5":               2.50,
    "gpt-5-chat":          2.50,
    "gpt-5-mini":          0.50,
    "gpt-5-nano":          0.10,

    # OpenAI: GPT-4.x (legacy)
    "gpt-4.1":             2.00,
    "gpt-4.1-mini":        0.40,
    "gpt-4.1-nano":        0.10,
    "gpt-4o":              2.50,
    "gpt-4o-mini":         0.15,

    # Google: Gemini 3.x (April 2026 preview; -preview while not GA)
    "gemini-3-pro-preview":         2.00,    # $2/1M ≤200K context
    "gemini-3.1-pro-preview":       2.00,    # supersedes gemini-3-pro-preview after 2026-03-09
    "gemini-3-flash-preview":       0.50,    # $0.50/1M
    "gemini-3.1-flash-lite-preview": 0.10,   # estimated; tracks 2.5-flash-lite tier

    # Google: Gemini 2.x (legacy; still bench-routable)
    "gemini-2.5-pro":      1.25,
    "gemini-2.5-flash":    0.30,
    "gemini-2.5-flash-lite": 0.10,
    "gemini-2.0-flash":    0.10,
    "gemini-2.0-flash-lite": 0.075,
}

# Paper Table 3 knee: matches GPT-5-medium accuracy at -27% cost in
# the paper's 8-model setup. Override via --alpha; expect retuning
# once Phase 1a's eval harness measures it on Claude Code traffic.
DEFAULT_ALPHA = 0.53


def load_registry(version_dir: Path) -> Tuple[List[Dict[str, object]], Dict[str, List[str]]]:
    """Read model_registry.json from a versioned artifact directory and
    return:
      * the flat list of deployed entries (each {model, provider,
        bench_column, proxy?, proxy_note?})
      * the bench_column → [deployed_model, ...] map used by load_bench
        to copy bench-column scores onto every deployed entry that
        references that column. List-valued because a single column
        commonly serves multiple deployed entries (e.g. gpt-5's column
        scores both the openai/gpt-5 entry and the proxy
        anthropic/claude-opus-4-7 entry).
    """
    registry_path = version_dir / "model_registry.json"
    if not registry_path.exists():
        sys.exit(
            f"ERROR: {registry_path} is missing. Either pre-create the "
            f"file (copy + edit a sibling version's registry) or pass "
            f"--from <version> to copy it from another bundle."
        )
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


def load_embedder(assets_dir: Path, use_coreml: bool = False, dynamic_padding: bool = False):
    """Mirror sweep_cluster_k.py.load_embedder. Kept inline here so
    the script can be run standalone without importing across siblings.

    When `use_coreml` is True, prefer the CoreML execution provider on
    macOS (M-series ANE / Metal) for ~3-5x faster INT8 BERT inference,
    falling back to CPU if CoreML isn't available. **Parity caveat:**
    the runtime Go scorer uses the CPU ONNX path in production, so
    INT8 quantization can produce embeddings that differ by epsilon
    between CoreML and CPU. Cosine drift is typically <0.001 per dim
    and well within the cluster scorer's tolerance, but if you ship a
    CoreML-trained `centroids.bin` to a CPU-only runtime you accept
    that risk. Default stays CPU so the same artifact serves both.

    When `dynamic_padding` is True, the tokenizer pads each batch to
    the longest prompt in that batch instead of the fixed MAX_TOKENS
    (256). For prompt sets where most inputs are well under 256 tokens
    (eval slices, real Claude Code traffic) this is a 2-4x throughput
    win because BERT compute is O(seq_len^2) in attention. **Trainer
    keeps fixed-padding (False)** for byte-identical centroid geometry
    across runs; iteration tools (holdout_eval, difficulty_judge) flip
    it on because tiny embedding deltas (<0.001 cosine) don't change
    cluster assignment in practice.
    """
    onnx_path = assets_dir / "model.onnx"
    tokenizer_path = assets_dir / "tokenizer.json"
    if not onnx_path.exists():
        sys.exit(f"ERROR: {onnx_path} missing — run scripts/download_from_hf.py")
    if not tokenizer_path.exists():
        sys.exit(f"ERROR: {tokenizer_path} missing — run scripts/download_from_hf.py")

    sess_opts = ort.SessionOptions()
    sess_opts.graph_optimization_level = ort.GraphOptimizationLevel.ORT_ENABLE_ALL
    # Apple Silicon has performance + efficiency cores; ONNX Runtime's
    # default thread pool covers all of them, which oversubscribes for
    # INT8 BERT-base where each op already saturates a P-core. Capping
    # at 4 P-cores wins ~25-35% throughput on M1/M2 Pro/Max for this
    # workload. Linux production isn't affected — the env var
    # ROUTER_ONNX_INTRA_OP_THREADS overrides if you want to tune for
    # a specific deployment shape.
    threads_override = os.environ.get("ROUTER_ONNX_INTRA_OP_THREADS")
    if threads_override:
        sess_opts.intra_op_num_threads = int(threads_override)
    elif sys.platform == "darwin":
        sess_opts.intra_op_num_threads = 4
    if use_coreml:
        # CoreML provider is silently dropped on non-macOS / when not
        # built into onnxruntime; CPU stays as the safety net.
        provider_list = ["CoreMLExecutionProvider", "CPUExecutionProvider"]
    else:
        provider_list = ["CPUExecutionProvider"]
    sess = ort.InferenceSession(str(onnx_path), sess_opts, providers=provider_list)
    active = sess.get_providers()
    print(f"  ONNX execution providers: requested={provider_list} active={active}")
    if use_coreml and "CoreMLExecutionProvider" not in active:
        print(
            "  WARNING: --coreml requested but CoreMLExecutionProvider not active. "
            "Did you install onnxruntime built with CoreML support? Falling back to CPU."
        )

    tok = Tokenizer.from_file(str(tokenizer_path))
    # Left-side truncation matches the runtime scorer's tail-truncate
    # behavior (scorer.go's tailTruncate keeps the prompt suffix). Without
    # this, training embeds the prompt head while the runtime embeds the
    # tail — different vectors, broken cluster geometry.
    tok.enable_truncation(max_length=MAX_TOKENS, direction="left")
    if dynamic_padding:
        # No fixed length → pad to longest in each batch. Saves the
        # quadratic-attention tax on short prompts.
        tok.enable_padding()
    else:
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
    encs = tok.encode_batch(texts)
    input_ids = np.array([e.ids for e in encs], dtype=np.int64)
    attention_mask = np.array([e.attention_mask for e in encs], dtype=np.int64)
    feed: Dict[str, np.ndarray] = {"input_ids": input_ids, "attention_mask": attention_mask}
    if "token_type_ids" in input_names:
        feed["token_type_ids"] = np.zeros_like(input_ids)
    last_hidden = sess.run([output_name], feed)[0]
    mask = attention_mask[:, :, None].astype(last_hidden.dtype)
    summed = (last_hidden * mask).sum(axis=1)
    counts = mask.sum(axis=1).clip(min=1)
    pooled = summed / counts
    norms = np.linalg.norm(pooled, axis=1, keepdims=True)
    norms = np.where(norms == 0, 1.0, norms)
    return (pooled / norms).astype(np.float32)


def aggregate_cells(
    labels: np.ndarray,
    prompts: List[str],
    scores: Dict[str, Dict[str, float]],
    deployed_models: List[str],
    k: int,
) -> Tuple[Dict[int, Dict[str, float]], Dict[int, Dict[str, int]]]:
    """Per-(cluster, model) mean of the (already per-prompt-normalized)
    bench score, plus per-cell observation counts. Returns
    ``(means, counts)`` shaped as ``{cluster_id: {deployed_model:
    value}}``. Counts are the raw record counts feeding each mean and
    are consumed by ``shrink_to_prior`` to weight cluster evidence vs
    a global prior.

    Cells with zero observations are absent from ``means``; the
    shrinkage step backfills them with the global mean.
    """
    sums: Dict[int, Dict[str, float]] = defaultdict(lambda: defaultdict(float))
    counts: Dict[int, Dict[str, int]] = defaultdict(lambda: defaultdict(int))
    for idx, prompt in enumerate(prompts):
        cluster_id = int(labels[idx])
        for m, s in scores.get(prompt, {}).items():
            sums[cluster_id][m] += s
            counts[cluster_id][m] += 1

    means: Dict[int, Dict[str, float]] = {}
    counts_out: Dict[int, Dict[str, int]] = {}
    for cluster_id in range(k):
        cell_means: Dict[str, float] = {}
        cell_counts: Dict[str, int] = {}
        for m in deployed_models:
            n = counts[cluster_id].get(m, 0)
            cell_counts[m] = n
            if n > 0:
                cell_means[m] = sums[cluster_id][m] / n
        means[cluster_id] = cell_means
        counts_out[cluster_id] = cell_counts
    return means, counts_out


# Bayesian-shrinkage prior weight: how many "phantom" records of the
# global mean we mix into every (cluster, model) cell. Small enough that
# clusters with hundreds of observations are dominated by their own
# evidence; large enough that a cluster with 2-3 records doesn't get to
# emit an outlier mean unchallenged. Empty cells (n=0) collapse to the
# global mean exactly, same as the v0.4 fill_missing_with_global path.
DEFAULT_SHRINKAGE_K0 = 10.0


def shrink_to_prior(
    means: Dict[int, Dict[str, float]],
    counts: Dict[int, Dict[str, int]],
    deployed_models: List[str],
    k0: float = DEFAULT_SHRINKAGE_K0,
) -> Dict[int, Dict[str, float]]:
    """Replace v0.4's flat fill-with-global with a sample-size-weighted
    shrinkage estimator:

        posterior_ji = (n_ji · cluster_mean_ji + k0 · global_mean_j)
                       / (n_ji + k0)

    where ``j`` is a deployed model and ``i`` is a cluster. n=0 cells
    fall back to the model's global mean (so sparse-coverage models
    still have a number for argmax). High-n cells track their cluster
    mean almost exactly. The middle case — n on the order of k0 — is
    where the prior actually matters: it stops a model with five
    SWE-bench-only observations in cluster 4 from claiming a 0.0
    cluster mean against a global mean of 0.55, the way v0.4 did for
    Opus.

    Global mean is computed across **observed** cells only (count-
    weighted), so models with sparse coverage don't have their global
    mean inflated by the empty cells we're about to backfill. This is
    the bug-class fix for v0.4's gpt-4.1 = 0.94-everywhere artifact.
    """
    # Count-weighted global mean per model. Using counts (not the flat
    # cell_means.values() average from v0.4) keeps a model whose
    # evidence is concentrated in 2 of 10 clusters from getting a
    # global prior dominated by 8 empty cells' fabricated means.
    global_sums: Dict[str, float] = defaultdict(float)
    global_counts: Dict[str, int] = defaultdict(int)
    for cluster_id, cell in means.items():
        for m, v in cell.items():
            n = counts[cluster_id].get(m, 0)
            global_sums[m] += v * n
            global_counts[m] += n
    global_mean = {
        m: (global_sums[m] / global_counts[m])
        for m in global_sums
        if global_counts[m] > 0
    }

    out: Dict[int, Dict[str, float]] = {}
    for cluster_id, cell in means.items():
        posterior: Dict[str, float] = {}
        for m in deployed_models:
            n = counts[cluster_id].get(m, 0)
            prior = global_mean.get(m, 0.5)
            if n == 0:
                posterior[m] = prior
                continue
            cluster_mean = cell[m]
            posterior[m] = (n * cluster_mean + k0 * prior) / (n + k0)
        out[cluster_id] = posterior
    return out


def alpha_blend(
    cells: Dict[int, Dict[str, float]],
    cost_per_1k: Dict[str, float],
    alpha: float,
    deployed_models: List[str],
) -> Dict[int, Dict[str, float]]:
    """Per-cluster min-max-normalize quality scores AND costs, then
    apply paper's α-blend:
        x_ji = α · p~_ji + (1 - α) · (1 - q~_ji)
    Cost is the same for every cluster (it's a per-model constant), so
    its min-max normalization across models within a cluster is the
    SAME for every cluster — but we redo it inside the loop for
    clarity vs. an outer hoist that makes the formula opaque.
    """
    out: Dict[int, Dict[str, float]] = {}
    for cluster_id, cell in cells.items():
        # Quality: min-max within this cluster's models.
        q_vals = [cell[m] for m in deployed_models if m in cell]
        q_min = min(q_vals)
        q_max = max(q_vals)
        q_range = q_max - q_min
        # Cost: min-max across models present in this cell.
        c_vals = [cost_per_1k[m] for m in deployed_models if m in cell]
        c_min = min(c_vals)
        c_max = max(c_vals)
        c_range = c_max - c_min

        blended: Dict[str, float] = {}
        for m in deployed_models:
            if m not in cell:
                continue
            q = cell[m]
            c = cost_per_1k[m]
            q_norm = (q - q_min) / q_range if q_range > 0 else 0.0
            c_norm = (c - c_min) / c_range if c_range > 0 else 0.0
            blended[m] = alpha * q_norm + (1 - alpha) * (1 - c_norm)
        out[cluster_id] = blended
    return out


def per_model_zscore(
    blended: Dict[int, Dict[str, float]],
    deployed_models: List[str],
) -> Dict[int, Dict[str, float]]:
    """Post-α-blend rescale: for each model, normalize its per-cluster
    scores to mean 0 / std 1 across the K clusters.

    Why: the runtime's top-p=K' sum favors models that are "consistent
    decent across all clusters" because the sum smooths per-cluster
    variation. Models with broad bench-column coverage (e.g., gpt-5
    with 17,668 prompts) end up consistently above-average per cluster
    after Stage A/B/C, which gives them a sum-monotonic advantage no
    amount of per-cluster minmax can break. Per-model z-score forces
    each model's per-cluster scores to have zero mean across clusters,
    so argmax picks the model whose score *peaks* on each prompt's
    top-p clusters rather than the model with the highest average.
    Models that are differentially good on specific clusters keep
    their high z-score there; models that are flat-good lose their
    consistent-winner edge.

    Trade-off: erases absolute-quality differences between models. A
    consistently-better model is indistinguishable post-zscore from a
    consistently-worse one. Use only when diversification beats the
    "best model wins everywhere" outcome — typically the case when
    bench coverage is uneven across models (proxy chains, missing
    direct labels).

    std == 0 (perfectly flat) → leave at 0. Implementation parity
    with scripts/simulate_routing.py::apply_zscore_per_model so
    A/B simulation results carry through to trained artifacts.
    """
    cluster_ids = sorted(blended.keys())
    out: Dict[int, Dict[str, float]] = {k: {} for k in cluster_ids}
    for m in deployed_models:
        vals = np.array(
            [blended[k].get(m, 0.0) for k in cluster_ids], dtype=np.float64
        )
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


def write_centroids(centroids: np.ndarray, out_path: Path) -> None:
    """Emit centroids.bin in the format Go's loadCentroids expects.

    Header (little-endian):
        4-byte magic = "CRT1"
        uint32 version = 1
        uint32 K
        uint32 dim
    Then K * dim * float32 LE bytes, row-major.
    """
    k, dim = centroids.shape
    if dim != EMBED_DIM:
        sys.exit(f"centroid dim {dim} != EMBED_DIM {EMBED_DIM}")
    with out_path.open("wb") as f:
        f.write(b"CRT1")
        f.write(struct.pack("<I", 1))
        f.write(struct.pack("<I", k))
        f.write(struct.pack("<I", dim))
        f.write(centroids.astype("<f4").tobytes())
    print(f"  wrote {out_path} ({out_path.stat().st_size:,} bytes)")


def write_rankings(
    blended: Dict[int, Dict[str, float]],
    out_path: Path,
    meta: dict,
) -> None:
    """Emit rankings.json. Cluster keys are stringified for JSON
    object compatibility; loadRankings parses them back to int.
    """
    payload = {
        "meta": meta,
        "rankings": {str(k): v for k, v in sorted(blended.items())},
    }
    out_path.write_text(json.dumps(payload, indent=2, sort_keys=True))
    print(f"  wrote {out_path} ({out_path.stat().st_size:,} bytes)")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cache", type=Path, default=Path(__file__).resolve().parent / ".bench-cache")
    parser.add_argument("--assets", type=Path, default=ASSETS_DIR)
    parser.add_argument("--k", type=int, required=True, help="Cluster count (output of sweep_cluster_k.py)")
    parser.add_argument("--alpha", type=float, default=DEFAULT_ALPHA)
    parser.add_argument(
        "--shrinkage-k0",
        type=float,
        default=DEFAULT_SHRINKAGE_K0,
        help=(
            "Shrinkage prior weight for the per-(cluster, model) "
            "Bayesian backfill. Higher = more pull toward the model's "
            "global mean; lower = trust per-cluster evidence sooner. "
            f"Default {DEFAULT_SHRINKAGE_K0}."
        ),
    )
    parser.add_argument(
        "--score-normalization",
        choices=("minmax", "raw", "zscore"),
        default="minmax",
        help=(
            "Per-prompt cross-column rescale applied in bench_walker "
            "Stage B. 'minmax' (default, v0.5+) is maximally "
            "discriminative but magnifies tiny bench gaps into maximal "
            "ranking gaps. 'raw' skips Stage B entirely and emits raw "
            "column means — preserves the original gap shape. 'zscore' "
            "is a middle ground (per-prompt z-score clipped to [-3,3] "
            "then mapped to [0,1])."
        ),
    )
    parser.add_argument(
        "--per-model-zscore",
        action="store_true",
        help=(
            "Post-α-blend rescale: for each deployed model, normalize "
            "per-cluster scores to mean 0 / std 1 across the K clusters. "
            "Breaks 'broad bench coverage wins' by killing the "
            "consistent-decent advantage that compounds with the "
            "runtime's top-p sum. Trade-off: erases absolute-quality "
            "differences between models. Use when bench coverage is "
            "uneven (proxy chains)."
        ),
    )
    parser.add_argument("--seed", type=int, default=42)
    parser.add_argument("--batch-size", type=int, default=32)
    parser.add_argument("--sample-size", type=int, default=0,
                        help="If >0, randomly subsample prompts (for fast iteration / smoke testing).")
    parser.add_argument("--dry-run", action="store_true",
                        help="Run the full pipeline but skip writing artifacts/<version>/.")
    parser.add_argument("--coreml", action="store_true",
                        help=("Use Apple's CoreML ONNX execution provider for ~3-5x "
                              "faster embedding on M-series Macs. Production runtime "
                              "uses CPU ONNX, so INT8 quantization may drift the "
                              "trained centroid geometry by epsilon vs. CPU training. "
                              "OK for fast iteration; use plain CPU for the artifact "
                              "you ship."))
    parser.add_argument("--router-version", default="weave-router-v0.1-bootstrap")
    parser.add_argument("--version", default=None,
                        help=("Target artifact version directory under "
                              "internal/router/cluster/artifacts/. Defaults to an "
                              "auto-bump of artifacts/latest (v0.2 → v0.3). Pass "
                              "an existing version (e.g. --version v0.2) to "
                              "overwrite it — only do this for in-place fixes."))
    parser.add_argument("--from", dest="parent", default=None,
                        help=("Copy model_registry.json from this sibling version "
                              "before training. Use when starting a new version "
                              "with the same deployed-model set as the previous one."))
    parser.add_argument("--no-promote", action="store_true",
                        help=("Skip updating artifacts/latest after a successful "
                              "write. Default is to promote: the runtime will pick "
                              "up the new version on next deploy."))
    parser.add_argument("--notes", default="",
                        help=("Free-form changelog text to embed in metadata.yaml's "
                              "`changelog` field. Helpful for follow-up version "
                              "comparisons."))
    args = parser.parse_args()

    target_version = args.version or next_version()
    parse_version(target_version)  # validate format
    target_dir = ARTIFACTS_DIR / target_version
    if not args.dry_run:
        target_dir.mkdir(parents=True, exist_ok=True)

    if args.parent:
        parent_dir = ARTIFACTS_DIR / args.parent
        parent_registry = parent_dir / "model_registry.json"
        if not parent_registry.exists():
            sys.exit(f"ERROR: --from {args.parent}: {parent_registry} not found.")
        target_registry = target_dir / "model_registry.json"
        if target_registry.exists() and not args.dry_run:
            sys.exit(
                f"ERROR: refusing to overwrite {target_registry}. Remove it "
                f"manually before re-running with --from to clone a parent "
                f"registry."
            )
        if args.dry_run:
            print(f"DRY-RUN: would copy registry from {args.parent} → {target_version}")
        else:
            target_registry.write_text(parent_registry.read_text())
            print(f"Copied registry from {args.parent} → {target_version}")

    print(f"Training cluster artifact {target_version} (dir: {target_dir})")
    # In dry-run mode the target_dir is not created and the parent registry
    # is not copied, so load from the parent dir if --from was passed.
    registry_source = (
        ARTIFACTS_DIR / args.parent if args.dry_run and args.parent else target_dir
    )
    entries, bench_to_deployed = load_registry(registry_source)
    deployed_models = sorted({e["model"] for e in entries})
    deployed_to_provider = {e["model"]: e["provider"] for e in entries}
    print(f"Deployed model set ({len(deployed_models)}):")
    for m in deployed_models:
        print(f"  - {m}  (provider={deployed_to_provider[m]})")

    cost_per_1k = {m: DEFAULT_COST_PER_1K_INPUT.get(m, 1.0) for m in deployed_models}
    if any(m not in DEFAULT_COST_PER_1K_INPUT for m in deployed_models):
        missing = [m for m in deployed_models if m not in DEFAULT_COST_PER_1K_INPUT]
        sys.exit(
            f"ERROR: missing cost values for {missing}. Add entries to "
            f"DEFAULT_COST_PER_1K_INPUT in train_cluster_router.py before "
            f"rerunning."
        )

    print(f"Loading bench from {args.cache} (score_normalization={args.score_normalization}) ...")
    prompts, scores = load_bench(
        args.cache,
        bench_to_deployed,
        score_normalization=args.score_normalization,
    )
    print(f"  {len(prompts)} unique prompts, {sum(len(v) for v in scores.values())} (prompt, model) score rows")

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
    embeddings = np.vstack(chunks)
    print(f"  embeddings shape: {embeddings.shape}")

    print(f"K-means K={args.k} (n_init=10, seed={args.seed}) ...")
    km = KMeans(n_clusters=args.k, n_init=10, random_state=args.seed)
    km.fit(embeddings)
    centroids = km.cluster_centers_.astype(np.float32)
    # L2-normalize centroids: the runtime computes cosine via dot product
    # and assumes both centroid and embedding are unit-norm.
    norms = np.linalg.norm(centroids, axis=1, keepdims=True)
    norms = np.where(norms == 0, 1.0, norms)
    centroids = centroids / norms
    # Reassign labels by cosine similarity against the *normalized*
    # centroids so the per-cluster aggregation rows are keyed by the same
    # geometry the runtime scorer uses (top-p by dot product on unit
    # vectors). Without this re-assignment, training labels are produced
    # by Euclidean distance to the un-normalized centroids and can drift
    # off the runtime's argmax.
    labels = np.argmax(embeddings @ centroids.T, axis=1)
    print(f"  inertia={km.inertia_:.1f}")

    if args.shrinkage_k0 < 0:
        sys.exit("ERROR: --shrinkage-k0 must be >= 0 (negative weights make the posterior undefined).")

    print("Aggregating per-(cluster, model) cells ...")
    raw_means, raw_counts = aggregate_cells(labels, prompts, scores, deployed_models, args.k)
    filled_cells = shrink_to_prior(raw_means, raw_counts, deployed_models, k0=args.shrinkage_k0)
    n_observed = sum(1 for cell in raw_means.values() for _ in cell)
    n_total = len(filled_cells) * len(deployed_models)
    print(
        f"  {len(filled_cells)} clusters x {len(deployed_models)} models = "
        f"{n_total} cells; {n_observed} observed, "
        f"{n_total - n_observed} backfilled to count-weighted global prior "
        f"(shrinkage k0={args.shrinkage_k0})"
    )

    print(f"α-blending (alpha={args.alpha}) ...")
    blended = alpha_blend(filled_cells, cost_per_1k, args.alpha, deployed_models)

    if args.per_model_zscore:
        print("Per-model z-score post-process across clusters ...")
        blended = per_model_zscore(blended, deployed_models)

    if args.dry_run:
        print()
        print("DRY-RUN: no artifacts written. Sample blended cells:")
        for cl in list(blended)[:5]:
            print(f"  cluster {cl}: {blended[cl]}")
        return 0

    print()
    print(f"Writing artifacts to {target_dir} ...")
    centroids_path = target_dir / "centroids.bin"
    rankings_path = target_dir / "rankings.json"
    metadata_path = target_dir / "metadata.yaml"

    write_centroids(centroids, centroids_path)
    write_rankings(
        blended,
        rankings_path,
        meta={
            "router_version": args.router_version,
            "embedder_model": "jina-v2-base-code-int8",
            "alpha": args.alpha,
            "shrinkage_k0": args.shrinkage_k0,
            "score_normalization": _normalization_label(args.score_normalization),
            "per_model_zscore": args.per_model_zscore,
            "top_p": 4,
            "k": args.k,
            "seed": args.seed,
            "n_prompts": len(prompts),
            "training_data_mix": {"d1": 1.0, "d2": 0.0, "d3": 0.0},
            "cost_per_1k_input_usd": cost_per_1k,
        },
    )

    parent = args.parent
    if parent is None and args.version is None:
        # Auto-bump: the implicit parent is whatever latest pointed at
        # before this run.
        parent = read_latest()

    deployed_providers = sorted({e["provider"] for e in entries})
    metadata = {
        "version": target_version,
        "parent": parent,
        "status": "latest" if not args.no_promote else "candidate",
        "promoted_date": _today_iso(),
        "embedder": {
            "model": "jina-v2-base-code-int8",
            "embed_dim": EMBED_DIM,
            "max_tokens": MAX_TOKENS,
        },
        "training": {
            "k": args.k,
            "top_p": 4,
            "alpha": args.alpha,
            "shrinkage_k0": args.shrinkage_k0,
            "score_normalization": _normalization_label(args.score_normalization),
            "per_model_zscore": args.per_model_zscore,
            "seed": args.seed,
            "n_prompts": len(prompts),
            "training_data_mix": {"d1": 1.0, "d2": 0.0, "d3": 0.0},
        },
        "deployed_providers": deployed_providers,
        "deployed_models": deployed_models,
        "cost_per_1k_input_usd": cost_per_1k,
        "changelog": args.notes or (
            f"Auto-bumped from {parent} on training run."
            if parent
            else f"Initial training run for {target_version}."
        ),
    }
    write_metadata_yaml(metadata_path, metadata)

    if not args.no_promote:
        LATEST_POINTER.write_text(target_version + "\n")
        print(f"Promoted: {LATEST_POINTER} → {target_version}")
    else:
        print(f"--no-promote: artifacts/latest still points at {read_latest()}")

    print()
    print("Done. Next:")
    print("  poetry run python dump_cluster_test_vector.py   # rebuild Go test fixture")
    print("  cd .. && go test -tags onnx_integration ./internal/router/cluster/...   # parity test")
    return 0


def _today_iso() -> str:
    """Today's date in YYYY-MM-DD. Matches the format hand-written
    metadata.yaml files use so the runtime parser handles both
    indistinguishably."""
    from datetime import date  # local import keeps top-level imports sorted
    return date.today().isoformat()


if __name__ == "__main__":
    raise SystemExit(main())
