# Cluster routing artifacts

Each subdirectory is one frozen bundle that the cluster scorer can load
at boot. The `latest` pointer file names the default served version;
`ROUTER_CLUSTER_VERSION` env var overrides at runtime.

## Layout

```
artifacts/
├── README.md      (this file)
├── latest         (version pointer, e.g. "v0.53")
├── legacy/        (v1-format bundles, frozen for reproducibility)
│   └── README.md
│   └── v0.21/ … v0.52/
└── v0.53/         (first v2-format bundle and beyond)
    ├── centroids.bin
    ├── model_registry.json
    ├── quality_means.json   (v2 only)
    ├── model_axes.json      (v2 only)
    ├── rankings.json        (v1; optional in v2 during dual-write)
    └── metadata.yaml
```

## Format versions

- **v1** (legacy): `rankings.json` holds the per-cluster, α-blended,
  min-max-normalized scalar score table. α, speed_weight, and
  output_cost_ratio are baked in at training time. Listed in
  `metadata.yaml` for provenance but not runtime-tunable.

- **v2**: `quality_means.json` holds the per-(cluster, model) shrunk
  quality means `Q̄[k][m]` (pre-blend). `model_axes.json` holds the
  per-model raw axes (input/output cost per 1k, TTFT, TPS, verbosity
  tokens). Five routing knobs (α, speed_weight, output_cost_ratio,
  expected_output_tokens, per_model_verbosity) are reconstructable at
  request time and overridable via `x-weave-routing-*` headers. See
  [`docs/plans/ROUTER_RUNTIME_TUNABLE_KNOBS.md`](../../../../docs/plans/ROUTER_RUNTIME_TUNABLE_KNOBS.md).

The loader probes for `quality_means.json` to detect v2; falls back to
`rankings.json` for v1. A v2 directory may co-host both files during
the trainer's dual-write window.

## Embedder

Each bundle's `metadata.yaml` `embedder` block names the embedding
space its `centroids.bin` lives in:

```yaml
embedder:
  model: jina-v2-base-code-int8   # or qwen3-embedding-0.6b-int8
  embed_dim: 768                  # 1024 for qwen3
  max_tokens: 256
```

The loader cross-checks `embed_dim` against the centroids.bin header,
and `NewScorer` refuses to pair a bundle with an embedder whose ID or
dim differs — a bundle trained in one embedding space can never be
scored in another. Bundles without an embedder block default to
`jina-v2-base-code-int8` / 768. The runtime constructs only the
embedders its built bundles require, so Jina and Qwen3 bundles can
coexist in one deploy (staging `ROUTER_CLUSTER_BUILD_ALL_VERSIONS=true`)
while prod loads exactly one model.

### Trainer contract (must match runtime exactly)

`train_cluster_router.py` (sibling `router-internal/eval/`) must embed
training prompts identically to how the Go runtime embeds requests, or
the bundle silently misroutes — there is no runtime error for a
training/serving embedding mismatch beyond the ID/dim guard.

For a `qwen3-embedding-0.6b-int8` bundle (e.g. v0.67):

| Aspect | Required value |
|---|---|
| Model | `Qwen/Qwen3-Embedding-0.6B` (the exact export served at runtime; prefer embedding through the same INT8 ONNX produced by `scripts/export_qwen3_onnx.py`) |
| Pooling | Last-token (attention-mask-aware; baked into the ONNX graph) |
| Normalization | L2 (runtime applies hugot `WithNormalization`) |
| Instruction prefix | **None** — raw prompt text, document-style (matches Jina behavior) |
| Truncation | Tail-truncate to 1024 chars (`MaxPromptChars`), UTF-8 boundary snapped |
| metadata.yaml | `embedder: {model: qwen3-embedding-0.6b-int8, embed_dim: 1024, max_tokens: 256}` |

For `jina-v2-base-code-int8` bundles the existing trainer path is
unchanged (mean pooling, L2 norm, no prefix, same truncation).

Before promoting any Qwen bundle (pointing `latest` at it), run the
latency gate: Qwen3-0.6B INT8 embed p95 on the target prod CPU must
clear the 1500 ms `EmbedTimeout` with margin; timeouts surface as
HTTP 503, not degraded routing.

## Working with bundles

- Use `train_cluster_router.py` to write a new version; the script
  auto-bumps from `latest` and never overwrites an existing directory.
- Pass `--write-v2` to emit a v2 bundle (default at the time of v0.53
  and forward).
- Promote a candidate by editing `latest` to its name and redeploying.
- Never edit `centroids.bin`, `rankings.json`, `quality_means.json`, or
  `model_axes.json` by hand; only `model_registry.json` is
  hand-editable (the trainer reads it).

Legacy v1 bundles live under `legacy/`. They remain loadable by the
runtime via the same code path — `bundleDirForVersion` resolves either
root or legacy locations transparently.
