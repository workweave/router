"""Generate testdata/fixture.json for the Go embedder integration test.

Loads the same INT8-quantized ONNX + tokenizer the runtime uses,
embeds a fixed set of texts, and writes the (text, embedding) pairs
to internal/router/cluster/testdata/fixture.json. The Go integration
test (`go test -tags onnx_integration ./internal/router/cluster/...`)
asserts cosine ≥ 0.99 against these references — which is the
load-bearing parity gate between Python training and Go inference.

Run after download_from_hf.py and train_cluster_router.py (the test
also depends on the centroids existing). Re-run on every export
refresh.

Usage:
    cd router/scripts && poetry run python dump_cluster_test_vector.py
"""

from __future__ import annotations

import json
from pathlib import Path

import numpy as np
import onnxruntime as ort
from tokenizers import Tokenizer

ROUTER_ROOT = Path(__file__).resolve().parents[1]
ASSETS_DIR = ROUTER_ROOT / "internal/router/cluster/assets"
TESTDATA_DIR = ROUTER_ROOT / "internal/router/cluster/testdata"
EMBED_DIM = 768
MAX_TOKENS = 256

# Fixture texts: a deliberately heterogeneous set so the parity test
# exercises tokenizer special-cases (punctuation, tool-call JSON,
# multi-byte UTF-8, code blocks). Keep these short — the Go test runs
# them every time `go test -tags onnx_integration` runs.
FIXTURE_TEXTS = [
    "hello world",
    "Write a Python function to compute the n-th Fibonacci number.",
    "{\"tool\": \"search\", \"args\": {\"q\": \"Anthropic Claude\"}}",
    "// Go implementation of binary search\nfunc bsearch(xs []int, t int) int { ... }",
    "Explique pourquoi le ciel est bleu en français.",
    "SELECT * FROM users WHERE id = 1 AND deleted_at IS NULL;",
]


def main() -> int:
    onnx_path = ASSETS_DIR / "model.onnx"
    tokenizer_path = ASSETS_DIR / "tokenizer.json"
    if not onnx_path.exists() or not tokenizer_path.exists():
        raise SystemExit(
            "ASSETS missing — run scripts/download_from_hf.py first.\n"
            f"  expected: {onnx_path}, {tokenizer_path}"
        )

    sess_opts = ort.SessionOptions()
    sess_opts.graph_optimization_level = ort.GraphOptimizationLevel.ORT_ENABLE_ALL
    sess = ort.InferenceSession(str(onnx_path), sess_opts, providers=["CPUExecutionProvider"])

    tok = Tokenizer.from_file(str(tokenizer_path))
    tok.enable_truncation(max_length=MAX_TOKENS)
    tok.enable_padding(length=MAX_TOKENS)

    output_name = sess.get_outputs()[0].name
    input_names = {inp.name for inp in sess.get_inputs()}

    encs = tok.encode_batch(FIXTURE_TEXTS)
    input_ids = np.array([e.ids for e in encs], dtype=np.int64)
    attention_mask = np.array([e.attention_mask for e in encs], dtype=np.int64)
    feed = {"input_ids": input_ids, "attention_mask": attention_mask}
    if "token_type_ids" in input_names:
        feed["token_type_ids"] = np.zeros_like(input_ids)

    last_hidden = sess.run([output_name], feed)[0]
    mask = attention_mask[:, :, None].astype(last_hidden.dtype)
    summed = (last_hidden * mask).sum(axis=1)
    counts = mask.sum(axis=1).clip(min=1)
    pooled = summed / counts
    norms = np.linalg.norm(pooled, axis=1, keepdims=True)
    norms = np.where(norms == 0, 1.0, norms)
    embeddings = (pooled / norms).astype(np.float32)
    if embeddings.shape != (len(FIXTURE_TEXTS), EMBED_DIM):
        raise SystemExit(f"unexpected embedding shape: {embeddings.shape}")

    TESTDATA_DIR.mkdir(parents=True, exist_ok=True)
    out_path = TESTDATA_DIR / "fixture.json"
    payload = {
        "embedder_name": "jinaai/jina-embeddings-v2-base-code",
        "quantization": "int8-dynamic",
        "embed_dim": EMBED_DIM,
        "texts": FIXTURE_TEXTS,
        "reference": [vec.tolist() for vec in embeddings],
    }
    out_path.write_text(json.dumps(payload, indent=2))
    print(f"Wrote {out_path} ({out_path.stat().st_size:,} bytes)")
    print(f"  {len(FIXTURE_TEXTS)} texts × {EMBED_DIM} dims = parity fixture")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
