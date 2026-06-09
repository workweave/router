#!/usr/bin/env python3
"""Export Qwen3-Embedding-0.6B to ONNX with last-token pooling baked in.

The router's Go embedder (hugot FeatureExtractionPipeline) mean-pools
3D token-level outputs — wrong for Qwen3, which requires last-token
pooling. Baking the pooling into the graph makes the export emit a 2D
[batch, 1024] output that hugot returns as-is (it only mean-pools 3D),
so the Go side needs no model-specific pooling code and train/runtime
parity is guaranteed by construction.

Pipeline:
  1. Load Qwen/Qwen3-Embedding-0.6B (transformers AutoModel).
  2. Wrap with attention-mask-aware last-token pooling (no L2 norm in
     the graph; hugot's WithNormalization applies it, matching Jina).
  3. Export to ONNX with dynamic batch/sequence axes.
  4. INT8 dynamic quantization (onnxruntime).
  5. Parity self-check vs sentence-transformers reference (cosine
     >= 0.99 fp32, >= 0.98 int8) and emit a Go test fixture JSON.
  6. Optionally upload to a Weave-owned HF repo (--upload).

Usage:
  python scripts/export_qwen3_onnx.py --out-dir ./qwen3-export
  python scripts/export_qwen3_onnx.py --out-dir ./qwen3-export \
      --upload weave-ai/qwen3-embedding-0.6b-onnx-router

Requires: torch>=2.2, transformers>=4.51, onnx, onnxruntime,
          sentence-transformers>=2.7 (parity check), huggingface_hub
          (upload only).

The output directory layout matches what the Dockerfile pulls and what
internal/router/cluster expects under assets/qwen3-embedding-0.6b-int8/:
  model.onnx       (INT8, pooled output [batch, 1024])
  model_fp32.onnx  (provenance; not deployed)
  tokenizer.json
  fixture.json     (Go parity-test fixture, copy to
                    internal/router/cluster/testdata/fixture_qwen3.json)
"""

from __future__ import annotations

import argparse
import json
import shutil
import sys
import tempfile
from pathlib import Path

MODEL_ID = "Qwen/Qwen3-Embedding-0.6B"
EMBED_DIM = 1024
EMBEDDER_NAME = "qwen3-embedding-0.6b-int8"
OPSET = 18

# Mirrors the prompts in scripts/dump_cluster_test_vector.py so the Go
# parity suite exercises the same multilingual/code coverage.
PARITY_TEXTS = [
    "How do I reverse a linked list in Python?",
    "Refactor this Go HTTP handler to use context cancellation.",
    "What is the capital of France?",
    "def quicksort(arr):\n    if len(arr) <= 1:\n        return arr",
    "Explique-moi la différence entre un processus et un thread.",
    "SELECT id, name FROM users WHERE created_at > now() - interval '7 days';",
    "Write a unit test for a debounce function in TypeScript.",
    "深圳的天气怎么样？",
]


def build_wrapper(model):
    import torch

    class LastTokenPooled(torch.nn.Module):
        """Emits [batch, hidden] by gathering each row's last real token.

        Works for right-padded batches (gather at mask.sum()-1) and
        degenerates correctly for left-padded ones (last column), the
        same logic as the HF model card's last_token_pool.
        """

        def __init__(self, base):
            super().__init__()
            self.base = base

        def forward(self, input_ids, attention_mask):
            hidden = self.base(
                input_ids=input_ids, attention_mask=attention_mask
            ).last_hidden_state
            seq_lengths = attention_mask.sum(dim=1) - 1
            batch = torch.arange(hidden.shape[0], device=hidden.device)
            return hidden[batch, seq_lengths]

    return LastTokenPooled(model)


def export_fp32(out_path: Path):
    import torch
    from transformers import AutoModel, AutoTokenizer

    print(f"Loading {MODEL_ID} ...")
    tokenizer = AutoTokenizer.from_pretrained(MODEL_ID, padding_side="right")
    model = AutoModel.from_pretrained(MODEL_ID, torch_dtype=torch.float32)
    model.eval()
    wrapper = build_wrapper(model)
    wrapper.eval()

    sample = tokenizer(
        ["export sample one", "a slightly longer export sample two"],
        padding=True,
        return_tensors="pt",
    )

    print(f"Exporting fp32 ONNX (opset {OPSET}) -> {out_path}")
    torch.onnx.export(
        wrapper,
        (sample["input_ids"], sample["attention_mask"]),
        str(out_path),
        input_names=["input_ids", "attention_mask"],
        output_names=["sentence_embedding"],
        dynamic_axes={
            "input_ids": {0: "batch", 1: "sequence"},
            "attention_mask": {0: "batch", 1: "sequence"},
            "sentence_embedding": {0: "batch"},
        },
        opset_version=OPSET,
    )
    return tokenizer


def quantize_int8(fp32_path: Path, int8_path: Path):
    import onnx
    from onnxruntime.quantization import QuantType, quantize_dynamic

    # Keep down_proj (3072x1024) and o_proj (2048x1024) MatMuls in fp32:
    # both consume outlier-heavy activations (SwiGLU product / attention
    # output) and quantizing them drops worst-case parity below 0.98.
    # The dynamo exporter anonymizes node names, so the sensitive layers
    # are identified by weight shape instead.
    model = onnx.load(str(fp32_path), load_external_data=False)
    init_dims = {i.name: tuple(i.dims) for i in model.graph.initializer}
    sensitive = {(3072, 1024), (2048, 1024)}
    exclude = [
        n.name
        for n in model.graph.node
        if n.op_type == "MatMul"
        and any(init_dims.get(inp) in sensitive for inp in n.input)
    ]

    print(f"Quantizing INT8 -> {int8_path} (excluding {len(exclude)} outlier-sensitive MatMuls)")
    quantize_dynamic(
        model_input=str(fp32_path),
        model_output=str(int8_path),
        weight_type=QuantType.QInt8,
        per_channel=True,
        nodes_to_exclude=exclude,
    )


def onnx_embed(onnx_path: Path, tokenizer, texts: list[str]):
    """Embed texts one at a time (the router embeds single prompts) and
    L2-normalize, mirroring hugot's WithNormalization."""
    import numpy as np
    import onnxruntime as ort

    sess = ort.InferenceSession(str(onnx_path), providers=["CPUExecutionProvider"])
    out = []
    for text in texts:
        enc = tokenizer([text], padding=True, truncation=True, max_length=8192, return_tensors="np")
        (vec,) = sess.run(
            ["sentence_embedding"],
            {"input_ids": enc["input_ids"], "attention_mask": enc["attention_mask"]},
        )
        v = vec[0].astype(np.float64)
        v /= max(float((v**2).sum() ** 0.5), 1e-12)
        out.append(v)
    return out


def reference_embed(texts: list[str]):
    """sentence-transformers ground truth, no instruction prompt — the
    router embeds raw prompt text (document-style), matching training."""
    from sentence_transformers import SentenceTransformer

    model = SentenceTransformer(MODEL_ID)
    return model.encode(texts, normalize_embeddings=True)


def cosine(a, b) -> float:
    import numpy as np

    a = np.asarray(a, dtype=np.float64)
    b = np.asarray(b, dtype=np.float64)
    a = a / max(float(np.linalg.norm(a)), 1e-12)
    b = b / max(float(np.linalg.norm(b)), 1e-12)
    return float(np.dot(a, b))


def parity_check(onnx_path: Path, tokenizer, threshold: float, label: str):
    print(f"Parity check ({label}, threshold {threshold}) ...")
    got = onnx_embed(onnx_path, tokenizer, PARITY_TEXTS)
    ref = reference_embed(PARITY_TEXTS)
    worst = 1.0
    for text, g, r in zip(PARITY_TEXTS, got, ref):
        c = cosine(g, r)
        worst = min(worst, c)
        print(f"  cosine={c:.5f}  {text[:60]!r}")
        if c < threshold:
            sys.exit(f"FAIL: cosine {c:.5f} < {threshold} for {text!r} ({label})")
    print(f"  worst={worst:.5f} OK")
    return ref


def write_fixture(out_dir: Path, reference):
    fixture = {
        "texts": PARITY_TEXTS,
        "reference": [[float(x) for x in row] for row in reference],
        "embedder_name": EMBEDDER_NAME,
        "embed_dim": EMBED_DIM,
        "quantization": "int8-dynamic",
    }
    path = out_dir / "fixture.json"
    path.write_text(json.dumps(fixture))
    print(f"Wrote Go parity fixture -> {path}")
    print("Copy to internal/router/cluster/testdata/fixture_qwen3.json for the Go suite.")


def upload(out_dir: Path, repo: str):
    from huggingface_hub import HfApi

    api = HfApi()
    print(f"Uploading {out_dir} -> {repo}")
    api.create_repo(repo, exist_ok=True, private=True)
    info = api.upload_folder(
        folder_path=str(out_dir),
        repo_id=repo,
        commit_message=f"Export {MODEL_ID} with baked last-token pooling (opset {OPSET}, int8)",
    )
    print(f"Uploaded. Pin HF_QWEN_REVISION to the new commit SHA: {info.oid}")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--out-dir", required=True, type=Path)
    ap.add_argument("--upload", metavar="HF_REPO", help="upload the export to this HF repo after parity passes")
    ap.add_argument("--fp32-threshold", type=float, default=0.99)
    ap.add_argument("--int8-threshold", type=float, default=0.98)
    args = ap.parse_args()

    out_dir: Path = args.out_dir
    out_dir.mkdir(parents=True, exist_ok=True)

    fp32_path = out_dir / "model_fp32.onnx"
    int8_path = out_dir / "model.onnx"

    tokenizer = export_fp32(fp32_path)
    quantize_int8(fp32_path, int8_path)

    with tempfile.TemporaryDirectory() as td:
        tokenizer.save_pretrained(td)
        shutil.copy(Path(td) / "tokenizer.json", out_dir / "tokenizer.json")
        for companion in ("tokenizer_config.json", "special_tokens_map.json"):
            src = Path(td) / companion
            if src.exists():
                shutil.copy(src, out_dir / companion)

    parity_check(fp32_path, tokenizer, args.fp32_threshold, "fp32")
    reference = parity_check(int8_path, tokenizer, args.int8_threshold, "int8")
    write_fixture(out_dir, reference)

    if args.upload:
        upload(out_dir, args.upload)


if __name__ == "__main__":
    main()
