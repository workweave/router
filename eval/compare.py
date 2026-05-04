"""compare.py — laptop equivalent of modal_app.py.

1:1 local mirror of `modal run modal_app.py`. Same flow: compose
prompts → fan-out inference → fan-out judging → aggregate → Pareto +
table. Same row schemas. Same output layout. Just runs locally:
asyncio for concurrency, local FS for results, no Modal, no GCS.

Defaults match modal_app exactly:

  --smoke (default):
      10 prompts (SLICES[:3] × n=4, capped) × 3 routers
      ([always-opus, heuristic, v0.2-cluster]) × gpt5 judge.
      Same composition modal smoke uses.

  --full:
      500 prompts (full SLICES) × 6 routers × [gpt5, gemini].
      Mirrors `modal run modal_app.py` without --smoke. Costs ~$250–600.

Required env (set in router/eval/.env or export):
  ROUTER_BASE_URL     — e.g. http://localhost:8082
  ROUTER_EVAL_API_KEY — seeded with `wv mr seed-key --eval-allowlist`
  ANTHROPIC_API_KEY   — for always-{opus,sonnet,haiku} routers
  OPENAI_API_KEY      — only when --judges includes gpt5
  GOOGLE_API_KEY      — only when --judges includes gemini

Results land in ./results/<run-id>/ (override with --out-dir).
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import sys
import time
import uuid
from collections import defaultdict
from pathlib import Path
from typing import Any

# Bootstrap so `from eval.X` resolves when run as a script. Mirrors
# modal_app.py and tests/conftest.py.
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from eval import _env  # noqa: F401  side-effect: load router/eval/.env
from eval.aggregation import aggregate_to_router_results
from eval.pareto import render_plot, render_table, to_points
from eval.pricing import estimate_cost
from eval.routing import route
from eval.types import (
    ALWAYS_X_ROUTERS,
    STAGING_ROUTERS,
    BenchmarkPrompt,
    InferenceRow,
    JudgeName,
    JudgmentRow,
    RouterName,
    validate_router_name,
)


# ---------------------------------------------------------------------------
# Defaults — kept in lock-step with modal_app.py's local entrypoint.
# Update both files together so smoke/full produce identical run
# compositions on Modal and on the laptop.
# ---------------------------------------------------------------------------

DEFAULT_SMOKE_ROUTERS: list[RouterName] = [
    "always-opus",
    "heuristic",
    "v0.2-cluster",
]
DEFAULT_SMOKE_JUDGES: list[JudgeName] = ["gpt5"]
SMOKE_PROMPT_CAP = 10  # `modal_app.py` caps at 10; mirror that.

DEFAULT_FULL_ROUTERS: list[RouterName] = [
    "always-opus",
    "always-sonnet",
    "always-haiku",
    "heuristic",
    "v0.2-cluster",
    "v0.2-cluster-last-user",
]
DEFAULT_FULL_JUDGES: list[JudgeName] = ["gpt5", "gemini"]

# Closed always-X set. Cluster routers are accepted by shape via
# eval.types.is_cluster_router so adding a new artifact version (e.g.
# v0.3) doesn't require touching this file — pass --routers
# v0.3-cluster directly.
EXPLICIT_ROUTERS: set[str] = set(DEFAULT_FULL_ROUTERS) | ALWAYS_X_ROUTERS | {
    # Legacy cluster baselines, kept enumerated for `--help`-style
    # discoverability. Any other vN.M-cluster name also works at runtime.
    "v0.1-cluster",
    "v0.1-cluster-last-user",
    "v0.2-cluster",
    "v0.2-cluster-last-user",
}
ALL_JUDGES: set[str] = {"gpt5", "gemini"}

INFERENCE_CONCURRENCY = 16
JUDGE_CONCURRENCY = 8


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def _parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--full", action="store_true",
                   help="Run the full 500-prompt slice composition (mirrors `modal run modal_app.py` without --smoke). Default is the 10-prompt smoke matching modal smoke.")
    p.add_argument("--num-prompts", type=int, default=None,
                   help="Run a proportionally-sampled N-prompt draw across the full slice composition. Mutually exclusive with --full; preserves the slice mix.")
    p.add_argument("--slices", default=None,
                   help=("Comma-separated subset of slice names from "
                         "eval.slice_plan.SLICES (e.g. 'coding-python,coding-ts,"
                         "coding-go,coding-rust-cpp-java,coding-sql,coding-humaneval-mbpp' "
                         "for coding-only). Each slice loads its full SLICES count "
                         "unless --num-prompts also given (which proportionally scales)."))
    p.add_argument("--routers", default=None,
                   help=f"Comma-separated router subset. Default: smoke→{','.join(DEFAULT_SMOKE_ROUTERS)}, full→{','.join(DEFAULT_FULL_ROUTERS)}.")
    p.add_argument("--judges", default=None,
                   help=f"Comma-separated judges. Default: smoke→{','.join(DEFAULT_SMOKE_JUDGES)}, full→{','.join(DEFAULT_FULL_JUDGES)}. Pass --judges '' to skip judging entirely.")
    p.add_argument("--baseline", default="always-opus",
                   help="Baseline router for pairwise judging. Default always-opus.")
    p.add_argument("--out-dir", default=None,
                   help="Where to write inference / judgment / aggregated outputs. Default ./results/<run-id>/.")
    p.add_argument("--run-id", default=None,
                   help="Resume an existing run by id. Default: fresh run-<uuid>.")
    p.add_argument("--force", action="store_true",
                   help="Overwrite existing per-row JSONL files. Default skips already-written rows for cheap resume.")
    return p.parse_args()


def _resolve_routers(args: argparse.Namespace) -> list[RouterName]:
    if args.routers:
        names = [s.strip() for s in args.routers.split(",") if s.strip()]
        unknown: list[str] = []
        for n in names:
            try:
                validate_router_name(n)
            except ValueError:
                unknown.append(n)
        if unknown:
            allowed = sorted(EXPLICIT_ROUTERS) + ["vX.Y-cluster (any committed artifact version)"]
            sys.exit(f"unknown routers: {unknown}; allowed: {allowed}")
        return names  # type: ignore[return-value]
    return DEFAULT_FULL_ROUTERS if args.full else DEFAULT_SMOKE_ROUTERS


def _resolve_judges(args: argparse.Namespace) -> list[JudgeName]:
    if args.judges is None:
        return DEFAULT_FULL_JUDGES if args.full else DEFAULT_SMOKE_JUDGES
    if not args.judges:
        return []
    names = [s.strip() for s in args.judges.split(",") if s.strip()]
    unknown = [n for n in names if n not in ALL_JUDGES]
    if unknown:
        sys.exit(f"unknown judges: {unknown}; allowed: {sorted(ALL_JUDGES)}")
    return names  # type: ignore[return-value]


def _check_env(routers: list[RouterName], judges: list[JudgeName]) -> None:
    missing: list[str] = []
    # Anthropic always-X routers
    if any(r in ("always-opus", "always-sonnet", "always-haiku") for r in routers):
        if not os.environ.get("ANTHROPIC_API_KEY"):
            missing.append("ANTHROPIC_API_KEY")
    # OpenAI always-X routers (frontier IDs reachable via api.openai.com).
    if any(r in ("always-gpt55", "always-gpt55-mini", "always-gpt-4.1") for r in routers):
        if not os.environ.get("OPENAI_API_KEY"):
            missing.append("OPENAI_API_KEY")
    # Google always-X routers (Gemini OpenAI-compat endpoint).
    if any(r in ("always-gemini3-pro", "always-gemini3-flash", "always-gemini3-flash-lite") for r in routers):
        if not os.environ.get("GOOGLE_API_KEY"):
            missing.append("GOOGLE_API_KEY")
    if any(r in STAGING_ROUTERS for r in routers):
        if not os.environ.get("ROUTER_BASE_URL"):
            missing.append("ROUTER_BASE_URL")
        if not os.environ.get("ROUTER_EVAL_API_KEY"):
            missing.append("ROUTER_EVAL_API_KEY")
    if "gpt5" in judges and not os.environ.get("OPENAI_API_KEY"):
        missing.append("OPENAI_API_KEY")
    if "gemini" in judges and not os.environ.get("GOOGLE_API_KEY"):
        missing.append("GOOGLE_API_KEY")
    # Dedupe — OPENAI_API_KEY can land via both router and judge paths.
    seen: set[str] = set()
    deduped = [m for m in missing if not (m in seen or seen.add(m))]
    if deduped:
        sys.exit(f"missing env: {', '.join(deduped)} — set them in router/eval/.env or export them.")


# ---------------------------------------------------------------------------
# Prompt loading
# ---------------------------------------------------------------------------


def _load_prompts(
    full: bool,
    num_prompts: int | None = None,
    slices: list[str] | None = None,
) -> list[BenchmarkPrompt]:
    """Load prompts via SLICES — the same path modal_app.py's local
    entrypoint takes. Smoke mirrors modal smoke exactly: walk the
    first 3 SLICES, take 4 prompts each, cap at 10. Full walks every
    slice with its full count. num_prompts proportionally scales the
    full composition to N prompts total, preserving the slice mix.

    If `slices` is given, restrict SLICES to that named subset before
    any of the above paths run. With slices + neither --full nor
    --num-prompts, each named slice loads its full count (mirrors
    --full but scoped). With slices + --num-prompts, the proportional
    sampler scales relative to the **subset's** total, not 500, so
    `--num-prompts 60 --slices coding-python,coding-go` actually
    yields ~60 prompts split across the two named slices.
    """
    from eval.benchmarks import get
    from eval.slice_plan import EXTRA_SLICES, SLICES, TOTAL_PROMPTS

    active = SLICES
    active_total = TOTAL_PROMPTS
    if slices:
        # SLICES carries the locked Phase 1a composition; EXTRA_SLICES
        # carries opt-in follow-ups (e.g. swebench-verified). Both are
        # addressable by name; --slices is the only way to reach
        # EXTRA_SLICES, so the locked gate numbers stay isolated.
        by_name = {s.slice: s for s in (*SLICES, *EXTRA_SLICES)}
        unknown = [n for n in slices if n not in by_name]
        if unknown:
            sys.exit(
                f"unknown slices: {unknown}; allowed: {sorted(by_name)}"
            )
        # dict.fromkeys preserves first-seen order; dedupe so a caller
        # passing --slices a,b,a doesn't double-count slice ``a``.
        deduped_slices = list(dict.fromkeys(slices))
        active = [by_name[n] for n in deduped_slices]
        active_total = sum(s.count for s in active)

    if num_prompts is not None:
        # Scale each slice's count by num_prompts / active_total, rounding
        # up so rare slices still get at least one row, then trim to
        # exactly num_prompts at the end. Loading errors are skipped
        # (some benchmarks gate behind credentials).
        out: list[BenchmarkPrompt] = []
        for spec in active:
            scaled = max(1, round(spec.count * num_prompts / active_total))
            loader = get(spec.loader)
            try:
                out.extend(loader.load(n=scaled, seed=42))
            except Exception as e:
                print(f"  WARN: slice {spec.slice} failed to load: {e}", file=sys.stderr)
        return out[:num_prompts]

    if slices or full:
        out = []
        for spec in active:
            loader = get(spec.loader)
            try:
                out.extend(loader.load(n=spec.count, seed=42))
            except Exception as e:
                print(f"  WARN: slice {spec.slice} failed to load: {e}", file=sys.stderr)
        return out

    out = []
    for spec in active[:3]:
        loader = get(spec.loader)
        try:
            out.extend(loader.load(n=4, seed=42))
        except Exception as e:
            print(f"  WARN smoke skipping slice {spec.slice}: {e}", file=sys.stderr)
        if len(out) >= SMOKE_PROMPT_CAP:
            break
    return out[:SMOKE_PROMPT_CAP]


# ---------------------------------------------------------------------------
# Local FS helpers (parallel to modal_app's GCS helpers)
# ---------------------------------------------------------------------------


def _write_json(path: Path, obj: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(obj, indent=2) + "\n")


def _write_jsonl_row(path: Path, row: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(row) + "\n")


def _read_jsonl_dir(directory: Path, cls: Any) -> list[Any]:
    if not directory.exists():
        return []
    out: list[Any] = []
    for f in sorted(directory.glob("*.jsonl")):
        for line in f.read_text().splitlines():
            line = line.strip()
            if not line:
                continue
            out.append(cls.model_validate_json(line))
    return out


# ---------------------------------------------------------------------------
# Inference
# ---------------------------------------------------------------------------


async def _run_inference(
    *,
    sem: asyncio.Semaphore,
    out_dir: Path,
    run_id: str,
    prompt: BenchmarkPrompt,
    router: RouterName,
    force: bool,
) -> tuple[str, RouterName, str | None]:
    target = out_dir / "inference" / f"{prompt.prompt_id}__{router}.jsonl"
    if not force and target.exists():
        return prompt.prompt_id, router, None
    # routing.py catches httpx + asyncio.TimeoutError and returns an
    # error-tagged result, but anything else (e.g. malformed response,
    # bug in a router branch) would propagate up through asyncio.gather
    # and abort the whole run. Persist unexpected exceptions as an
    # error row so one bad prompt × router pair can't take the run down.
    try:
        async with sem:
            res = await route(router=router, prompt=prompt.prompt_text)
    except Exception as e:
        row = InferenceRow(
            run_id=run_id,
            prompt_id=prompt.prompt_id,
            router=router,
            model_used="",
            output_text="",
            input_tokens=0,
            output_tokens=0,
            latency_ms=0,
            cost_usd=0.0,
            error=f"{type(e).__name__}: {e}",
        )
        _write_jsonl_row(target, row.model_dump())
        return prompt.prompt_id, router, row.error
    cost = estimate_cost(res.model_used, res.input_tokens, res.output_tokens) if res.model_used else 0.0
    row = InferenceRow(
        run_id=run_id,
        prompt_id=prompt.prompt_id,
        router=router,
        model_used=res.model_used,
        output_text=res.output_text,
        input_tokens=res.input_tokens,
        output_tokens=res.output_tokens,
        latency_ms=res.latency_ms,
        cost_usd=cost,
        error=res.error,
    )
    _write_jsonl_row(target, row.model_dump())
    return prompt.prompt_id, router, res.error


# ---------------------------------------------------------------------------
# Judging
# ---------------------------------------------------------------------------


def _build_judge(name: JudgeName):
    if name == "gpt5":
        from eval.judges.gpt5 import GPT5Judge
        return GPT5Judge()
    if name == "gemini":
        from eval.judges.gemini import GeminiJudge
        return GeminiJudge()
    raise ValueError(f"unknown judge: {name}")


async def _run_judge(
    *,
    sem: asyncio.Semaphore,
    out_dir: Path,
    run_id: str,
    prompt: BenchmarkPrompt,
    judge_name: JudgeName,
    candidate: RouterName,
    candidate_text: str,
    baseline: RouterName,
    baseline_text: str,
    force: bool,
) -> tuple[str, JudgeName, RouterName, str | None]:
    target = out_dir / "judgments" / f"{prompt.prompt_id}__{judge_name}__{candidate}.jsonl"
    if not force and target.exists():
        return prompt.prompt_id, judge_name, candidate, None
    from eval.judges.ensemble import judge_pair_ensemble

    async with sem:
        try:
            result = await judge_pair_ensemble(
                judges=[_build_judge(judge_name)],
                prompt=prompt.prompt_text,
                baseline_text=baseline_text,
                candidate_text=candidate_text,
                prompt_id=prompt.prompt_id,
                run_id=run_id,
                candidate_router=candidate,
                baseline_router=baseline,
            )
        except Exception as e:
            return prompt.prompt_id, judge_name, candidate, str(e)
    _write_jsonl_row(target, result.judgments[0].model_dump())
    return prompt.prompt_id, judge_name, candidate, None


# ---------------------------------------------------------------------------
# Routing-decision matrix
# ---------------------------------------------------------------------------


def _print_routing_matrix(inference_rows: list[InferenceRow], routers: list[RouterName]) -> None:
    """Side-by-side of model picks per (prompt, router). Useful before
    judging finishes (or when --judges '' skips judging entirely) to
    eyeball whether routers are differentiating at all."""
    by_prompt: dict[str, dict[str, InferenceRow]] = defaultdict(dict)
    for r in inference_rows:
        by_prompt[r.prompt_id][r.router] = r

    print()
    print("Routing-decision matrix (model picks per prompt × router)")
    print("-" * 60)
    label_w = max((len(pid) for pid in by_prompt), default=10)
    col_w = max((len(r) for r in routers), default=12)
    header = "  " + f"{'prompt':<{label_w}}  " + "  ".join(f"{r:<{col_w}}" for r in routers)
    print(header)
    print("  " + "-" * (len(header) - 2))
    for pid in sorted(by_prompt):
        cells = []
        for r in routers:
            row = by_prompt[pid].get(r)
            if row is None:
                cells.append(f"{'-':<{col_w}}")
            elif row.error:
                cells.append(f"{'ERR':<{col_w}}")
            else:
                cells.append(f"{row.model_used:<{col_w}}")
        print(f"  {pid:<{label_w}}  " + "  ".join(cells))


# ---------------------------------------------------------------------------
# Orchestration
# ---------------------------------------------------------------------------


async def _orchestrate(args: argparse.Namespace) -> int:
    if args.num_prompts is not None and args.full:
        sys.exit("--num-prompts and --full are mutually exclusive.")
    if args.num_prompts is not None and args.num_prompts <= 0:
        sys.exit("--num-prompts must be positive.")
    slice_names: list[str] | None = None
    if args.slices:
        slice_names = [s.strip() for s in args.slices.split(",") if s.strip()]
        if not slice_names:
            sys.exit("--slices was empty.")
    routers = _resolve_routers(args)
    judges = _resolve_judges(args)
    _check_env(routers, judges)

    run_id = args.run_id or f"run-{uuid.uuid4().hex[:10]}"
    out_dir = Path(args.out_dir or f"./results/{run_id}").resolve()
    out_dir.mkdir(parents=True, exist_ok=True)

    print(f"[{run_id}] routers={routers}  judges={judges or '(none)'}  out_dir={out_dir}")

    # When --num-prompts is set, the run isn't "full" but uses the full
    # composition path; flag it on the manifest as full=False so resume
    # logic can distinguish.
    prompts = _load_prompts(full=args.full, num_prompts=args.num_prompts, slices=slice_names)
    if not prompts:
        sys.exit("no prompts loaded.")
    print(f"[{run_id}] loaded {len(prompts)} prompts")

    _write_json(out_dir / "manifest.json", {
        "run_id": run_id,
        "routers": list(routers),
        "judges": list(judges),
        "baseline": args.baseline,
        "prompt_count": len(prompts),
        "full": args.full,
    })
    (out_dir / "prompts.jsonl").write_text(
        "\n".join(p.model_dump_json() for p in prompts) + "\n"
    )

    # Fan inference.
    print(f"[{run_id}] inference: {len(prompts) * len(routers)} calls")
    sem = asyncio.Semaphore(INFERENCE_CONCURRENCY)
    started = time.monotonic()
    inf_results = await asyncio.gather(*(
        _run_inference(sem=sem, out_dir=out_dir, run_id=run_id, prompt=p, router=r, force=args.force)
        for p in prompts for r in routers
    ))
    inf_errs = sum(1 for _, _, e in inf_results if e)
    print(f"  done in {time.monotonic() - started:.1f}s; errors={inf_errs}")

    inference_rows: list[InferenceRow] = _read_jsonl_dir(out_dir / "inference", InferenceRow)

    # Routing matrix is always shown — pairs nicely with the per-router
    # quality scores rendered below once judging completes.
    _print_routing_matrix(inference_rows, routers)

    judgment_rows: list[JudgmentRow] = []
    if judges:
        if args.baseline not in routers:
            sys.exit(f"baseline router {args.baseline!r} must be in --routers when judging.")
        # Build (prompt_id → router → output_text) for fast lookup.
        text_by: dict[tuple[str, str], str] = {
            (r.prompt_id, r.router): r.output_text for r in inference_rows
        }
        candidates = [r for r in routers if r != args.baseline]
        judge_args = [
            (p, j, c)
            for p in prompts
            for j in judges
            for c in candidates
        ]
        print(f"[{run_id}] judging: {len(judge_args)} calls")
        jsem = asyncio.Semaphore(JUDGE_CONCURRENCY)
        started = time.monotonic()
        results = await asyncio.gather(*(
            _run_judge(
                sem=jsem,
                out_dir=out_dir,
                run_id=run_id,
                prompt=p,
                judge_name=j,
                candidate=c,
                candidate_text=text_by.get((p.prompt_id, c), ""),
                baseline=args.baseline,
                baseline_text=text_by.get((p.prompt_id, args.baseline), ""),
                force=args.force,
            )
            for (p, j, c) in judge_args
        ))
        j_errs = sum(1 for _, _, _, e in results if e)
        print(f"  done in {time.monotonic() - started:.1f}s; errors={j_errs}")
        judgment_rows = _read_jsonl_dir(out_dir / "judgments", JudgmentRow)

    # Aggregate (always — Pareto only renders if we have judgments).
    results = aggregate_to_router_results(
        inference_rows, judgment_rows, baseline_router=args.baseline,
    )
    _write_json(out_dir / "aggregated.json", [r.model_dump() for r in results])

    print()
    print(render_table(results))

    if judgment_rows:
        fig = render_plot(to_points(results), title=f"{run_id}: cost vs quality")
        pareto_path = out_dir / "pareto.png"
        fig.savefig(pareto_path, dpi=150)
        md = (
            f"# {run_id}\n\n"
            f"![Pareto plot](pareto.png)\n\n"
            f"{render_table(results)}\n"
        )
        (out_dir / "eval_results.md").write_text(md)
        print(f"\nWrote pareto.png + eval_results.md to {out_dir}")

    return 0


def main() -> None:
    args = _parse_args()
    sys.exit(asyncio.run(_orchestrate(args)))


if __name__ == "__main__":
    main()
