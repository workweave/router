Created: 2026-04-30
Last edited: 2026-05-03

# Cluster routing — full plan

> Living plan for the content-aware router. P0 is "working version, end-to-end,
> in-process, OpenRouterBench-derived (no new model evals)." Phase 1a's eval
> harness is the explicit gate before further investment. The Production
> Readiness section is the path from P0 to a router we trust on every
> Claude Code request. Update all halves as decisions land.

---

## Context

The router today picks Opus vs Haiku via a deterministic token-threshold
rule (`internal/router/heuristic/`). It's safe but blunt: a 100-token
"design a distributed cache" prompt routes to Haiku because it's short, and
a 5k-token paste of a stack trace routes to Opus because it's long. The
correlation between size and difficulty is weak, especially for Claude Code
where users paste large context but ask simple questions, or write short
prompts that imply hard tasks.

Two earlier workstreams are already done:

- **WS1 — prompt trimming** (`router/internal/auth/tokens.go`): the router
  now extracts `PromptText = last user message, tail-truncated`, dropping
  the ~10k-token static system prompt from the routing-features payload.
- **WS2 — RouteLLM removal**: the LMSYS proof-of-concept scorer and the
  `OPENAI_API_KEY` path are gone. Heuristic-only baseline runs in prod.

This plan covers WS3 onward: a cluster-based scorer that **adopts
AvengersPro** ([arxiv 2508.12631](https://arxiv.org/abs/2508.12631), DAI 2025
Best Paper) for our 3 Anthropic models, then upgraded with real-traffic
retraining, shadow-mode validation, GCS-backed artifacts, and per-decision
observability (production).

**P0 ambition: adapt AvengersPro to 3 models without re-running model
evaluations.** AvengersPro's GitHub
([ZhangYiqun018/AvengersPro](https://github.com/ZhangYiqun018/AvengersPro))
releases code only — no centroids, no scoring matrix, no precomputed
embeddings — and uses an API-served Qwen3-embedding-8B (4096-dim) that
can't drop into in-process Go inference. So "without retraining" reduces to
a concrete plan: reuse their per-(prompt, model) scoring matrix from
**OpenRouterBench** (`NPULH/OpenRouterBench` on HF, released 2025-12-10 by
the same group — the actual AvengersPro evaluation data), reuse their
α-blend formula, but **re-cluster with our embedder** (centroids don't
transfer across embedders anyway). Filter their model columns to Opus 4.1
- Sonnet 4 + Gemini-2.5-flash, map to our deployed Opus 4.7 / Sonnet 4.5 /
Haiku 4.5. No new model evaluations; compute cost ~$0.

**Hyperparameters in this plan are pinned to paper §3 and §4.1.3.** Earlier
drafts attributed details to AvengersPro that aren't actually in the paper
— a softmax temperature, a three-weight SLA, a $/1k cost subtraction. Those
are corrected below; plan-invented operating-mode policy (Beta priors, ε-greedy
ramps, retrain cadence) is called out as such.

**Decisions locked in:**

| Decision | Choice | Reasoning |
|---|---|---|
| Embedder | `jinaai/jina-embeddings-v2-base-code` (768-dim, 8k context, ~161 MB, **INT8-quantized ONNX**, CPU) | Paper uses Qwen3-embedding-8B (4096-dim, ~16 GB, GPU/API). We can't ship that in-process Go on Cloud Run, so we swap. Jina v2 is code-specialized and fits the latency + memory budget once INT8-quantized. **Accuracy delta vs. paper's embedder is unknown** — measure on the eval harness (Phase 1a). |
| Routing scope | 3-way: Opus 4.7 / Sonnet 4.5 / Haiku 4.5 | All three deployed. OpenRouterBench has direct proxies for the first two (`anthropic/claude-opus-4.1`, `anthropic/claude-sonnet-4`); Haiku has no direct proxy, so Gemini-2.5-flash stands in at P0. **Load-bearing assumption** — the moment D3 traffic accumulates, we re-grade Haiku directly and overwrite. |
| Training data, P0 | **OpenRouterBench** (`NPULH/OpenRouterBench` on HF) — the AvengersPro paper's actual evaluation dataset | Reuses paper's scoring matrix directly, no new model evals. Distribution mismatch with Claude Code traffic is real but eval harness (Phase 1a) measures it before we trust v0.5. |
| Training data, prod | Real Claude Code prompts replayed + judge-graded | Distribution-matched. **Open question (P1):** which Postgres/ClickHouse table actually captures Claude Code prompt text — the prior research agent named `ai_user_prompts.prompt_content` but the migration isn't visible. Verify before D3 work depends on it. |
| Cluster count K | **TBD; measure cell-collapse on a sample before committing** | Paper used K=60 with N=8 models and ~2,600 prompts. With N=3, K=60 likely produces many cells with identical "Opus > Sonnet > Haiku" ranking, defeating the routing premise. Sample sweep K∈{10, 20, 40, 60} on OpenRouterBench during Phase 0 and pick the smallest K where ≥80% of cells have a distinct top-1. |
| Scoring formula | **Paper's α-blend** with min-max normalization per cluster: `xⱼⁱ = α · p̃ⱼⁱ + (1−α) · (1−q̃ⱼⁱ)` | Earlier draft of this plan subtracted raw `$/1k tokens` from a [0,1] quality score — cost dominated by ~10× and would have routed everything to Haiku. Use the paper's exact formulation. Default α=0.53 (paper's "matches GPT-5 accuracy at −27% cost" sweet spot). |
| Top-p cluster aggregation | **p=4, uniform sum** (paper §3) | Earlier draft used softmax-weighted top-3 with β=9.0; the paper has no softmax. Sum raw scores over top-p nearest clusters. |
| Latency target | **P95 routing decision ≤100ms** (router-only). 300ms is the *system* SLO including network. | 50ms was aspirational; realistic with INT8 + tight input cap is ~30ms steady state. 100ms gives headroom for tokenizer P99 and short tail spikes without paging. |
| Production bar | All four: latency SLO, shadow-mode accuracy, retrain-without-redeploy, per-decision observability | Ambitious but right for a router on the request path of every request. |

---

## P0: Working version

**Definition of done.** Cluster scorer wired into `cmd/router/main.go`,
artifacts derived from OpenRouterBench (Path A: re-cluster + re-aggregate,
no new model evals), runs entirely in-process (no Modal, no GCS, no
network on the request path), falls open to heuristic on any error
including a per-request `Embed` deadline. P95 routing decision adds ≤100ms
(INT8-quantized embedder + tiny K-means lookup; ~20-30ms steady-state).
Behavior validated end-to-end via docker-compose smoke. **Not yet
shadow-rolled, not yet known to be better than heuristic** — that gate is
Phase 1a's eval harness. Not yet observable beyond basic logs, not yet
retrainable. Those are all later phases.

### High-level architecture

```
Request
  │
  ├─► auth.Service.Route(req)
  │     ├─► extractRoutingFeatures (already done in WS1)
  │     │     └─► PromptText = last user message, tail-truncated
  │     │           (currently 2048 chars in auth/tokens.go;
  │     │            tighten to ≤1024 chars / ~256 tokens for embedder budget)
  │     │
  │     └─► router.Router.Route(req)            ◄── Strategy seam
  │           │
  │           └─► cluster.Scorer.Route(req)
  │                 ├─► embedder.Embed(PromptText)   ──► [768]float32 (L2-normed)
  │                 ├─► nearest top-p=4 centroids    ──► [K]float32 distances
  │                 ├─► uniform sum of (cluster, model) scores over top-p
  │                 ├─► α-blend with min-max-normalized cost (paper §3)
  │                 ├─► argmax → router.Decision      ──► {provider, model}
  │                 │
  │                 └─► on ANY error: heuristic.Rules.Route(req)  (fail-open)
  │
  └─► provider.Proxy(decision, req)              (auth/service.go:ProxyMessages)
```

> **Note on the existing `auth.Service.Dispatch` method:** it's defined in
> `auth/service.go:104` but never called — `ProxyMessages` goes
> `Route → provider.Proxy` directly. We don't introduce a Dispatch hop.

### File layout for `internal/router/cluster/`

```
router/internal/router/cluster/
├── embedder.go            // Jina v2 INT8-quantized ONNX wrapper via hugot
├── embedder_test.go
├── artifacts.go           // //go:embed centroids.bin rankings.json model_registry.json
├── artifacts_test.go
├── scorer.go              // implements router.Router; the Strategy impl
├── scorer_test.go
├── assets/
│   ├── model.onnx         // INT8-quantized, ~160 MB; NOT in git — pulled from HF Hub (jinaai/jina-embeddings-v2-base-code) at Docker build time and locally via scripts/download_from_hf.py
│   └── tokenizer.json     // ~700 KB
├── centroids.bin          // K × 768 × 4 bytes; ≤180 KB at K=60
├── rankings.json          // ~10 KB (per-cluster per-model α-blended scores; cost already folded in)
├── model_registry.json    // ~1 KB (bench-name → deployed-name proxy map)
└── testdata/
    ├── fixture.json       // Python-generated reference vectors for embedder integration tests
    └── scorer_cases.json  // hand-picked centroid-equal vectors for scorer unit tests
```

### Workstream 3a — Offline training (Python, dev machine, one-time)

Lives under `router/scripts/`. Run once on a dev box; outputs are committed
binary artifacts into `internal/router/cluster/`. **Path A** — we reuse
OpenRouterBench's per-(prompt, model) scores rather than running new model
evaluations; the only "training" is K-means + scoring-matrix aggregation
in our embedding space.

| Script | Purpose | One-line summary |
|---|---|---|
| `download_bench.sh` | One-shot fetch | `huggingface-cli download NPULH/OpenRouterBench --repo-type dataset --local-dir scripts/.bench-cache`. Output gitignored. |
| `inspect_bench.py` | Diagnostic | Walks `bench/*/`, prints per-benchmark row counts and per-model coverage %. Confirm Opus-4.1, Sonnet-4, and Gemini-2.5-flash columns are populated. **Run first.** |
| `export_jina_onnx.py` | Build embedder asset | Downloads `jinaai/jina-embeddings-v2-base-code` from HF, exports to ONNX via `optimum.exporters.onnx`, **applies INT8 dynamic quantization** (`onnxruntime.quantization.quantize_dynamic`), copies `tokenizer.json`. Writes both into `internal/router/cluster/assets/`. Run once. |
| `sweep_cluster_k.py` | Choose K | Re-embeds OpenRouterBench prompts with quantized Jina v2; runs K-means at K∈{10, 20, 40, 60} with `n_init=10, seed=42`; for each K reports % of (cluster, model)-rankings that are distinct (not all-Opus-then-Sonnet-then-Haiku). **Pick the smallest K with ≥80% distinct top-1.** Output goes into the plan / decisions table; not a runtime artifact. |
| `train_cluster_router.py` | Main training | Walks OpenRouterBench → embeds prompts with quantized Jina v2 → K-means at the chosen K → aggregates the bench's existing scores into per-(cluster, model) means → emits `centroids.bin` + `rankings.json`. **No model API calls.** ~150 lines. |
| `dump_cluster_test_vector.py` | Build Go test fixture | Generates a deterministic seeded embedding + expected decision; writes `testdata/fixture.json`. Mirrors the deleted `dump_test_vector.py` style. |

**Critical: train and inference must use the same embedder, and the same
quantization.** `export_jina_onnx.py` produces the INT8-quantized ONNX that
both Python training and Go inference load. Parity test: the Python ONNX
runtime and the Go hugot pipeline embed the same string and the result
must have cosine ≥ 0.99. (Cosine 1.0 isn't realistic with FFI tokenizer
differences; 0.99 is the bar.)

**Per-cluster aggregation** uses min-max normalization across models within
each cluster (paper §3): given raw scores `qⱼⁱ` for model `i` in cluster
`j`, write `q̃ⱼⁱ = (qⱼⁱ − qⱼmin)/(qⱼmax − qⱼmin)`. Same transform on cost.
Final per-cell score: `xⱼⁱ = α · p̃ⱼⁱ + (1−α) · (1−q̃ⱼⁱ)`. Default α=0.53
(paper's "matches GPT-5 at −27% cost" knee).

`model_registry.json` content for P0:

```json
{
  "deployed_models": {
    "strong":      "claude-opus-4-7",
    "medium":      "claude-sonnet-4-5",
    "weak":        "claude-haiku-4-5"
  },
  "bench_to_deployed": {
    "anthropic/claude-opus-4.1":   "claude-opus-4-7",
    "anthropic/claude-sonnet-4":   "claude-sonnet-4-5",
    "google/gemini-2.5-flash":     "claude-haiku-4-5"
  }
}
```

The Gemini-Flash → Haiku mapping is the load-bearing proxy that should be
revisited the moment we have real-traffic-trained data. Document it
prominently; it's not a bug, it's a P0 stand-in.

### Workstream 3b — `internal/router/cluster/` Go package

**`embedder.go`** — local ONNX inference. Two viable libraries:

- **Recommended: `github.com/knights-analytics/hugot`** — sentence-transformer
  pipeline wrapper. Handles tokenization + inference + mean-pooling +
  L2-norm in one `Pipeline.Run(text)` call. ~10 lines of code.
- Alternative: `github.com/yalue/onnxruntime_go` + `github.com/daulet/tokenizers`.
  Lower level — we'd own pooling + masking. ~80 lines. Only worth if hugot's
  dep weight or its API turns out to be a problem.

Constructor signature: `NewEmbedder(modelPath, tokenizerPath string) (*Embedder, error)`.
The session is goroutine-safe — share one instance across all requests.
Embedded asset bytes get written to `os.TempDir()` once at boot (hugot
loads from the filesystem).

```go
type Embedder interface {
    Embed(ctx context.Context, text string) ([]float32, error)  // returns 768-dim L2-normalized
}
```

**`artifacts.go`** — load on package init.

- `loadCentroids()` — parse `CRT1` magic, K, dim, then `[K][dim]float32`. Fail-fast on dim mismatch with embedder.
- `loadRankings()`, `loadRegistry()` — `json.Unmarshal`. `rankings.json` carries pre-blended per-cell `xⱼⁱ` values (cost already folded in via the paper's α-blend at training time).
- All embed-bytes private to package; only parsed structs exposed.

> No `costs.json` at runtime. Costs are baked into `rankings.json` during
> training via min-max normalization + α-blend. Changing α requires a
> retrain — that's by design (it makes runtime scoring a single sum +
> argmax, no per-request cost lookup).

**`scorer.go`** — the Strategy implementation. **Algorithm pinned to paper
§3** (no plan-invented mechanics).

```go
type Config struct {
    TopP           int     // # of nearest clusters to sum over; default 4 (paper §4.1.3)
    Alpha          float64 // performance/cost blend; default 0.53 (paper Table 3 knee)
    MinPromptChars int     // default 20; below this, fall through to heuristic
    MaxPromptChars int     // default 1024 (~256 tokens) — tail-truncate before embed for latency
}

type Scorer struct { /* embedder, centroids, rankings, registry, fallback, log */ }

func NewScorer(cfg Config, embedder Embedder, fallback router.Router) (*Scorer, error)
func (s *Scorer) Route(ctx context.Context, req router.Request) (router.Decision, error)
var _ router.Router = (*Scorer)(nil)
```

> `rankings.json` is already min-max-normalized at training time and
> already contains the per-cell `xⱼⁱ` values, not raw quality/cost. The
> runtime scorer just sums + argmaxes — no normalization or cost lookup
> on the request path.

`Route` body, in order:

1. If `len(req.PromptText) < cfg.MinPromptChars` → fallback at Debug.
2. `text := tailTruncate(req.PromptText, cfg.MaxPromptChars)`.
3. `vec, err := s.embedder.Embed(ctx, text)` — on error, fallback at Warn.
4. Distances: `[K]float32` via `1 - dot(vec, centroid_k)`. Hand-rolled (K × dim=768 = trivial; K decided by sweep, expect K ≤ 60).
5. Top-p nearest: pick `cfg.TopP` lowest-distance clusters. **Uniform sum, no softmax** (paper §3): for each deployed model `m`, `score[m] = Σ_{k ∈ top_p} rankings[cluster_k][m]`.
6. Argmax over `score`. Build `Decision{Provider:"anthropic", Model:deployedName, Reason:"cluster:top_p=[k1,k2,k3,k4] model=m"}`.
7. Log decision at Info, structured: `top_clusters`, `model`, `embed_ms`, `score_ms`, `prompt_chars`.

**Two things deliberately not in the algorithm at P0:**

- No softmax temperature (β). The earlier draft attributed β=9.0 to the paper; the paper has no softmax.
- No γ-latency term. The paper has a single α∈[0,1] tradeoff between performance and cost. Latency is enforced as a hard cap upstream (drop any model whose P95 exceeds the system SLO from the candidate set), not a smooth weight. See "Beyond the paper" for future iteration.

**`scorer_test.go`** — fake `Embedder` returning fixture vectors.

- Empty / sub-`MinPromptChars` `PromptText` → fallback called.
- Embedder error → fallback called, Warn logged.
- Centroid-equal vector → expected cluster's top model wins.
- Top-p aggregation: vector equidistant from two clusters with opposing top-1 models → tied score broken deterministically.
- Tail-truncation: `PromptText` longer than `MaxPromptChars` is truncated before reaching the embedder fake (assert via captured arg).

**`embedder_test.go`** — gated by build tag `//go:build onnx_integration`.

- Loads real ONNX + tokenizer.
- Embeds known sentences, asserts cosine ≥ 0.99 vs Python-generated reference vectors in `testdata/fixture.json`.
- Skipped in default `make test`; opt-in for full validation. (Even the INT8-quantized 40-50MB model + libonnxruntime is too heavy for CI default.)

### Workstream 3c — Wiring

**`cmd/router/main.go`** — replace the heuristic-only branch:

```go
heuristicRouter := heuristic.NewRules(heuristic.Config{
    SmallModel:      "claude-haiku-4-5",
    LargeModel:      "claude-opus-4-7",
    ThresholdTokens: 1000,
})

// Cluster scorer wraps heuristic as fail-open fallback. If embedder
// or artifact loading fails, we silently degrade to heuristic.
embedder, err := cluster.NewEmbedder(modelTmpPath, tokenizerTmpPath)
if err != nil {
    logger.Error("Cluster embedder failed; using heuristic only", "err", err)
    rtr = heuristicRouter
} else {
    scorer, err := cluster.NewScorer(cluster.DefaultConfig(), embedder, heuristicRouter)
    if err != nil {
        logger.Error("Cluster scorer failed; using heuristic only", "err", err)
        rtr = heuristicRouter
    } else {
        rtr = scorer
        logger.Info("Routing via cluster scorer", "embedder", "jina-v2-base-code")
    }
}
```

No env-var gates; no `ROUTER_STRATEGY`. The scorer is always built when
artifacts load. Simpler state machine.

**`Dockerfile`** changes — bigger jump than earlier drafts implied. Current
state (verified via `Dockerfile`): runtime stage is `alpine:3.19`, build
stage uses `golang:1.25.9-alpine3.23` with `CGO_ENABLED=0` (static
binary). hugot + onnxruntime needs dynamic linking against `libonnxruntime.so`,
which Alpine (musl) does not support out of the box. Required changes:

1. **Builder stage:** switch base from `golang:*-alpine` to `golang:1.25.9-bookworm` (glibc). Download ONNX Runtime tarball into `/opt/`. Set `CGO_ENABLED=1`, `CGO_CFLAGS=-I/opt/onnxruntime-*/include`, `CGO_LDFLAGS=-L/opt/onnxruntime-*/lib -lonnxruntime`.
2. **Runtime stage:** switch from `alpine:3.19` to `gcr.io/distroless/cc-debian12` (glibc 2.36, ships with the C runtime needed by libonnxruntime). `COPY --from=builder /opt/onnxruntime-*/lib/libonnxruntime.so* /usr/lib/`. Versioned `.so` (e.g. `libonnxruntime.so.1.18.0`) is required, not just the symlink.
3. **Image size:** ~25 MB → ~270 MB (~160 MB INT8 model + ~50 MB libonnxruntime + base). Acceptable.
4. **Vuln triage:** moving off the static-Alpine binary loses some scanner-friendly properties. Confirm with whoever owns image scanning that the distroless/cc switch is OK.

**Model artifact distribution (HuggingFace Hub, not Git LFS).** The
~160 MB INT8 ONNX is not committed to git. It lives in Jina's public
HF repo (`jinaai/jina-embeddings-v2-base-code`); we pin
`HF_MODEL_REVISION` to a specific commit SHA so deploys are
reproducible. The Dockerfile pulls it during build via `curl` against
the HF resolve URL (no `HF_TOKEN` required for the public repo); local
dev uses [`scripts/download_from_hf.py`](scripts/download_from_hf.py).
This avoids the LFS billing/checkout footguns. `HF_MODEL_REVISION` in
the Dockerfile pins the deployed weights to a specific HF commit
SHA — bump it deliberately when promoting a new train.

**Latency budget** (P0, in-process, P95 target ≤100ms router-only; 300ms
is the *system* SLO including upstream network + provider TTFB).

Realistic numbers, INT8-quantized Jina v2, ≤256-token input on a 2 vCPU
Cloud Run instance:

| Step | Estimated | Notes |
|---|---|---|
| Tokenize (hugot FFI) | ~2-4ms | Plan's earlier "1ms" was too optimistic |
| Jina v2 INT8 ONNX inference (CPU) at ≤256 tokens | ~15-25ms | FP32 is ~25-40ms; INT8 is required to hit budget |
| L2 normalize | <0.1ms | |
| K dot products × 768 dim (K ≤ 60) | <1ms | |
| Top-p sum + argmax | <0.1ms | |
| **Total embed + score, P50** | **~20-30ms** | |
| **Total embed + score, P95** | **~40-60ms** | Tail dominated by tokenizer + occasional GC |
| Heuristic fallback (rare, on error) | ~0.05ms | |

**Sequence-length scaling is the load-bearing risk.** BERT inference is
O(n²) attention + O(n) FFN. The plan's `MaxPromptChars = 1024` (~256
tokens) keeps the embedder well below the budget. Without that cap (e.g.,
the existing `auth/tokens.go` cap of 2048 chars ≈ 512 tokens), inference
roughly doubles and we breach 100ms on most prompts. **The cap is
load-bearing — if it's relaxed for any reason, re-run the latency test.**

**Cold start: 3-6s, not 1s.** ONNX session create + INT8 model load + graph
optimize + first-inference penalty. Several mitigations stack:

- Cloud Run `min_instances >= 1` keeps the warm pool warm, but **autoscale
  spinups still pay this cost**. Any request landing on a fresh instance
  during scale-out sees ≥3s tail latency.
- Embedder warmup: `cluster.NewEmbedder` runs `Embed(ctx, "warmup")` before
  `srv.ListenAndServe()` returns. Takes the first-request hit off the
  critical path within an instance.
- The autoscale tail is unavoidable at the embedder level. The fail-open
  fallback to heuristic is what protects users — if the embedder hasn't
  warmed up within a per-request deadline (e.g., 200ms ctx with-timeout
  around `Embed`), the scorer falls through to heuristic for that request.

**Memory.** INT8 working set is ~500-700MB (weights ~40-50MB + ONNX arena
~200-400MB + Go runtime). FP32 is ~1.0-1.2GB. **Cloud Run instance: 1GiB
minimum for INT8.** Worth specifying explicitly in the deploy config —
default is 512MiB which OOMs on first inference.

### P0 testing & verification

1. **Unit tests** — `go test ./internal/router/cluster/...` with fake embedder.
2. **ONNX integration test** — `go test -tags onnx_integration ./internal/router/cluster/...` for Python↔Go embedder parity (cosine ≥ 0.99 on fixture vectors).
3. **Bench inspection** — `python scripts/inspect_bench.py` to confirm row counts, model coverage, and that Opus 4.1 / Sonnet 4 / Gemini-2.5-flash columns are populated.
4. **K sweep** — `python scripts/sweep_cluster_k.py` to pick K (≥80% distinct top-1 cells across (cluster, model) rankings). Update `Cluster count K` in the Decisions table once chosen.
5. **Training smoke** — `python scripts/train_cluster_router.py --sample-size 100 --dry-run` first, then full run with the chosen K. Held-out top-1 accuracy printed; sanity-check that v0.1 ≥ always-Opus baseline on bench data.
6. **End-to-end smoke** — `docker compose up --build -d`; exercise `/v1/messages` with (a) trivial prompt, (b) hard prompt; confirm logs show different `model=` decisions.
7. **Latency smoke** — `wrk` against the local stack; confirm P95 routing decision ≤100ms (full request P95 obviously larger from upstream call).
8. **Full local CI** — the standard `wv gg && git diff --quiet && wv be tc && ...` chain.

---

## Production readiness

P0 ships a router that *works*. It does not ship a router we *trust*. The
prod-ready half of this plan is what makes the router something we'd point
every Claude Code request at. It has five threads:

1. **Training pipeline** — three datasets layered together, with a Bayesian
   update that keeps the scoring matrix fresh as traffic shifts and as
   models silently change under us.
2. **OTel-native observability** — every routing decision and every
   downstream call instrumented under OpenTelemetry semantic conventions.
   Spans, metrics, and a long-term decision log all flow through the
   same OTel collector pipe the rest of Weave already uses.
3. **Latency SLO** — load tests, alerts, warmup, fail-open guarantees.
4. **Shadow / promotion gates** — every router version (`v0.1` → `v0.5`
   → `v1.0` → ongoing posterior updates) earns its way into the primary
   path through measured comparison against the previous version.
5. **Retrain without redeploy** — artifacts in GCS, fetched on boot,
   versioned, atomically rollback-able.

Order of landing: **eval harness first** (Phase 1a — see Phases section).
That's the gate that tells us whether the architecture is worth investing
in beyond P0. Then training pipeline (D2 probe set + judge), then OTel +
shadow + retrain alongside the v0.5 ship in Phase 2. Holding OTel until
v0.5 is deliberate — staging traffic is zero and the data would be
dropped anyway, so the instrumentation lands when it actually catches
something.

**Important context: the router is not yet serving real production traffic.**
D3 starts empty. While D3 is below threshold, the probe set (D2) is the
*only* distribution-relevant signal we have — not just a periodic benchmark.

The router-version naming scheme used throughout:

| Version | Trained on | Job |
|---|---|---|
| `weave-router-v0.1-bootstrap` | D1 only (OpenRouterBench) | "Does cluster-and-score work end-to-end? Latency budget holds?" Staging only. Phase 1a's eval harness is the quality gate. |
| `weave-router-v0.5-probe` | D1 prior + D2 strong updates | The **first prod router**. Beats heuristic + always-Opus on Phase 1a's eval harness. Initial rollout: shadow against heuristic for data-collection, then promote to primary after a disagreement audit shows it doesn't disagree wildly with heuristic on cases the heuristic obviously gets right. |
| `weave-router-v1.0` | D1 + D2 + D3 (first batch ≥10k decisions + ≥1k judgments) | First retrain that includes real traffic. Promoted via shadow-against-v0.5 with statistical significance gates (≥1k disagreements, p<0.01). |
| `weave-router-v1.x` | + ongoing D3 posterior updates | Steady state: nightly Beta-posterior refresh, scheduled probe-set refresh, model-pool extensions via cold-start protocol. |

---

## Training pipeline

### The shape of the problem

AvengersPro routing needs a per-`(cluster, model)` cell with two numbers:
a quality score and a cost number (paper §3). At runtime we serve a
single α-blended value; the offline pipeline tracks them separately so we
can re-blend with a different α without re-running model evaluations.
Latency is enforced as a hard cap upstream, not a third dimension at P0
(see "Beyond the paper" for when that changes). Everything in this
section is in service of populating quality and cost reliably and keeping
them fresh as models and traffic drift.

**Why one dataset isn't enough.** OpenRouterBench
(`NPULH/OpenRouterBench`, the AvengersPro paper's evaluation data — 8
models × 6 benchmarks × ~2,600 prompts, with per-(prompt, model) scores
already computed) is an excellent bootstrap because the model outputs are
pre-computed — no GPU dollars to replicate. But three gaps matter for
shipping in Workweave:

1. **The model pool isn't ours.** The bench has Anthropic models but not
   the exact deployed versions. Capability scores transfer roughly, not
   exactly. Document every cell as a prior to be overwritten, not a
   ground truth.
2. **The task mix isn't ours.** Heavy on academic reasoning / math /
   code-completion; light on agentic tool-use, multi-turn coding, and
   long-context document work — which is what Claude Code traffic
   actually looks like.
3. **The judge methodology is fixed.** We want our own judge protocol,
   applied consistently to bench prompts AND production traffic, so
   labels are comparable across datasets.

So the plan is **three datasets layered with a Bayesian update**:

| Dataset | Source | Size | Job |
|---|---|---|---|
| **D1: Bootstrap** | OpenRouterBench, filtered to Opus 4.1 + Sonnet 4 + Gemini-2.5-flash columns | ~2,600 rows | Seed cluster centroids + initial score matrix. Inherited from the AvengersPro paper's evaluation; no new model evals. |
| **D2: Probe set** | Curated, judge-labeled by us | 500–1000 prompts × N models | Stable benchmark. Re-run on every model change, every checkpoint refresh, weekly cron. The hot-reload path. |
| **D3: Production trace** | Sampled real Weave traffic with async judge labels | Grows over time, target 10k/week | The dataset that actually matches our distribution. What `v1.x` learns from. |

How they compose:

```
              ┌─────────────────────────────────────────┐
              │  D1 BOOTSTRAP (OpenRouterBench filtered)│
              │  → cluster centroids                    │
              │  → initial per-(cluster, model) scores  │
              │  → router-v0.1 (staging only)           │
              └─────────────────────────────────────────┘
                              │
                              ▼  (overwrite cells with strong observations)
              ┌─────────────────────────────────────────┐
              │  D2 PROBE SET (~750 curated prompts)    │
              │  → re-score full matrix weekly + on     │
              │    every model change                   │
              │  → router-v0.5 (shadow rollout)         │
              └─────────────────────────────────────────┘
                              │
                              ▼  (Bayesian posterior update)
              ┌─────────────────────────────────────────┐
              │  D3 PRODUCTION TRACE + ASYNC JUDGE      │
              │  → Beta posterior on every cell, refresh│
              │    nightly                              │
              │  → router-v1.x (production)             │
              └─────────────────────────────────────────┘
```

### D1 — Bootstrap from OpenRouterBench

What this section covers is mostly already specified in P0 (workstream
3a — `download_bench.sh`, `inspect_bench.py`, `sweep_cluster_k.py`,
`train_cluster_router.py`). The production-side responsibilities:

- **Cluster centroids are derived once and fixed.** Re-running K-means
  produces different cluster IDs and breaks the layering of D2 / D3
  observations on stable buckets. Bumping the cluster geometry is a
  separate, more invasive change (treat as a new `v2.x` major bump). The
  paper itself doesn't have a retrain protocol; this fixed-centroids
  policy is plan-invented operating mode.
- **Score cells from D1 are priors, not ground truth.** The Bayesian
  updater (below) tracks raw quality per (cluster, model) as a Beta
  posterior. Centered on the bootstrap quality `m ∈ [0,1]` with total
  effective weight 2 (deliberately weak): `Beta(α₀ = 2m, β₀ = 2(1−m))`.
  D2 / D3 observations easily overwrite it. Cost is *not* under the
  posterior — it's a known per-model price ($/1k tokens), so it stays
  fixed in the input to the α-blend at retrain time.
- **Tag every shipped artifact with the source mix.** `latest.json` carries
  `training_data_mix: {"d1": 1.0, "d2": 0.0, "d3": 0.0}` for `v0.1`,
  shifts as later datasets layer in. This is what lets us roll back to
  a known mix if a retrain produces bad routing.

### D2 — Probe set (the production training data, not just a benchmark)

This is the single most leveraged artifact in the whole pipeline. With D3
starting empty, **D2 is what trains the first prod router**. That changes
its job description vs the AvengersPro / UniRoute literature:

- **Their probe set role**: a fixed benchmark used to (a) re-score
  matrices when models change and (b) cold-start new models. ~500–1000
  prompts is enough.
- **Our probe set role while D3 is below threshold**: the same as above
  PLUS it IS the training distribution. Bigger and more thoughtfully
  composed matters more than it would in a "real traffic available from
  day 1" setup.

**Recommendation: start at ~750 prompts, plan to grow to ~1500** as we
identify which slices the cluster scorer disagrees on. The hand-curated
long-context and multilingual slices benefit most from expansion; the
Aider-Polyglot-derived coding slices saturate quickly. Trigger the
expansion once we see clusters with `α + β < 10` in the posterior — those
are the cells the probe set hasn't touched enough.

**Composition target (initial ~755 prompts; expand as needed):**

| Slice | Count | Source |
|---|---|---|
| Coding — Python | 75 | LiveCodeBench + a few from internal Workweave PRs (anonymized) |
| Coding — TS/JS | 60 | LiveCodeBench-TS + Aider Polyglot |
| Coding — Go | 50 | Aider Polyglot Go + custom from the Workweave monorepo |
| Coding — Rust/C++/Java | 60 | Aider Polyglot remainder |
| Coding — SQL | 30 | BIRD-SQL sample |
| Tool-calling — single | 60 | BFCL v4 simple |
| Tool-calling — parallel/multi | 60 | BFCL v4 parallel + multi-turn |
| Tool-calling — agentic | 40 | τ-bench sample |
| Math/reasoning | 60 | GPQA-Diamond + MATH sample |
| Long-context (>20k tokens) | 50 | Custom: real long PRs / docs from our traffic |
| Summarization/extraction | 50 | Real customer docs (anonymized) |
| Chat/prose | 50 | Chatbot Arena sample |
| Multilingual | 50 | Translated subset of the above, ~10 languages |
| Edge cases (refusals, ambiguity, multimodal) | 60 | Hand-curated |
| **Total** | **~755** | |

The composition can shift, but the slices are picked to match Claude
Code traffic shape. Custom slices ("Coding — Go from our monorepo",
"Long-context real PRs") are explicitly there to bias toward our
distribution and away from generic benchmark-leakage.

**Storage:** the probe set lives in
`gs://workweave-router-probes/v1/probe.jsonl` with a JSON manifest
(slice → row range). Versioned via filename — never edit `v1`; cut a
new `v2` and migrate.

### Labeling protocol — applied to D2 and D3 identically

This is where to be careful. Same protocol on both datasets means we can
mix observations across them without bias correction.

**Per prompt:** run all N (3 today, 10 eventually) deployed models, capture
full output + token counts + latency + cost. Then judge.

**Two judges, not one:**

- **Reference-based judge** for tasks with ground truth: code → does it
  pass tests, math → is the answer right, tool-calling → does the
  structured output match. Score is binary or pass-rate.
- **LLM-as-judge** for tasks without ground truth: chat, summarization,
  prose. **Use a frontier model not in our routing pool** — Opus judging
  Opus is biased. Cross-family judge ensemble: judge with two
  independent models (e.g. GPT-5 + Gemini 2.5 Pro), take median; flag
  disagreements >0.3 for human review.

**Anti-bias measures from the LLM-judge literature** (these are not
optional):

- **Randomize answer order** before showing to the judge. Position bias
  is real and large.
- **Strip model identity** from outputs before judging.
- **Use pairwise + score, not just score.** Pairwise is more reliable;
  absolute scores drift over time.
- **Score on a rubric with concrete dimensions.** `{correctness,
  completeness, code_quality, style, follows_instructions}` each 1-5,
  then aggregate. Not "which is better."
- **Sample 5–10% for human spot-check** and compute Cohen's κ between
  judge and human. **If κ < 0.6, the rubric is broken.** Fix before
  trusting any score.

**Output artifact:**
`gs://workweave-router-training/<run_id>/judgments.jsonl`, one row per
`(prompt_id, model_id)` with the rubric breakdown, the aggregated score
(`judged_score ∈ [0, 1]`), the reference-based pass/fail when available,
and metadata (judge model, judge version, prompt_hash).

**Refresh cadence for D2:**

- **Cold-start phase (D3 below threshold):** event-driven only. Re-run on
  every model checkpoint update or every probe-set expansion. No
  schedule-based cron — burns budget without producing signal when
  traffic is zero.
- **Steady-state (D3 carries the posterior):** scheduled cron, weekly is
  the AvengersPro default. Cheap insurance against silent provider
  model updates ($50–150/run × 3 models).

### D3 — Production trace + async judge

This is the dataset that makes the router actually good in our
distribution. **It starts empty.** The instrumentation ships with v0.5
(so we don't lose data once traffic flows) but the judge job and
posterior updater don't run until D3 crosses threshold (≥10k decisions
- ≥1k judgments).

**Capture (synchronous, in the request path).** This is OTel-native — see
the Observability section below for the full attribute schema. Per request,
the router emits a `router.routing_decision` span with:

- `prompt_features` — token count, has_tools, has_image, conv_depth, language, code language detected
- `prompt_embedding` — 768 floats from Jina v2, quantized to int8 for storage (~768 bytes per row)
- `prompt_hash` — sha256 of `PromptText` (always present; raw text only when consent flag set)
- `candidate_models`, `filter_dropped_models` — the routing search space and any policy filters applied
- `predicted_score {model: x}` — the α-blended per-cell score the scorer actually summed (cost already folded in at training time)
- `chosen_model`, `chosen_reason`, `exploration_flag` — the decision and whether ε-greedy exploration kicked in
- `alpha` — performance/cost tradeoff (per-request override if header set, else org default, else global)

**Capture (post-call, attached to the same trace via `request_id`):**

- `actual_tokens_out`, `actual_cost_usd`, `actual_ttft_ms`, `actual_total_ms`
- `finish_reason`, `error_class`

These come back from the Anthropic provider after the upstream call
completes. Same OTel span; child of the routing-decision span.

**Judge (async, sampled).** Don't judge every request — too expensive, too
slow. A nightly Cloud Run Job (or Modal job, mirroring `models/v2/`) pulls
samples, runs the labeling protocol, writes scores back to a `judgments`
table joined on `request_id`.

Sampling policy:

- **1–5% uniform** over all traffic, for general distribution coverage.
- **100% of high-value slices**: high-priority workspaces, requests
  flagged `cost_usd > $0.50` (long-context Opus calls), and
  `exploration_flag = true` requests (so we always learn from the
  exploration arm).

**Implicit signals — capture these too, they're free.** No judge call
required and directly tied to our traffic:

- Did the user regenerate within 60s? → likely thumbs-down signal.
- Did they copy the output (if Workweave UI exposes a copy event)? → likely thumbs-up.
- Did the conversation continue? → success.
- Did they hit "stop generating"? → bad.
- Did the assistant's tool-call result in a successful tool execution downstream? → quality signal.

These are noisy but free. Combine with judge labels via a small
calibration model that estimates `P(judge_acceptable | implicit_signals)`
on the labeled subset, then use that calibration on the unlabeled tail.

**Cold-start note:** the implicit-signal pipeline lights up only once
traffic flows. Wire the capture with v0.5 so data doesn't get dropped,
but the calibration step against judge labels needs both (a) ≥1k
implicit-signal rows and (b) ≥500 judged rows before the calibration
is statistically useful. Treat the first calibration as a milestone, not
an automatic step in the retrain pipeline.

**Privacy / PII handling.** Hash prompt text by default; store full text
only when the workspace `prompt_retention_opt_out` flag is false. The
existing `RedactPromptContentProcessor` (`backend/internal/app/tasks/redact_prompt_content.go`)
implements the redaction pipeline; **the exact table that holds Claude
Code prompt text is an open question** (an earlier research agent named
`ai_user_prompts.prompt_content` but the migration isn't visible — see
Open Questions). Resolve before D3 capture depends on it. Embeddings can
leak content via inversion attacks — encrypt at rest, drop after 90 days
unless the workspace has explicit long-term-retention. Document the
policy in `router/PRIVACY.md`.

### Bayesian update model

Each `(cluster, model)` cell holds a Beta posterior over success rate.
The update rule:

```
prior:        Beta(α₀, β₀)   where α₀=2·m, β₀=2·(1−m); m = bootstrap quality from D1
              (mean = m, total weight = 2 — deliberately weak, easily
               overwritten by data)

D2 update:    α := α + N_probe_successes
              β := β + N_probe_failures
              (full-strength observations — D2 has been judged with
               our protocol, so they're trusted)

D3 update:    α := α + s · N_traffic_successes
              β := β + s · N_traffic_failures
              where s = min(1, ESS_cap / (α + β))
              (effective-sample-size cap keeps the posterior responsive
               to drift — ESS_cap = 1000 means a cell never accumulates
               more than 1000 effective observations, so a provider
               quality regression starts moving the score within ~7 days
               at typical traffic volumes)
```

**Why Beta posteriors and not arithmetic means.** Two reasons. (a)
Uncertainty: a cell with 5 observations should be weighted differently
from one with 500. The Beta encodes this for free; the cluster scorer
can use posterior mean OR posterior mean − k·posterior_std to pick a
"conservative" model when the posterior is wide. (b) Drift response:
the ESS cap means cells stay responsive to provider model updates. Pure
incremental averaging would have a 100k-observation cell never move
again.

**Implementation home.** The posterior update is pure-Python and runs in
Modal as part of the retrain pipeline. The router itself only ever reads
the materialized scores out of `rankings.json`.

### Cold-start protocol — adding a new model (model #4 onwards)

This is what the probe set buys us. When we add (say) Gemini 2.5 Pro to
the deployed pool:

1. Run the probe set through the new model — 750 prompts, judge protocol,
   full row in the cell matrix. Cost: ~$15 for one model.
2. Insert the new model into `model_registry.json`. Use its probe scores
   as initial cell values.
3. Re-blend `rankings.json` so the new model has scores in every cluster
   it covers; deploy.

This is the part the paper claims (§1 ¶7): "incremental evaluation of the
new models on the dataset" is enough. **Below is plan-invented operating
policy on top of that** — under-exploration prior + ε-greedy ramp — which
the paper does *not* describe but which we want as a safety net for a
production router:

- **Conservative Beta prior:** `Beta(α₀=1, β₀=2)` on the new model's
  cells, so the Bayesian updater under-explores it until D3 evidence
  accumulates.
- **Traffic ramp:** start at 5% of cluster-eligible requests, double per
  day if no quality regression.
- **ε-greedy boost:** force a small fraction of requests onto the new
  model regardless of cluster score, so we always learn from it; drop the
  boost once each cell has ≥50 D3 observations.

The plan-invented parts are revisitable — if Phase 1a / 2 show simpler
approaches work, drop them.

### Routing knobs — α tradeoff (P0)

The cluster scorer at request time computes (paper §3, restated):

```
score(c, m) = Σ_{k ∈ top_p}  α · p̃(k, m) + (1 − α) · (1 − q̃(k, m))
```

where `p̃` and `q̃` are min-max-normalized performance and cost per cluster.
`α ∈ [0, 1]` is the only knob: `α = 0` is pure cost-minimizing, `α = 1` is
pure performance-maximizing. **No latency term at P0** — latency is
enforced as a hard cap (drop any model whose published P95 exceeds the
system SLO) before scoring, not as a smooth weight.

**Default α = 0.53.** Paper Table 3 shows this knee matches GPT-5-medium
average accuracy at −27% cost across 8 models. With our 3-Anthropic
subset, the calibrated value will likely shift; the eval harness (Phase
1a) measures it on Claude Code traffic and tunes the default.

**Per-request override (P1, not P0):** custom HTTP header
`x-weave-routing-alpha: 0.8` for clients that want a different point on
the Pareto frontier (e.g. a cron job that prefers cost). Org-level
defaults stored in `model_router_installations` override global; per-request
beats org beats global. Wait for a customer to ask before shipping the
header — it's not P0.

> **A latency weight (γ) and other multi-objective dimensions (per-language
> coding, tool-calling, modality) are explicitly research-backlog, not P0.
> See "Beyond the paper" below.**

---

## Beyond the paper

P0 implements AvengersPro faithfully (paper §3): cluster the embedding,
sum scores over top-p clusters, blend performance vs cost via a single α.
That captures task-difficulty implicitly through the embedding geometry,
which is what the paper bets on. It's a clean baseline — but it's also
demonstrably less than what's possible. Each item below is an iteration
to land **only after v0.5 ships and the eval harness shows where the
baseline is leaving accuracy / cost on the table.** Order is by expected
leverage; none are P0 commitments.

### Cluster metadata tagging (highest leverage)

Today the scorer treats clusters as opaque indices. They have semantic
content — cluster 7 might be "Python coding," cluster 19 might be
"agentic tool-calling on long context." If we surface that as cluster
metadata, two things become possible:

- **Per-tag scoring overrides.** "When the cluster is `tool_calling`,
  prefer Sonnet by default even if Opus's score is higher" — captures
  domain knowledge the paper has no way to express.
- **Better debuggability.** A routing decision logged as `cluster=19
  (agentic tool-calling) → Sonnet` is human-readable; today's `cluster_id=19`
  isn't.

Implementation sketch: nightly job runs an LLM over the prompts assigned
to each cluster, asks for a 3-word descriptor + dimension tags
(`{coding_lang: "python", task_type: "tool_calling", modality: "text"}`).
Tags are advisory metadata in `rankings.json`; the scorer can optionally
apply tag-conditional overrides.

### Per-language and per-task scoring axes

Cluster geometry implicitly captures coding language ("write a Go
function" embeds near other Go prompts), but the paper's score matrix
collapses everything into one number per (cluster, model). If model A is
better at Go and model B is better at Python within the same cluster,
the matrix can't represent it. Layered axes:

- `rankings_by_lang.json` — per-(cluster, language) per-model scores.
- Score formula adds a language detector (cheap heuristic or fastText)
  → looks up the language-specific row when present, falls back to the
  language-agnostic row otherwise.

Worth it only if the eval harness shows a meaningful per-language gap
between models. If GPT-5 dominates Python and Claude dominates Go in our
data, this matters; if not, it's noise.

### γ-latency as a smooth weight

P0 enforces latency as a hard cap (drop slow models from the candidate
set). For latency-sensitive clients, a smooth tradeoff might be
preferable: `score(c, m) = α · p̃ + β · (1 − q̃) + γ · (1 − ℓ̃)` with
α + β + γ = 1. The paper doesn't have this; it would be our extension.
Open question: where does latency data come from at routing time —
historical P95 from `weave.router.upstream_call.duration_ms`, baked into
`rankings.json` like cost is? Probably yes.

### Multi-objective Pareto routing

Rather than collapsing to one scalar score, return the Pareto frontier
of `(quality, cost, latency)` and let the client pick. Heavy lift, low
priority — only matters if customers ask for it.

### Submit to RouterArena

Once v0.5 ships, submit our router to RouterArena
([routerarena.org](https://routerarena.org) or whatever the current entry
URL is) for external benchmark validation. Cheap signal: our internal eval
might be biased by the probe set we curated; an external benchmark tells
us whether the architecture generalizes. Tracked here so we don't forget;
not a phase gate.

### RouterBench (Martian) as ablation dataset

[RouterBench](https://huggingface.co/datasets/withmartian/routerbench)
ships ~405k pre-computed model outputs across 11 models. Useful as a
*second* offline dataset alongside OpenRouterBench for ablations: does
our K choice transfer? Does our α calibration transfer? Free download,
no inference cost. Add as `D1.5` if Phase 1a's eval harness shows
volatility we want to debug; skip if results from OpenRouterBench alone
look stable.

### Speculative research direction — beyond AvengersPro

Everything above is incremental — extensions that stay within the paper's
single-shot, offline, prompt-embedding-only frame. The bigger swing is
to break that frame and target coding agents directly.

[`FUTURE_RESEARCH.md`](FUTURE_RESEARCH.md) sketches a four-layer system
(step-typed cluster routing + confidence-gated speculative escalation +
cache-aware switching cost + decision-aware online dueling-bandit
training) that combines ideas from MTRouter, STEER, EquiRouter, and
Anthropic prompt-cache economics. The contributions: first router to
condition on agent step type, first to cost KV-cache invalidation in
the routing decision, and first to validate on agentic coding
benchmarks (SWE-bench-Live / FeatureBench).

Strictly post-v0.5 — none of it competes with the production-readiness
work in this plan. Treat it as the "what comes after we've shipped
AvengersPro and have real coding-agent traces" reading.

---

## OTel observability

**Replace** the ad-hoc `routing_decisions` Postgres table from the
earlier draft. Everything is OpenTelemetry-native, lands in the same
collector pipe Workweave already uses for Claude Code OTLP ingestion
(`backend/internal/app/api/ingestapi/unified.go`), and persists to the
same long-term storage (Postgres for hot, ClickHouse via `chpipeline`
for cold).

### Spans

The router instruments three spans per request, parent → child:

```
router.handle_messages              (root span, gin handler)
├── router.routing_decision         (cluster scorer + heuristic fallback)
│   └── router.embed                (Jina v2 ONNX inference)
└── router.upstream_call            (Anthropic provider call)
```

**Semantic conventions** — use the OpenTelemetry GenAI namespace where it
exists (`gen_ai.*`), put router-specific bits under `weave.router.*`.
Concretely the attribute set on `router.routing_decision`:

| Attribute | Source | Notes |
|---|---|---|
| `gen_ai.system` | const | `"anthropic"` |
| `gen_ai.request.model` | request body | what client asked for |
| `gen_ai.response.model` | scorer | what we routed to (becomes `gen_ai.response.model` officially after upstream) |
| `gen_ai.usage.input_tokens` | post-call | filled in by `router.upstream_call` span |
| `gen_ai.usage.output_tokens` | post-call | same |
| `weave.router.version` | const | `"weave-router-v1.0"` etc — built-time stamp |
| `weave.router.strategy` | scorer | `"cluster" \| "heuristic" \| "shadow:cluster:heuristic"` |
| `weave.router.cluster.id` | scorer | argmax cluster id; absent for heuristic |
| `weave.router.cluster.distance` | scorer | top-1 cluster cosine distance (smaller = closer) |
| `weave.router.cluster.top_k` | scorer | comma-separated top-K cluster ids |
| `weave.router.candidate_models` | scorer | JSON array of considered models |
| `weave.router.predicted_score` | scorer | JSON `{model: x}` — the α-blended per-cell score (cost already folded in) |
| `weave.router.alpha` | request/org | performance/cost tradeoff knob ∈ [0, 1] |
| `weave.router.exploration` | scorer | bool — was this an ε-greedy exploration pick |
| `weave.router.fallback.reason` | scorer | populated when fail-open path taken |
| `weave.router.prompt.hash` | extractor | sha256 first 16 hex chars |
| `weave.router.prompt.chars` | extractor | for distribution analysis |
| `weave.router.prompt.embedding_b64` | scorer | int8-quantized embedding, base64 — only attached when sampled (1% default) |
| `weave.router.installation_id` | auth | from `auth.Service` |

The `prompt.embedding_b64` attribute is heavy (~1 KB even quantized).
**Only attach on sampled traces** (1% default, 100% for `exploration=true`
and high-cost requests). This is what lets the async judge job
re-cluster historical decisions without re-embedding from raw text.

### Metrics

Three OTel metric instruments, all exported via the same collector:

| Metric | Type | Labels | Purpose |
|---|---|---|---|
| `weave.router.decision.duration_ms` | histogram | `strategy`, `chosen_model`, `fallback_reason` | The 100ms SLO is enforced against this. |
| `weave.router.decisions` | counter | `strategy`, `chosen_model`, `cluster_id` | Decision distribution; how traffic flows through the cluster space. |
| `weave.router.embed.duration_ms` | histogram | `embedder_model` | Latency of the Jina v2 inference specifically. |

Plus the standard `gen_ai.client.operation.duration` and
`gen_ai.client.token.usage` from the OTel GenAI semantic conventions on
`router.upstream_call`.

### Persistent decision log — derived from OTel, not separate

The OTel collector pipeline (already deployed for Claude Code OTLP) lands
spans in long-term storage. Reuse that pipe; don't add a parallel
Postgres-write path from the router.

Concretely:

- Router emits OTel spans → OTel collector → existing pipeline lands
  spans in `claude_code_*`-shaped tables today.
- We add a new sink: `weave.router.*`-attributed spans land in a new
  Postgres table `model_router_decisions` (partitioned by ts, TTL 30
  days) and the same ClickHouse warehouse the rest of `chpipeline`
  uses.
- The Postgres table schema is **derived from the OTel attribute set**
  — same column names, same types. No schema drift between what we
  log and what we query.

This is also where the Bayesian updater reads from. The retrain Modal
job pulls from `model_router_decisions` joined to `judgments`, computes
posteriors, emits new `rankings.json`.

### Dashboards (Cloud Monitoring / Grafana)

1. **Cluster distribution** — `weave.router.decisions` grouped by `cluster_id`, daily. Drift alert at >2σ.
2. **Model decision distribution** — same, grouped by `chosen_model`. Compare against heuristic baseline (shadow-mode dashboard).
3. **Routing latency** — P50/P95/P99 of `weave.router.decision.duration_ms`. SLO line at 100ms.
4. **Fail-open rate** — `weave.router.decisions{fallback_reason!=""}` rate. Alert >0.1%.
5. **Alpha distribution** — histogram of α values across requests. Lets us see how clients are tuning routing once per-request override ships (P1).
6. **Per-installation drift** — top 20 installations' cluster/model distributions over time.
7. **Posterior health** — for each cell, current `(α, β)` and posterior mean ± std. Cells with `α + β < 10` are flagged as low-confidence.

### Alerts

- `weave.router.decision.duration_ms` P95 > 100ms for 5 minutes → page.
- Fail-open rate >0.1% for 5 minutes → page.
- Cluster distribution shifts >2σ from rolling 7-day baseline → ticket.
- Posterior drift: any cell's posterior mean changes by >0.15 from one nightly retrain to the next → ticket. (Catches provider model updates.)

### Acceptance criteria

OTel pipeline emits spans + metrics from the router; collector lands them in `model_router_decisions` (Postgres) and the ClickHouse warehouse; all five dashboards live; alerts wired in Cloud Monitoring. **No bespoke Postgres-write code in `auth.Service` for routing decisions** — the router's only persistence is via OTel.

---

## Latency SLO

**Goal:** P95 routing decision adds ≤100ms in production (router-only).
The 300ms hard ceiling is the *system* SLO, which also covers upstream
network and provider TTFB — the router shouldn't dominate that budget.

P0 targets ~20-30ms steady-state in-process (INT8 + ≤256-token cap). The
100ms P95 gives headroom for tokenizer P99 spikes and occasional GC.

- `weave.router.decision.duration_ms` is the source of truth (see OTel section).
- Cloud Run autoscaling: `min_instances >= 1` keeps the warm pool warm, but autoscale spinups still pay 3-6s cold start. Mitigation is fail-open: requests landing on a fresh instance fall through to heuristic if `Embed` exceeds a per-request 200ms `context.WithTimeout`.
- Embedder warmup: `cluster.NewEmbedder` runs `Embed(ctx, "warmup")` before `srv.ListenAndServe()` returns. Within an instance, takes the first-request hit off the critical path.
- Load test: `scripts/loadtest.sh` (vegeta or wrk) against staging at 100 / 500 / 1000 RPS for 5 minutes; assert P95 stays ≤100ms.
- Alert: P95 > 100ms for 5 minutes → page. (Also: fail-open rate > 0.1% → page; that's the signal that cold-start tail or embedder errors are happening more than expected.)

**Acceptance:** load test on staging at 1000 RPS shows P95 ≤ 100ms over a
5-min window with INT8-quantized weights and `MaxPromptChars = 1024`. Run
the same load test with the cap relaxed to 2048 chars to characterize the
penalty — informs whether we should tighten `auth/tokens.go`'s 2048-char
cap globally or only inside the scorer.

---

## Shadow / promotion gates

Every router version (`v0.1` → `v0.5` → `v1.0` → ongoing) earns its way
into the primary path through measured comparison against the previous
version. The pattern reused for every promotion:

1. **Build the new artifacts** (or new code).
2. **Wire as a `shadow.Router`** wrapping the current primary. Returns the
   primary's decision; logs both decisions to OTel as a `shadow:` strategy.
   Shadow goroutine swallows errors — never affects the primary path.
3. **Soak until enough disagreements** to power a comparison: ≥1k
   disagreements between primary and shadow, or the full probe set if
   that lands first.
4. **Offline analysis**: the eval Modal job (`models/v2/deploy/apps/router_eval.py`)
   pulls `model_router_decisions` filtered to `strategy=shadow:*`, joins to
   `judgments` (D3) and `claude_code_api_requests` (cost ground truth),
   computes net deltas across agreement/disagreement buckets.
5. **Promote** if the eval shows positive net cost savings or quality lift
   with statistical significance (≥1k disagreements, p<0.01). Else rollback;
   investigate.

**`internal/router/shadow/` package:** wraps a primary and a shadow router,
returns the primary's decision, runs the shadow's `Route()` in a goroutine,
emits both as separate OTel attributes on the same span (`weave.router.strategy = "shadow:cluster:heuristic"`).
Bounded queue with drop-oldest if the shadow falls behind.

**Promotions in this plan:**

| Promotion | Pattern | Notes |
|---|---|---|
| `v0.5` rollout | heuristic primary → v0.5 shadow → v0.5 primary | Phase 1a's eval harness is the *quality* gate; the shadow stage here is a **disagreement audit** + data-collection, not a statistical promotion gate (D3 is empty). Promote once the audit shows v0.5 doesn't disagree wildly with heuristic on cases heuristic obviously gets right. |
| `v1.0` shadow ramp | v0.5 primary → v1.0 shadow → v1.0 primary | First *statistical* promotion (≥1k disagreements, p<0.01 on judge-graded D3). |
| Ongoing nightly | current → newly-retrained → newly-retrained | Auto-promote on positive eval; auto-rollback on negative. |

---

## Retrain without redeploy

**Goal:** centroids and rankings update without a code release. New
`rankings.json` from a Modal retrain job is live in router pods within
the next deploy cycle (or sooner with hot-reload).

- Artifacts move from `//go:embed` to GCS:
  `gs://workweave-router-artifacts/<env>/cluster/<run_id>/{centroids.bin, rankings.json, model_registry.json}`
  plus a `latest.json` pointer.
- The ONNX **model itself stays embedded** (changes rarely; even 40-50 MB INT8 is enough that we'd rather not fetch it on every container start).
- Startup flow: `cluster.NewScorer(...)` reads `latest.json` → fetches the small JSON/bin artifacts (~10 KB total) → loads them. Falls open to last-known-good local copy if GCS unreachable.
- Hot-reload: optional. SIGHUP re-fetches `latest.json` and atomically swaps the in-memory artifacts. Document but defer.
- `latest.json` schema:

  ```json
  {
    "run_id": "20260501-abc123",
    "router_version": "weave-router-v1.0",
    "trained_at": "2026-05-01T12:00:00Z",
    "embedder_model": "jina-v2-base-code",
    "embed_dim": 768,
    "training_data_mix": {"d1": 0.05, "d2": 0.20, "d3": 0.75},
    "eval": {"top1_acc": 0.71, "cost_per_correct": 0.0023}
  }
  ```

- Rollback: `previous.json` pointer; manual flip via `gsutil cp gs://.../previous.json gs://.../latest.json`. Auto-rollback on negative eval covered in the shadow section above.
- Modal Secret `gcp-credentials` (already configured) gives router runtime read-only access.

---

## Modal training apps

Mirroring `models/v2/deploy/apps/` patterns the research agent surfaced.
Each Modal app is a single file; uses `CloudBucketMount` for GCS;
secrets via `modal.Secret.from_name`; deployed via `CI_DEPLOYED_APPS`
auto-deploy on merge.

```
models/v2/deploy/apps/
├── router_probe.py         # Re-runs D2 probe set on every model change + weekly cron
├── router_replay.py        # Pulls D3 sample, replays through models, captures responses
├── router_judge.py         # Runs labeling protocol on D2 + D3 outputs
├── router_train.py         # Walks D1 + D2 + D3 → Beta-posterior update → emits artifacts to GCS
└── router_eval.py          # Reads model_router_decisions for shadow/promotion analysis

models/v2/orchestrator/src/tasks/router/
├── retrain.ts              # Trigger.dev: weekly cron; orchestrates probe → replay → judge → train → eval → promote/rollback
└── add-model.ts            # Trigger.dev: ad-hoc; cold-start protocol for new model
```

**Reuse:**

- `models/v2/deploy/infra/images.py` — base image; add scikit-learn, sentence-transformers, jina-embeddings extras to pip list.
- `models/v2/deploy/infra/mounts.py` — mounts for `gs://workweave-router-probes/`, `gs://workweave-router-training/`, `gs://workweave-router-artifacts/`.
- `models/v2/deploy/infra/gcp.py` — Cloud SQL Proxy access to Postgres for D3 sampling.
- Modal Secret `gcp-credentials` — already configured.
- `CI_DEPLOYED_APPS` registry (`models/v2/shared/constants/modal.py`) — add the five `router_*` apps.

**GPU? Not needed.** The training is CPU-only (sklearn KMeans on ~10k embeddings is seconds; Beta updates are arithmetic). Embedding D3 samples re-uses the Jina v2 ONNX runtime — CPU is fine. Modal container choice: `modal.Image.debian_slim()` + uv pip install, no GPU type. Saves cost vs the GPU-heavy `models/v2/deploy/apps/inference_*.py` apps.

---

## Privacy & data retention

`router/PRIVACY.md` (new file, committed alongside the code):

- **Routing decisions** (`model_router_decisions` table): `prompt_hash` always; `prompt_text` never; `prompt_embedding` only when consent flag set OR sampled <1%. 30-day TTL in Postgres; 90-day TTL in ClickHouse. Drop entirely on workspace deletion or `prompt_retention_opt_out` flip (cascade from existing redaction pipeline).
- **D3 training extracts** (`gs://workweave-router-training/<run_id>/`): `prompt_hash` + `prompt_embedding` + judgment labels only. Raw prompt text **never** leaves the live database. 180-day TTL on the GCS bucket.
- **Judge calls**: when running the judge protocol on D3, the judge model sees the raw prompt + the candidate model responses. This is a third-party API call with the same data treatment as the routing call itself (covered by existing customer ToS for inference). Document explicitly.
- **D2 probe set**: synthetic / public benchmarks + anonymized internal samples. Reviewed and signed off by the team that curates it. No customer data.

---

## Phases

The router is not yet serving real production traffic. D3 starts empty
and builds slowly. The probe set (D2) carries the load until D3 crosses
threshold; D3 takes over once it has enough rows to materially move the
posteriors.

The principle: **ship v0.5 to prod as soon as it earns its way in**, even
though it's only D1+D2 trained. Waiting for D3 before shipping anything
is a chicken-and-egg deadlock — v0.5 produces traffic, traffic produces
D3, D3 produces v1.0.

Six phases (0, 1a, 1b, 2, 3, 4, 5), each gated on a concrete milestone
rather than calendar time. A phase ends when its exit criteria are met;
multiple phases can be in flight in parallel where the dependencies allow.

### Phase 0 — P0 working version

**Goal:** an end-to-end working cluster scorer of *some* quality, deployed
to staging behind a kill switch. Quality is intentionally not the bar
yet — Phase 1a measures it. The bar is "exists, runs, doesn't crash."

**Work:**

- Land workstreams 3a + 3b + 3c: P0 cluster scorer with all artifacts `//go:embed`'d, fail-open to heuristic.
- Build `weave-router-v0.1-bootstrap` from OpenRouterBench (Path A: re-cluster + re-aggregate scores, no new model evals). Ship to staging.
- Basic decision logging (structured `slog`, no OTel yet) so Phase 1a can read what the scorer is doing on staging traffic.

**Exit criteria:**

- Cluster scorer runs end-to-end on a docker-compose smoke against `/v1/messages` with both trivial and hard prompts; logs show different `model=` decisions per prompt class.
- Latency P95 ≤ 100ms in a 1000-RPS staging load test (confirms INT8 quantization + 1024-char cap are doing their job before we measure quality).
- v0.1 beats heuristic on OpenRouterBench's held-out split (router-internal sanity check; not the same as the eval-harness gate in Phase 1a).

### Phase 1a — Eval harness (gate before further investment)

> **Status: IN PROGRESS.** Harness scaffolding lives at
> [`router/eval/`](eval/) (Modal app + 14 BenchmarkLoaders + GPT-5/Gemini
> 2.5 Pro judge ensemble + Pareto plotter + spot-check CLI). The
> per-request override (`x-weave-disable-cluster` trusted header,
> evalswitch wrapping the cluster scorer + heuristic) is wired in
> `cmd/router/main.go`. Awaiting the full 500-prompt run + κ
> verification + maintainer's gate decision; result lands in
> [`router/docs/eval/EVAL_RESULTS.md`](../../eval/EVAL_RESULTS.md).

**Goal:** measure where our router lands vs. heuristic and vs. always-Opus
on a curated 500-prompt eval harness. This is the explicit "do this
before spending weeks more" decision: cheap signal that tells us whether
we're chasing 10% upside or 60% upside.

**Work:**

- Pick 500 prompts representative of Claude Code traffic (real prompts where consent permits, augmented from public benchmarks otherwise — not the full ~755-prompt probe set yet, just enough for a Pareto plot).
- Run each prompt through Opus, Sonnet, Haiku directly; capture full output + cost + latency.
- Judge with the cross-family ensemble (GPT-5 + Gemini 2.5 Pro, paired comparison + rubric, 5-10% human spot-check). This is the same judge protocol Phase 1b will productionize — Phase 1a is the protocol's first run.
- Plot cost-vs-quality Pareto: heuristic, always-Opus, always-Sonnet, always-Haiku, v0.1 cluster scorer.
- Also evaluate v0.1 against **RouterBench (Martian)** if the schema is mappable (free download, ~405k pre-computed inferences across overlapping models). Skip if it's not cheap; mention as future work.
- Submit v0.1 to **RouterArena** for external validation if the submission process is straightforward (~$0 expected); mention as future work otherwise.

**Exit criteria:**

- Pareto plot exists, shows where v0.1, heuristic, and always-Opus land relative to each other.
- Decision: **continue** if v0.1 shows a clear cost-or-quality win over heuristic (e.g., matches always-Opus quality at <50% Opus cost, OR meaningfully beats heuristic on judge score at equivalent cost). **Stop and rethink** otherwise — adding D2 + D3 won't save a fundamentally broken cluster geometry.
- Cohen's κ ≥ 0.6 between judge ensemble and human spot-check on ≥30 hand-validated rows. **If κ < 0.6, fix the rubric before Phase 1b.**

### Phase 1b — Probe set + judge pipeline

**Goal:** the labeling protocol is productionized, and we have a populated
per-`(cluster, model)` scoring matrix from D2 — overwriting the
OpenRouterBench-derived priors with our own judge-graded data.

**Work:**

- Expand from Phase 1a's 500-prompt eval set to the full ~755-prompt probe set across the 14 slices. Half from public benchmarks (Aider Polyglot, BFCL v4, LiveCodeBench, GPQA, MATH, BIRD-SQL, τ-bench, Chatbot Arena), half hand-written or pulled from anonymized Workweave traffic.
- Stand up `gs://workweave-router-probes/v1/probe.jsonl` + manifest.
- Productionize `router_replay.py` and `router_judge.py`: cross-family judge ensemble (GPT-5 + Gemini 2.5 Pro), the rubric, anti-bias measures, human-spot-check sampling. Phase 1a's judge run is the dev iteration; this is the productionized version.
- Run the probe set against Opus + Sonnet + Haiku. Judge. Populate the score matrix.

**Exit criteria:**

- κ ≥ 0.6 maintained on ≥50 hand-validated rows (extends Phase 1a's bar).
- Score matrix populated for all `(cluster, model)` cells the probe set covers.

### Phase 2 — Ship v0.5 to prod

**Goal:** the first content-aware production router is live; D3 starts accumulating.

**Work:**

- Train `weave-router-v0.5-probe` (D1 prior + D2 strong updates).
- Validate against the held-out probe split — should be a clear win over v0.1 since D2 is full-strength and D1 is weak prior.
- Initial shadow rollout: heuristic primary, v0.5 shadow. Purpose is data-collection (capture v0.5 decisions next to heuristic for retrospective comparison once D3 builds), **not** statistical promotion gate.
- Ramp v0.5 to primary once a small disagreement audit confirms it doesn't disagree wildly with heuristic on cases the heuristic obviously gets right (trivial prompts, etc.). Heuristic stays as fail-open fallback.
- **Land OTel instrumentation here** (moved from Phase 0): `router.handle_messages` → `router.routing_decision` → `router.embed` / `router.upstream_call`, full attribute schema, persistent decision log via the existing `chpipeline` pipe (open question — verify that wiring before depending on it).
- Land all dashboards + alerts: latency SLO, fail-open rate, cluster distribution, posterior health, alpha distribution.
- Document `router/PRIVACY.md` and `router/observability/`.

**Exit criteria:**

- v0.5 promoted to primary. The router is making content-aware routing decisions on real traffic.
- All Phase 2 dashboards and alerts wired.

### Phase 3 — D3 accumulation + D2 expansion

**Goal:** D3 reaches threshold for its first posterior update; D2 grows to cover under-sampled clusters; v0.6 ships if D2 expansion produces a clear improvement.

**Work:**

- The OTel pipeline is producing `model_router_decisions` rows continuously from Phase 2. Nothing to do here for capture.
- Stand up `router_judge.py` running async over D3 samples (1–5% uniform, 100% of high-cost / exploration-flagged). Writes `judgments` joined on `request_id`. **Don't yet feed these into posteriors** — wait for the threshold.
- Expand D2 from ~755 → ~1500 prompts. Focus expansion on slices the v0.5 scorer is uncertain about (cells with `α + β < 10`).
- Re-run probe + judge on the expanded set; train `weave-router-v0.6-probe`. Promote if it beats v0.5 on probe-set held-out accuracy.
- Build the Bayesian-posterior updater fully (`router_train.py`). The D3 update path is implemented and tested on synthetic data; just not exercised against real D3 yet.

**Exit criteria:**

- D3 crosses threshold: ≥10k rows in `model_router_decisions` AND ≥1k rows in `judgments`.
- D2 expanded; v0.6 either shipped or explicitly skipped (no improvement justified the rollout).

### Phase 4 — First D3-trained router (v1.0)

**Goal:** the router is trained on real Workweave traffic, validated via shadow with statistical power.

**Work:**

- First D3-inclusive retrain: `weave-router-v1.0` on D1 prior + D2 strong + D3 ESS-capped.
- Shadow rollout, this time as a real promotion gate: v0.5 primary, v1.0 shadow.
- Soak until ≥1k disagreements collected. With statistical power available, run `router_eval.py`: net delta on cost, quality (judge-graded), and latency.
- **Promote v1.0 if** positive net delta at p < 0.01. **Else investigate, don't promote.** A failure here points to either bad D3 labels, broken posterior math, or a regression in v1.0 that v0.5 caught more reliably.

**Exit criteria:**

- v1.0 promoted to primary, OR v1.0 explicitly rejected with a documented investigation.

### Phase 5 — Steady state

**Goal:** the retrain pipeline runs autonomously; the router improves over time.

**Work (continuous):**

- Scheduled retrain cron (`router_train.py` via Trigger.dev). Auto-promote on positive eval, auto-rollback on negative. Cadence: weekly is the default; tune from there.
- Probe set re-run on every model checkpoint update; weekly cron now that D3 carries the load and probe-set cost is justified.
- Implicit-signal calibration model trained once ≥1k labeled samples are cross-referenced with implicit signals. Calibration becomes part of the retrain pipeline once validated.
- Add model #4 via cold-start protocol when business asks for it.

**No exit criteria** — this is the long-running operating mode.

### Phase summary

| Phase | Output | Gated on |
|---|---|---|
| 0 — P0 working | v0.1 in staging, end-to-end working | latency P95 ≤ 100ms; v0.1 beats heuristic on OpenRouterBench held-out |
| **1a — Eval harness (gate)** | Cost-vs-quality Pareto plot vs heuristic / always-Opus / RouterBench-Martian if cheap | v0.1 shows clear win over heuristic; κ ≥ 0.6 |
| 1b — Probe set + judge | D2 populated, score matrix filled | κ ≥ 0.6 sustained on ≥50 rows |
| 2 — Ship v0.5 | v0.5 primary in prod, D3 accumulating, OTel pipeline live | Disagreement audit clean |
| 3 — D3 + D2 expansion | D3 ≥ threshold, possibly v0.6 shipped | ≥10k decisions + ≥1k judgments |
| 4 — First D3 retrain | v1.0 promoted (or rejected) | p < 0.01 over ≥1k disagreements |
| 5 — Steady state | Continuous retrain cron, model adds | (none — operating mode) |

Phases 0 → 1a → 1b → 2 are sequential. Phase 1a is the explicit "stop or
go" gate — if v0.1's Pareto position is no better than heuristic, stop and
rethink rather than spending weeks on D2 + D3. Phase 3 starts the moment
Phase 2 exits and runs in parallel with whatever else needs doing. Phase
4 starts the moment Phase 3's threshold is met. Phase 5 is everything
that follows.

> **OTel and the persistent decision log moved from Phase 0 to Phase 2.**
> Earlier draft pulled it earlier on the rationale "ship instrumentation
> before traffic so no data is dropped." But staging traffic is zero, the
> data is dropped anyway, and the OTel pipeline's existence isn't on the
> critical path for the Phase 1a quality gate. Land it alongside v0.5,
> when it actually catches data we'll use.

---

## Implementation cadence — PR sequence

Three PRs to put a content-aware router on every Claude Code request,
plus a fourth-and-beyond bucket for production iteration. Each PR is
independently mergeable, leaves CI green, and is revertible without
breaking what came before. The PRs are intentionally **large** — the
goal is one coherent landing per phase milestone, not many small
steps. Smaller slicing exists if needed (see "If you need finer
slices" at the bottom) but is not the default.

### PR 1 — Avengers-Pro working in staging (all of Phase 0)

Everything needed to make the scorer real, end-to-end. After this PR
merges, the architecture is live in staging behind a kill switch.

- Python: `download_bench.sh`, `inspect_bench.py`, `export_jina_onnx.py` (with INT8 quantization), `sweep_cluster_k.py`, `train_cluster_router.py`, `dump_cluster_test_vector.py`. Run them once, commit the artifacts.
- Committed artifacts: `centroids.bin`, `rankings.json`, `model_registry.json`, `assets/model.onnx` (Git LFS), `assets/tokenizer.json`. K choice baked in based on the sweep result.
- Go: full `internal/router/cluster/` package — embedder, scorer, artifacts loader, unit tests, build-tag-gated ONNX integration test.
- Dockerfile: Alpine → `gcr.io/distroless/cc-debian12`, `CGO_ENABLED=0` → `1`, libonnxruntime copied in.
- `cmd/router/main.go` wired: scorer with heuristic as fail-open fallback, embedder warmup before listen, 200ms `context.WithTimeout` around `Embed`, kill switch `ROUTER_DISABLE_CLUSTER=true`.
- Deploy to staging only. Prod stays on heuristic.

**Test plan (must pass before merge):**

- `go test ./internal/router/cluster/...` green.
- ONNX integration test (`go test -tags onnx_integration`) cosine ≥ 0.99 vs Python reference vectors.
- `docker build` + image scan clean.
- Staging docker-compose smoke: trivial prompt → Haiku, hard prompt → Opus, logs differ.
- Staging load test at 1000 RPS for 5 min → P95 ≤ 100ms.
- Chaos test: delete `model.onnx` in the container, confirm fail-open to heuristic without crash.
- Kill switch flip works without redeploy.

**Why this is one PR:** anything smaller doesn't actually exercise the
architecture end-to-end. A "Python pipeline only" or "Go package only"
PR is dead code that proves nothing about whether the system works. The
fail-open + kill switch is what makes it safe to merge as a single unit.

### PR 2 — Eval harness + go/no-go decision (Phase 1a)

The only PR in the project where the test result decides whether the
project continues.

- `router/scripts/eval_harness/` — 500 prompts representative of Claude Code traffic, judge ensemble (GPT-5 + Gemini 2.5 Pro, paired comparison + rubric, 5-10% human spot-check), Pareto plotter.
- `router/docs/eval/EVAL_RESULTS.md` committed with the actual numbers: cost-vs-quality Pareto plot, table comparing v0.1 / heuristic / always-Opus / always-Sonnet / always-Haiku, κ score, decision.
- RouterBench-Martian schema-mapped run + RouterArena submission if both are cheap; skip and mention as future otherwise.

**Test plan:** harness runs end-to-end; `EVAL_RESULTS.md` is the artifact.
**The gate is a human decision** based on what the plot shows.

**Merge criteria:**

- v0.1 shows a clear cost-or-quality win over heuristic (matches always-Opus quality at <50% Opus cost, OR meaningfully beats heuristic on judge score at equivalent cost).
- Cohen's κ ≥ 0.6 between judge ensemble and human spot-check on ≥30 hand-validated rows.

**If the gate fails:** revert PR 1's wiring (kill switch flip is enough,
no code revert needed), close PR 2 with the EVAL_RESULTS.md committed as
the documented "stop" decision, and rethink. Adding D2 + D3 won't fix a
fundamentally broken cluster geometry.

### PR 3 — Ship v0.5 to prod (Phases 1b + 2 combined)

The biggest PR by surface area, but each piece is bounded by the shadow
- fail-open machinery. Primary path is heuristic until the disagreement
audit clears.

- Probe set curated and stored at `gs://workweave-router-probes/v1/probe.jsonl` (~755 prompts across the 14 slices).
- Modal apps: `router_replay.py`, `router_judge.py`, `router_train.py`. Productionized rubric and anti-bias measures.
- v0.5 trained with D1 prior + D2 strong updates. Beats v0.1 on probe held-out.
- OTel instrumentation: `router/internal/observability/otel.go`, full attribute schema. Collector wiring to `chpipeline` — verifies the open question with a single end-to-end span before ramping.
- `internal/router/shadow/` package — wraps primary + shadow, emits both decisions to OTel, bounded queue.
- Dashboards (cluster distribution, model decision distribution, routing latency, fail-open rate, alpha distribution, per-installation drift, posterior health) + alerts (P95 > 100ms, fail-open >0.1%, cluster drift >2σ).
- Cloud Run ramp: heuristic primary → v0.5 shadow → disagreement audit on first ~1k prod requests → promote v0.5 to primary.
- `router/PRIVACY.md` documented.

**Test plan:**

- v0.5 beats v0.1 on the held-out probe split.
- A v0.5 routing decision in staging shows up as a row in `model_router_decisions` (proves the OTel → `chpipeline` wiring).
- Shadow audit shows v0.5 doesn't disagree wildly with heuristic on cases heuristic obviously gets right (trivial prompts route to Haiku in both, etc.).
- Latency P95 stays ≤100ms in prod under real traffic for 24 hours pre-promotion.
- Fail-open rate < 0.1% across the audit window.

After this PR merges, the router is making content-aware routing
decisions on every Claude Code request.

### PR 4+ — Production iteration (Phases 3–5, as they materialize)

These land as smaller PRs because the shadow + auto-rollback machinery
from PR 3 bounds the risk — each retrain is one PR, each new model is
one PR.

- D3 judge cron (Modal app + Trigger.dev orchestration).
- Bayesian posterior updater (`router_train.py` extension).
- First D3-trained v1.0 retrain, shadow against v0.5, statistical promotion gate (≥1k disagreements, p<0.01).
- D2 expansion to ~1500 prompts as cells with `α + β < 10` accumulate.
- Cold-start protocol PR when model #4 is needed.
- Implicit-signal calibration once ≥1k labeled samples cross-reference with implicit signals.

### If you need finer slices

The above is the default cadence. PR 1 specifically can be split if the
review burden is too high — natural seams are (a) offline Python pipeline
- committed artifacts, (b) Dockerfile migration + onnx-smoke binary, (c)
Go cluster package without wiring, (d) `main.go` wiring with kill
switch. The cost is harder reverts and four staging deploys instead of
one. Combine 3+4 specifically only if you're confident the package and
the wiring are reviewable in one read — that's the seam where live-traffic
risk lives.

---

## Open questions

1. **K choice with N=3 models.** Paper used K=60 with N=8. With our 3-model subset, the cell-collapse risk is real (many clusters with identical "Opus > Sonnet > Haiku" rankings). `sweep_cluster_k.py` (Phase 0) measures this on OpenRouterBench. Pick the smallest K with ≥80% distinct top-1 cells. Likely K∈[10, 40], not K=60.

2. **`ai_user_prompts` table existence.** An earlier research agent claimed `ai_user_prompts.prompt_content` already captures Claude Code prompt text, gated by `RedactPromptContentProcessor`. The processor exists at `backend/internal/app/tasks/redact_prompt_content.go`, but no migration named `ai_user_prompts` is visible. **Verify the actual table name (Postgres or ClickHouse) before D3 work depends on it.** Possibly stored under a different name in `chpipeline`-managed tables.

3. **OTel collector → `chpipeline` wiring.** Plan assumes router-emitted spans can land in the existing pipe `chpipeline` uses for Claude Code OTLP (collector at `backend/internal/app/api/ingestapi/unified.go`). The endpoint exists; the wiring from router → that endpoint is **not yet verified**. Before Phase 2 starts, prove this end-to-end with a single span. **Recommendation:** same pipe, same downstream — minimum new infra. Confirm with whoever owns the ingest pipeline.

4. **Embedder accuracy delta vs paper.** Paper uses Qwen3-embedding-8B (4096-dim); we use Jina v2 base code (768-dim, INT8). Plan-invented swap; the routing-quality cost is unknown until the eval harness measures it. **If Phase 1a shows v0.1 lands clearly worse than always-Opus *because of* embedder choice, options are: (a) use a stronger CPU embedder like e5-large-v2 (1024-dim, ~115MB INT8) at higher latency cost, (b) fall back to Modal sidecar with Qwen3 (paper-faithful but adds 50-100ms RTT), (c) accept the gap if it's small.** Phase 1a tells us which.

5. **Probe-set source access.** Aider Polyglot, BFCL v4, LiveCodeBench, GPQA-Diamond, MATH, BIRD-SQL, τ-bench, Chatbot Arena samples — most are public on HF or GitHub but a few may need licensing review. Worth confirming before Phase 1b.

6. **Judge model for the labeling protocol.** Cross-family ensemble means we need *non-Anthropic* judges since Anthropic models are the routing pool. Candidates: GPT-5 (cross-family, strong), Gemini 2.5 Pro (cross-family, strong, cheap), or DeepSeek-R1 (open weights, cheapest). Plan recommends GPT-5 + Gemini 2.5 Pro ensemble for D2 (highest signal); switch to Gemini-only for D3 to control cost (~$200/week at 5k judged samples).

7. **Posterior storage format.** The retrain Modal job reads ~10k–100k rows of `model_router_decisions` and writes ~K × M Beta-posterior cells (40 × 3 ≈ 120 today, 40 × 10 = 400 at full scope). Trivial size. Open: full `(α, β)` history (lets us replay updates) or only current state? **Recommendation:** current state in `rankings.json`, full history in a separate `posterior_history` ClickHouse table for offline analysis.

8. **Legal/contract review** before D3 judge runs against real customer prompts. Plausibly covered under existing inference ToS but worth explicit confirmation.

9. **Traffic ramp strategy.** With v0.5 as the first prod router, do we ramp traffic gradually (1% → 10% → 100% across a Cloud Run revision sequence) or flip directly? Direct is fine if there's a quick rollback path; gradual is safer but slows D3 accumulation. **Recommendation:** direct flip with the heuristic as the documented one-flag-flip rollback. Bias toward speed because D3 needs traffic.

10. **Vuln-scanner impact of Alpine→distroless/cc switch.** The current Dockerfile produces a static Go binary on Alpine; the switch to dynamically-linked CGO + glibc on distroless/cc-debian12 changes the image profile. Confirm with whoever owns image scanning that this is acceptable before P0 lands.

---

## Critical files & timeline summary

**Phase 0 — P0 working version (one engineer):**

- New: `router/internal/router/cluster/` (full package), `router/scripts/{download_bench.sh,inspect_bench.py,export_jina_onnx.py,sweep_cluster_k.py,train_cluster_router.py,dump_cluster_test_vector.py}`, `router/.gitattributes`
- Modified: `router/cmd/router/main.go` (drop heuristic-only branch, wire scorer with heuristic as fail-open fallback), `router/Dockerfile` (Alpine→distroless/cc-debian12, CGO_ENABLED=0→1), `router/go.mod` (hugot + onnxruntime_go deps), `router/README.md` (`git lfs install` requirement, INT8 export note), `router/CLAUDE.md`+`AGENTS.md`

**Phase 1a — eval harness:**

- New: `router/scripts/eval_harness/` (the 500-prompt eval set, judge driver, Pareto plotter). One-shot script set; doesn't need to be productionized as a Modal app yet — that comes in Phase 1b.
- New: `router/docs/eval/EVAL_RESULTS.md` (committed Pareto plot + table; numbers from the run that gates Phase 1b).

**Production readiness (Phases 1a → 5, sequenced as above):**

- **Observability**: OTel SDK in `router/internal/observability/otel.go`; OTel collector config (whatever's already deployed for Claude Code OTLP, extended); migration `0002_model_router_decisions.up.sql` (downstream of OTel collector, not written-to from router code); `chpipeline` extension for the new table; dashboard JSON + alert configs under `router/observability/`.
- **Probe set**: `gs://workweave-router-probes/v1/probe.jsonl` + manifest; `models/v2/deploy/apps/router_probe.py`; `models/v2/orchestrator/src/tasks/router/refresh-probe.ts`.
- **Judge & training**: `models/v2/deploy/apps/router_replay.py`, `router_judge.py`, `router_train.py`, `router_eval.py`; `models/v2/orchestrator/src/tasks/router/retrain.ts` and `add-model.ts`.
- **Shadow + retrain-without-redeploy**: `router/internal/router/shadow/` package; `router/internal/router/cluster/gcs.go` (artifact fetch); modify `router/cmd/router/main.go` to wire shadow + GCS fetch.
- **Privacy**: `router/PRIVACY.md`.

---

## What's explicitly NOT in this plan

- **Multi-provider routing.** Beyond Anthropic requires `internal/providers/openai/`, `internal/providers/google/`, etc. Defer until cross-provider routing is on the roadmap.
- **Per-org full routing customization** beyond SLA knobs (e.g. "always Haiku for org X"). Not v1; revisit after shadow data shows real customer-level patterns.
- **Streaming response routing.** Decision happens before the upstream call; streaming is orthogonal.
- **Speculative dispatch / hedging.** Latency optimization, not routing accuracy. README roadmap.
- **Human-labeled dataset from scratch.** Cross-family LLM-as-judge + reference-based scoring + 5–10% human spot-check is good enough and 100x cheaper. Confirmed by the literature.
- **Replicating OpenRouterBench's full benchmark suite.** Their 6 benchmarks aren't all equally relevant to Claude Code — we use them as priors at P0 but D2 + D3 supersede them. No need to replicate the paper's full eval setup.
