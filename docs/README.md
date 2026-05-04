Created: 2026-05-03
Last edited: 2026-05-03

# Router docs — table of contents

> Index of every Markdown doc under `router/docs/`, ordered by creation
> date (oldest first). When adding a new doc, append a row to this file
> as part of the same change — see `router/CLAUDE.md` (and the mirror
> `router/AGENTS.md`) under "Adding a doc under `docs/`".

## Active docs

| Created | Doc | What it covers |
|---|---|---|
| 2026-05-01 | [`testing/TESTING.md`](testing/TESTING.md) | End-to-end runbook for verifying the router OTel pipeline through the Weave backend stack (Router → OTLP/HTTP → Pub/Sub → Worker → Postgres). |
| 2026-05-02 | [`architecture/ARCHITECTURE.md`](architecture/ARCHITECTURE.md) | Reference for runtime layering, package responsibilities, and the request lifecycle. The "what does this codebase look like" entry point. |
| 2026-05-02 | [`plans/ROUTER_V1_PLAN.md`](plans/ROUTER_V1_PLAN.md) | Active 10-week v1 plan: telemetry foundation, cache-aware overlay, TTL choice, optional speculative escalation, and per-cluster α tuning. Phase 4 landed early as v0.6 on 2026-05-02; Phases 1–3 unstarted. |
| 2026-05-02 | [`eval/EVAL_RESULTS.md`](eval/EVAL_RESULTS.md) | Latest judge-ensemble eval run. The quality bar nothing else may regress. Currently records `CONTINUE` for `run-f687cd8cae` (v0.6-cluster Pareto-dominates v0.5 and always-Opus). |
| 2026-05-03 | [`plans/CCR_ANALYSIS.md`](plans/CCR_ANALYSIS.md) | Single source of truth for what to borrow from / avoid in `musistudio/claude-code-router` (CCR), and how to stage the work without breaking the cluster-router architecture. Consolidates two archived CCR docs. |
| 2026-05-03 | [`plans/ROUTER_IMPROVEMENTS.md`](plans/ROUTER_IMPROVEMENTS.md) | Menu of upgrades drawn from the RouterArena leaderboard analysis: free wins (log-cost normalization, `max_tokens` injection, robustness CI, semantic cache, …), three V2 candidates (R2-Router length-conditioning, RouterDC contrastive fine-tune, vLLM-SR ModernBERT classifier), and a do-not-adopt list. |
| 2026-05-03 | [`plans/ROUTER_SPEED.md`](plans/ROUTER_SPEED.md) | Plan to extend the cluster scorer from 2-axis (quality, cost) to 3-axis (quality, cost, latency). Motivated by Cerebras being added as a provider on `steven/router-cerebras-provider`. |
| 2026-05-03 | [`plans/AGENTIC_CODING.md`](plans/AGENTIC_CODING.md) | Agentic-coding-specific upgrade menu for Claude Code traffic on Opus 4.7 / Sonnet 4.5 / Haiku 4.5: session-sticky role-conditioned routing, effective-cached-input cost in α-blend, turn-type detection, fleet-specific trajectory labels, and a layered eval suite (Aider Polyglot / SWE-Bench Verified / Terminal-Bench / τ²-bench). Complements the generalist `ROUTER_IMPROVEMENTS.md`. |
| 2026-05-03 | [`plans/ROUTER_V0_7_CALIBRATION_FIX.md`](plans/ROUTER_V0_7_CALIBRATION_FIX.md) | Tonight's execution plan for v0.7 → v0.10 retraining: drop per-prompt minmax score normalization (A), drop Gemini proxy entries (B), add direct RouterArena per-model labels (C), and switch to log cost normalization (D). Triggered by the discovery that v0.6's published RouterArena numbers were measuring the heuristic fallback after a `-tags ORT` Makefile gap silently broke the cluster scorer. |
| 2026-05-03 | [`plans/ROUTER_V0_7_CALIBRATION_PROGRESS.md`](plans/ROUTER_V0_7_CALIBRATION_PROGRESS.md) | Decision log paired with `ROUTER_V0_7_CALIBRATION_FIX.md`. Updated each time we make a non-trivial choice during execution (registry edits, score normalization mode, what to skip/sequence) so the rationale and findings persist across sessions. |

## Archived docs

Superseded planning material kept for historical context. See also
[`plans/archive/README.md`](plans/archive/README.md) for the
archived-doc → active-replacement map.

| Created | Doc | Why archived |
|---|---|---|
| 2026-04-30 | [`plans/archive/CLUSTER_ROUTING_PLAN.md`](plans/archive/CLUSTER_ROUTING_PLAN.md) | Original cluster-routing baseline plan. Load-bearing parts consolidated into `plans/ROUTER_V1_PLAN.md` §2.1 and `architecture/ARCHITECTURE.md`. |
| 2026-05-02 | [`plans/archive/FUTURE_RESEARCH.md`](plans/archive/FUTURE_RESEARCH.md) | Speculative research direction (cache-aware switching, per-cluster α, speculative drafting). Superseded by the execution-focused `plans/ROUTER_V1_PLAN.md`. |
| 2026-05-02 | [`plans/archive/CCR_COMPARISON.md`](plans/archive/CCR_COMPARISON.md) | Side-by-side comparison of `router/` vs CCR. Merged into `plans/CCR_ANALYSIS.md`. |
| 2026-05-02 | [`plans/archive/CCR_BORROW_FOR_PRICEPERF.md`](plans/archive/CCR_BORROW_FOR_PRICEPERF.md) | Ranked list of CCR ideas to borrow for price/performance. Merged into `plans/CCR_ANALYSIS.md`. |
| 2026-05-03 | [`plans/archive/README.md`](plans/archive/README.md) | Archive-only index mapping each archived doc to its active replacement. Read this when you find a stale link to an archived path. |
