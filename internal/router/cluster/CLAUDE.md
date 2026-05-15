# internal/router/cluster — CLAUDE

> **Mirror notice.** Verbatim sync with [AGENTS.md](AGENTS.md). **Update both together** — divergence = bug.

AvengersPro-derived primary router (arXiv 2508.12631, DAI 2025). **P0.** Full design in [`../../../docs/plans/archive/CLUSTER_ROUTING_PLAN.md`](../../../docs/plans/archive/CLUSTER_ROUTING_PLAN.md); this file is the rules-for-AI subset. Read [root CLAUDE.md](../../../CLAUDE.md) and [internal/router/CLAUDE.md](../CLAUDE.md) first.

## What's load-bearing

### Build tags

Package compiles in **two layered modes via build tags**:

- `embedder_onnx.go` vs `embedder_stub.go` — gated by `no_onnx`. Default builds compile the real hugot-backed embedder; `-tags=no_onnx` swaps in a stub `NewEmbedder` that always errors. Used by contributors without `libonnxruntime`.
- `-tags ORT` — required by **hugot v0.7+** to enable the ONNX Runtime backend. Without it, `hugot.NewORTSession` returns "to enable ORT, run `go build -tags ORT`" and `cluster.NewEmbedder` fails. Dockerfile builds with `-tags ORT`. **Do not drop this tag from any production-bound build.**
- To run the parity integration test, combine: `-tags "onnx_integration ORT"`.

### Local-dev build env (Apple Silicon)

- `libtokenizers` static lib must be on the linker path. Pre-built releases at https://github.com/daulet/tokenizers/releases/. Extract `libtokenizers.darwin-arm64.tar.gz` somewhere user-writable + set `CGO_LDFLAGS=-L/path/to/dir`.
- `libonnxruntime` shared lib via `brew install onnxruntime`. brew installs to `/opt/homebrew/lib`, hugot defaults to `/usr/local/lib` lookup. Set `ROUTER_ONNX_LIBRARY_DIR=/opt/homebrew/lib` to override. (Linux containers using Dockerfile don't need this — `/usr/lib/libonnxruntime.so` is the default, populated by the runtime stage.)

### Versioned artifacts

Every committed bundle lives at `artifacts/v<X.Y>/` with four files: `centroids.bin`, `rankings.json`, `model_registry.json`, `metadata.yaml`.

- `artifacts/latest` pointer file (single line, e.g. `v0.37`) names the version the runtime serves by default; `ROUTER_CLUSTER_VERSION` env overrides.
- Promotion = one-line edit to `latest` + redeploy.
- Committed history spans v0.21 through the current `latest` — earlier versions are pruned once they fall out of eval comparison.

### Multi-version build flag

Go runtime builds **only the served default version** by default (`cmd/router/main.go`'s `buildClusterScorer`). Setting `ROUTER_CLUSTER_BUILD_ALL_VERSIONS=true` switches to building **one Scorer per committed bundle** so callers can pin per-request to a sibling version with `x-weave-cluster-version: v0.X` via `middleware.WithClusterVersionOverride`.

"Compare-against-each-other" mechanism — staging/eval deploys set the flag so a single deploy carries every committed bundle + the eval harness flips between them per-request. Prod leaves the flag off: only the default bundle is loaded into memory, and the header override is a no-op.

### Centroids/rankings are write-once

`train_cluster_router.py` always writes to `artifacts/v<X.Y>/` and never overwrites a previous version (auto-bumps from `latest` when `--version` is omitted). Pass `--from v0.36` to clone the previous version's `model_registry.json` before training a new one. **Never edit `centroids.bin` / `rankings.json` by hand.** `model_registry.json` is the only hand-editable file in a bundle (the training script reads it).

### `metadata.yaml`

Informational at runtime — carries version changelog, training params, deployed models, α-blend cost values. Go runtime parses it for `/health`-style provenance; eval harness reads it offline. Keep it accurate but it does not affect routing decisions.

### `assets/model.onnx`

**NOT in git.** Use Jina's own INT8 export at `jinaai/jina-embeddings-v2-base-code`, file path `onnx/model_quantized.onnx`.

- Dockerfile pulls anonymously during build (Jina repo public — self-hosters don't need creds); local dev pulls via `scripts/download_from_hf.py`.
- `HF_TOKEN` build secret is *optional* (raises rate limits in CI) + `required=false` in Dockerfile.
- Go embedder reads from `/opt/router/assets/model.onnx` (override via `ROUTER_ONNX_ASSETS_DIR`).
- If missing or <1 MiB, `cluster.NewEmbedder` errors at boot + `main.go` panics — router refuses to start rather than silently degrading.
- `HF_MODEL_REVISION` pinned to Jina SHA by default; bump deliberately to pick up new upstream exports.

### Cost values

Used in α-blend, live in `train_cluster_router.py`'s `DEFAULT_COST_PER_1K_INPUT`. Baked into `rankings.json` at training time, not looked up at request time (paper §3 — runtime scoring is a single argmax). When Anthropic changes prices, update the dict + rerun training.

## What to NOT do

- **Don't add per-request cost lookup or runtime α knob.** α is baked at training time; changing it requires retraining. Per-request override (`x-weave-routing-alpha`) is P1, not P0 — wait for a customer ask before shipping.
- **Don't loosen `MaxPromptChars = 1024` cap** without re-running the latency test. BERT inference is O(n²) attention; the cap is load-bearing.
- **Don't add fail-open fallbacks.** Cluster scorer returns `ErrClusterUnavailable` on every failure path (embed timeout, embed error, dim mismatch, prompt too short, empty argmax). API handlers map it to HTTP 503. The previous `heuristic` fallback was removed because it silently degraded routing — every request that should have hit the cluster scorer instead got `claude-haiku-4-5`, masking real regressions in eval + prod. New failure modes return the sentinel; no default-model shortcut "for safety".
- **Don't change the centroid format without bumping the magic string.** `loadCentroids` uses magic + version header to refuse mismatched binaries; if the layout changes, bump `centroidsMagic` from `CRT1` to `CRT2` so the next deploy refuses old binaries instead of silently misrouting.
- **Don't overwrite a previously committed artifact version.** Versions are frozen for comparison — once `v0.37` is committed, train to `v0.38` rather than re-running `train_cluster_router.py` against `v0.37`. Training script auto-bumps; only override with `--version v0.X` for in-place fixes intended to land as a separate commit.
- **Don't bypass the version pointer.** `artifacts/latest` is the single source of truth for the default served version. Don't hardcode a version in `cmd/router/main.go`; let `cluster.ResolveVersion` read the pointer.
