#!/usr/bin/env python3
"""Regenerate the precomputed embeddings for the register-probe corpus.

The routing-report tool (cmd/routing-report) and its CI check route a labeled
probe corpus (internal/router/cluster/testdata/register_probes.jsonl) through
the real cluster Scorer. To keep that check ONNX-free in CI, the probe
embeddings are computed here ONCE and committed next to the corpus as
register_probes.emb. The Go side reads them back and feeds a static embedder,
so CI never needs libonnxruntime.

Run this whenever you edit register_probes.jsonl or change the embedder. The
Go check fails loudly (corpus sha mismatch) if the corpus moves without a
regenerate, so a stale cache can't ship silently.

Embedding matches the Go runtime jina path exactly: tail-truncate the prompt
to 1024 bytes (the Scorer's MaxPromptChars, byte-based with UTF-8 boundary
snapping), left-truncate to 256 tokens, mean-pool over the attention mask,
L2-normalize. Every probe is well under 1024 bytes today.

Output format (little-endian), read by loadEmb in cmd/routing-report/main.go:
  magic   [4]byte  = "PEM1"
  hlen    uint32   = len(header JSON)
  header  [hlen]byte JSON: {embedder_id, embed_dim, n, corpus_sha256, comment}
  data    [n][embed_dim]float32  L2-normalized, in corpus (file) order

Usage:
  python scripts/embed_register_probes.py                # jina, default paths
  python scripts/embed_register_probes.py --assets-dir /path/to/assets

Requires: numpy, onnxruntime, tokenizers. The ONNX model.onnx + tokenizer.json
are NOT committed (pulled at Docker build); point --assets-dir at a checkout
that has them, or run on Modal where they're mounted.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import struct
import sys
from pathlib import Path

import numpy as np
import onnxruntime as ort
from tokenizers import Tokenizer

REPO_ROOT = Path(__file__).resolve().parents[1]
TESTDATA = REPO_ROOT / "internal" / "router" / "cluster" / "testdata"
DEFAULT_CORPUS = TESTDATA / "register_probes.jsonl"
DEFAULT_OUT = TESTDATA / "register_probes.emb"
DEFAULT_ASSETS = REPO_ROOT / "assets"  # jina-v2-base-code-int8

MAGIC = b"PEM1"
MAX_TOKENS = 256
MAX_PROMPT_BYTES = 1024  # Scorer MaxPromptChars (tail-truncate, byte-based)
BATCH = 32


def tail_truncate(s: str, max_bytes: int) -> str:
    """Keep the last max_bytes bytes of s, advancing off any partial UTF-8
    continuation byte. Byte-for-byte identical to the Go Scorer's tailTruncate
    (and cmd/routing-report), so the cached vector matches the string the
    Scorer embeds even for multibyte prompts."""
    b = s.encode("utf-8")
    if len(b) <= max_bytes:
        return s
    cut = len(b) - max_bytes
    while cut < len(b) and (b[cut] & 0xC0) == 0x80:
        cut += 1
    return b[cut:].decode("utf-8")
COMMENT = (
    "Precomputed register-probe embeddings so cmd/routing-report needs no ONNX "
    "runtime in CI. Regenerate with scripts/embed_register_probes.py when "
    "register_probes.jsonl or the embedder changes."
)


def embed(texts: list[str], assets_dir: Path) -> np.ndarray:
    onnx_path = assets_dir / "model.onnx"
    tok_path = assets_dir / "tokenizer.json"
    if not onnx_path.exists() or not tok_path.exists():
        sys.exit(f"ERROR: missing {onnx_path} or {tok_path} (ONNX assets are not committed)")
    sess = ort.InferenceSession(str(onnx_path), providers=["CPUExecutionProvider"])
    tok = Tokenizer.from_file(str(tok_path))
    tok.enable_truncation(max_length=MAX_TOKENS, direction="left")
    tok.enable_padding()
    in_names = {i.name for i in sess.get_inputs()}
    out_name = sess.get_outputs()[0].name
    out: list[np.ndarray] = []
    for i in range(0, len(texts), BATCH):
        batch = [tail_truncate(t, MAX_PROMPT_BYTES) for t in texts[i : i + BATCH]]
        enc = tok.encode_batch(batch)
        ids = np.array([e.ids for e in enc], dtype=np.int64)
        mask = np.array([e.attention_mask for e in enc], dtype=np.int64)
        feeds: dict[str, np.ndarray] = {"input_ids": ids, "attention_mask": mask}
        if "token_type_ids" in in_names:
            feeds["token_type_ids"] = np.zeros_like(ids)
        feeds = {k: v for k, v in feeds.items() if k in in_names}
        last = sess.run([out_name], feeds)[0]  # [B, T, H]
        m = mask[:, :, None].astype(np.float32)
        pooled = (last * m).sum(1) / np.clip(m.sum(1), 1e-9, None)
        pooled /= np.linalg.norm(pooled, axis=1, keepdims=True)
        out.append(pooled.astype(np.float32))
    return np.vstack(out)


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--corpus", type=Path, default=DEFAULT_CORPUS)
    ap.add_argument("--out", type=Path, default=DEFAULT_OUT)
    ap.add_argument("--assets-dir", type=Path, default=DEFAULT_ASSETS,
                    help="dir containing model.onnx + tokenizer.json")
    ap.add_argument("--embedder-id", default="jina-v2-base-code-int8",
                    help="recorded in the header; must match the artifact's embedder.model")
    args = ap.parse_args()

    raw = args.corpus.read_bytes()
    corpus_sha = hashlib.sha256(raw).hexdigest()
    rows = [json.loads(line) for line in raw.splitlines() if line.strip()]
    texts = [r["text"] for r in rows]
    print(f"embedding {len(texts)} probes from {args.corpus.name} ...", file=sys.stderr)

    vecs = embed(texts, args.assets_dir)
    dim = int(vecs.shape[1])
    header = {
        "embedder_id": args.embedder_id,
        "embed_dim": dim,
        "n": len(rows),
        "corpus_sha256": corpus_sha,
        "comment": COMMENT,
    }
    hb = json.dumps(header).encode()
    with open(args.out, "wb") as f:
        f.write(MAGIC)
        f.write(struct.pack("<I", len(hb)))
        f.write(hb)
        f.write(vecs.astype("<f4").tobytes())
    print(f"wrote {args.out} ({args.out.stat().st_size} bytes), embedder={args.embedder_id}, "
          f"n={len(rows)}, dim={dim}, corpus_sha={corpus_sha[:12]}", file=sys.stderr)


if __name__ == "__main__":
    main()
