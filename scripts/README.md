# router/scripts

Offline tooling for the cluster routing pipeline. None of these run on
the request path — they produce the committed artifacts under
`router/internal/router/cluster/` that the Go runtime loads via
`//go:embed` at boot.

> See also [`../eval/`](../eval/) — the Phase 1a eval harness (sibling
> Poetry package). It evaluates the artifacts produced here against a
> 500-prompt cost-vs-quality Pareto and gates whether the project
> continues to D2 + D3 + shadow + retrain.

## Install

Poetry-managed (mirrors [`labeler/`](../../labeler/pyproject.toml)).
Uses an in-project venv at `scripts/.venv/` (configured via
`poetry.toml`, gitignored). Python 3.12.3 is pinned to match labeler;
`pyenv install 3.12.3` if your system Python is newer.

```bash
cd router/scripts
poetry env use $(pyenv which python3.12)   # only if `poetry env info` shows a 3.13/3.14 mismatch
poetry install
```

`huggingface-cli` (from `huggingface_hub`) is the only non-Python
dependency surfaced by the scripts directly — it's installed by
`poetry install`. PyTorch is pulled from default PyPI (not
`pytorch-cpu`) because the pytorch-cpu index doesn't ship macOS
wheels and this pipeline targets Apple Silicon dev boxes. The macOS
wheels are CPU-only by default, so we don't pay the ~2 GB CUDA tax.

## Run order — first time

The committed artifacts under `internal/router/cluster/` start as
*placeholders* (a 4-cluster Opus-favoring fallback, an empty ONNX, an
empty tokenizer). Until you run the pipeline below the cluster scorer
fails-open to the heuristic on every request — see
[`internal/router/cluster/embedder_onnx.go`](../internal/router/cluster/embedder_onnx.go)
for the exact fail-open path.

All commands run from `router/scripts/`. `poetry run` activates the
in-project venv automatically; you don't need `poetry shell` first.

```bash
cd router/scripts

# 1. Pull OpenRouterBench (NPULH/OpenRouterBench on HF)
bash download_bench.sh

# 2. Confirm the bench has rows for our deployed-model proxies
poetry run python inspect_bench.py
# Expected: gpt-5, gemini-2.5-pro, claude-sonnet-4, gemini-2.5-flash
#           all have non-zero rows.

# 3. Pull the embedder artifacts from Jina's official HF repo.
#    Writes (all gitignored):
#       ../internal/router/cluster/assets/model.onnx       (Jina's onnx/model_quantized.onnx, ~154 MB INT8)
#       ../internal/router/cluster/assets/tokenizer.json
#       ../internal/router/cluster/assets/{config,tokenizer_config,special_tokens_map}.json
# We use Jina's official quantization rather than maintaining our own.
# No HF token required — the Jina repo is public.
poetry run python download_from_hf.py

# 4. Pick K. Reports % distinct top-1 cells per K; pick the smallest
#    K with ≥80%. Likely K∈[10, 40] given N=3 deployed models.
poetry run python sweep_cluster_k.py

# 5. Train. K from step 4. Writes:
#       ../internal/router/cluster/centroids.bin
#       ../internal/router/cluster/rankings.json
poetry run python train_cluster_router.py --k 40

# 6. Rebuild the Go integration test fixture.
poetry run python dump_cluster_test_vector.py

# 7. Verify Go ↔ Python parity (cosine ≥ 0.98 on every fixture text;
#    English/code/JSON/SQL all hit ≥0.99, multilingual lands at ~0.987
#    due to UTF-8 NFC vs NFD differences between the Python `tokenizers`
#    lib and Go's `daulet/tokenizers`).
#
# Local-dev environment requirements (one-time per machine):
#
#   # libonnxruntime — brew installs to /opt/homebrew/lib; hugot's
#   # default lookup is /usr/local/lib, so override via env var.
#   brew install onnxruntime
#   export ROUTER_ONNX_LIBRARY_DIR=/opt/homebrew/lib
#
#   # libtokenizers — pre-built static lib from
#   # https://github.com/daulet/tokenizers/releases/.
#   mkdir -p ~/.local/lib/libtokenizers
#   curl -sL https://github.com/daulet/tokenizers/releases/latest/download/\
#libtokenizers.darwin-arm64.tar.gz | tar -xz -C ~/.local/lib/libtokenizers/
#   export CGO_LDFLAGS="-L$HOME/.local/lib/libtokenizers"
#
#   # The integration test combines `onnx_integration` (gates the test
#   # itself) and `ORT` (hugot's flag for enabling the ONNX Runtime
#   # backend). The Dockerfile builds production binaries with `-tags ORT`.
cd ..
go test -tags "onnx_integration ORT" ./internal/router/cluster/...
```

That's the whole pipeline. We don't push anything to HF — the model
artifact is whatever Jina ships at the pinned revision. If Jina ever
publishes a new export and we want to pick it up, bump
`DEFAULT_REVISION` in `download_from_hf.py` and `HF_MODEL_REVISION`
in `Dockerfile` together.

## Pulling the model for local dev

Most contributors only need to populate the assets dir to run the
Go integration test. **The Jina HF repo is public** — no token
required:

```bash
cd router/scripts
poetry install                         # one-time
poetry run python download_from_hf.py
```

The Go integration test reads from `assets/model.onnx` directly; the
production binary reads from `/opt/router/assets/model.onnx`
(populated by the Dockerfile's HF curl step). Override either via
`ROUTER_ONNX_ASSETS_DIR`. If you hit HF's anonymous rate limits,
set `HF_TOKEN` (any HF account, read scope is enough).

## Refreshing with public-data ingestion (v0.3+)

OpenRouterBench predates the Claude 4.x / GPT-5.5 / Gemini-3 frontier
launches, so 6 of the 8 v0.2 deployed entries are routed through proxy
bench columns. Two public dumps — SWE-bench experiments (frontier
agent submissions, per-instance pass/fail on 500 Verified instances)
and BFCL-Result (115 models, per-instance correctness on tool-calling)
— supply direct labels for `claude-sonnet-4-5`, `claude-haiku-4-5`,
`claude-opus-4-5` (closer proxy for `claude-opus-4-7`), and
`gemini-3-pro-preview`.

Pipeline:

```bash
cd router/scripts
poetry install                                          # picks up datasets + pytest

# Ingestion: clones the two public repos under .bench-cache/ and emits
# OpenRouterBench-shaped JSONs that bench_walker picks up alongside the
# existing corpus. Multi-GB clone on first run; re-runs are no-ops.
poetry run python -m ingest.fetch_all

# Confirm the new direct columns are populated. Exits non-zero if any
# REQUIRED_MODELS column ends up empty (alias drift).
poetry run python inspect_bench.py

# Hand-edit the v0.3 registry: clone v0.2 + add direct entries for
# claude-sonnet-4-5, claude-haiku-4-5, claude-opus-4-5 (proxy for
# opus-4-7), and gemini-3-pro-preview. The deployed-models list stays
# at 8; only bench_column entries are added.
mkdir -p ../internal/router/cluster/artifacts/v0.3
cp  ../internal/router/cluster/artifacts/v0.2/model_registry.json \
    ../internal/router/cluster/artifacts/v0.3/model_registry.json
$EDITOR ../internal/router/cluster/artifacts/v0.3/model_registry.json

# Re-pick K (likely stays at 10) and train. --no-promote leaves
# artifacts/latest pointing at v0.2 until the A/B compare clears.
poetry run python sweep_cluster_k.py
poetry run python train_cluster_router.py --k 10 --no-promote \
    --notes "v0.3: SWE-bench + BFCL direct labels for claude-sonnet-4-5, claude-haiku-4-5, claude-opus-4-5, gemini-3-pro-preview"

# A/B compare and parity-check before promotion.
poetry run python compare_artifacts.py \
    ../internal/router/cluster/artifacts/v0.2 \
    ../internal/router/cluster/artifacts/v0.3
poetry run python dump_cluster_test_vector.py
cd .. && go test -tags "onnx_integration ORT" ./internal/router/cluster/...

# Promote (one-line edit) only after compare_artifacts shows no
# regressions and the eval harness smoke against v0.3 returns a
# non-degenerate Pareto plot. The eval harness picks v0.3 via
# `x-weave-cluster-version: v0.3` on the allowlisted installation.
echo v0.3 > ../internal/router/cluster/artifacts/latest
```

Adding new direct entries to v0.3's registry follows the
list-valued-aggregation pattern of `bench_walker.load_bench`:

| existing entry (kept) | new sibling entry to add |
|---|---|
| `{"model": "claude-sonnet-4-5", "bench_column": "claude-sonnet-4"}` | `{"model": "claude-sonnet-4-5", "provider": "anthropic", "bench_column": "claude-sonnet-4-5"}` |
| `{"model": "claude-haiku-4-5", "bench_column": "gemini-2.5-flash", "proxy": true}` | `{"model": "claude-haiku-4-5", "provider": "anthropic", "bench_column": "claude-haiku-4-5"}` |
| `{"model": "claude-opus-4-7", "bench_column": "gpt-5", "proxy": true}` | `{"model": "claude-opus-4-7", "provider": "anthropic", "bench_column": "claude-opus-4-5", "proxy": true, "proxy_note": "Anthropic-family closer proxy"}` |
| `{"model": "gemini-3.1-pro-preview", "bench_column": "gemini-2.5-pro", "proxy": true}` | `{"model": "gemini-3.1-pro-preview", "provider": "google", "bench_column": "gemini-3-pro-preview"}` |

`gpt-5.5`, `gemini-3-flash-preview`, and `gemini-3.1-flash-lite-preview`
stay on their existing proxies — public dumps don't have direct labels
for them yet.

When new BFCL drops or SWE-bench submissions land with model names
missing from `ingest/model_aliases.py`, the ingest run logs
`WARNING: unmapped model "..."`. Extend `SWEBENCH_ALIASES` /
`BFCL_ALIASES` and re-run.

## Run order — refresh

When any of these change, retrain:

| Change | Re-run |
|---|---|
| Jina ships a new export (`DEFAULT_REVISION` bumped) | 3 → 7 (and bump `HF_MODEL_REVISION` in `Dockerfile`) |
| K (`sweep_cluster_k.py` recommendation moved) | 5 → 7 |
| α (`--alpha` to `train_cluster_router.py`) | 5 → 7 |
| Per-1k-token cost values (`DEFAULT_COST_PER_1K_INPUT`) | 5 → 7 |
| OpenRouterBench upstream update | 1 → 7 |
| Deployed model added (model #4) | 5 → 7 + cold-start protocol (PRODUCTION READINESS phase) |
| New SWE-bench / BFCL drop with new model spellings | edit `ingest/model_aliases.py`, then `python -m ingest.fetch_all` → `inspect_bench.py` → 5 → 7 |

## What gets committed where

| File | Source | Size | Tracking |
|---|---|---|---|
| `internal/router/cluster/centroids.bin` | `train_cluster_router.py` | ≤180 KB at K=60 | git (binary) |
| `internal/router/cluster/rankings.json` | `train_cluster_router.py` | ~10 KB | git |
| `internal/router/cluster/model_registry.json` | hand-edited (maps benchmark model names to deployed names; read by `train_cluster_router.py`) | ~1 KB | git |
| `internal/router/cluster/assets/model.onnx` | Jina's `onnx/model_quantized.onnx` via `download_from_hf.py` | ~154 MB | **HuggingFace Hub** (`jinaai/jina-embeddings-v2-base-code`); gitignored |
| `internal/router/cluster/assets/tokenizer.json` | Jina's HF repo via `download_from_hf.py` | ~2.4 MB | **HuggingFace Hub** (same repo); gitignored |
| `internal/router/cluster/testdata/fixture.json` | `dump_cluster_test_vector.py` | ~25 KB | git |

`scripts/.bench-cache/` is **not** committed — it's regenerated by
`download_bench.sh` and is ~200 MB.

## What is plan-deviating

* **Build-tag-gated embedder**: the Go package supports `-tags=no_onnx`
  to compile without `libonnxruntime`. The plan only mentions build
  tags for the integration test; this extension is for dev-machine
  convenience. Default builds (Dockerfile, CI) use the real ONNX
  embedder — the plan's intent.
* **Placeholder ONNX**: until you run step 3, the ONNX assets are
  zero-byte placeholders. The `//go:embed` directive still compiles
  (zero-byte embed is legal); `cluster.NewEmbedder` returns an error
  on construction; `main.go` fail-opens to the heuristic. This is the
  documented fail-open path; it just means the cluster scorer is
  inactive until the pipeline runs.

See [`docs/plans/archive/CLUSTER_ROUTING_PLAN.md`](../docs/plans/archive/CLUSTER_ROUTING_PLAN.md) for
the full design.
