"""RouterArena harness: routing distribution + official-methodology accuracy.

Two modes, gated by ``--max-output-tokens``:

  - **Shortcut mode (default = 1)**: cheap routing-only pass. The
    cluster scorer still runs and the picked model still gets
    dispatched, but we cap output at one token so we don't pay for
    response generation. Use this to sanity-check the routing
    distribution + cost shape against the leaderboard.

  - **Live mode (e.g. 512)**: full inference with on-the-fly grading
    using the **vendored official RouterArena methodology**
    (``_routerarena_official/``, Apache-2.0). Each prompt uses the
    exact per-dataset template from
    ``config/eval_config/zero-shot/<dataset>.json`` (``\\boxed{X}``
    answer convention). Each response is scored by the dataset's
    official metric (mcq_accuracy / math_metric / exact_match /
    meteor_score / superglue_*). LiveCodeBench is left ungradeable
    until we wire in their sandboxed code-execution harness — every
    other dataset is graded the way the leaderboard grades it.

  ~$15–20 for v0.6 across the full 8,400. The number it produces is
  directly comparable to the leaderboard's "Accuracy" column;
  ``compute_arena_score`` reproduces the "Acc-Cost Arena" score.

What this gives you in either mode:
  - Per-(domain, difficulty) routing distribution.
  - Estimated cost per 1k queries (input-only in shortcut mode;
    full-inference in live mode).
  - Coverage / error tally — staging-path bugs surface here.

Dataset: ``RouteWorks/RouterArena`` on HF.
  - ``sub_10``  — 809 prompts (stratified). Use this for the
    shortcut.
  - ``full``    — 8,400 prompts. Use once sub_10 looks reasonable.
  - ``robustness`` — perturbed-prompt subset.

CLI::

    poetry run python -m eval.routerarena \\
        --router v0.2-cluster --split sub_10 --n 100 \\
        --out results/routerarena_v0.2_sub10.json
"""

from __future__ import annotations

import argparse
import asyncio
import json
import statistics
import sys
import time
from collections import Counter, defaultdict
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any

import eval._env  # noqa: F401  — auto-load router/eval/.env
from eval.arena_score import arena_score
from eval.grade import GradeResult, grade
from eval.pricing import COST_PER_1K_INPUT, COST_PER_1K_OUTPUT
from eval.routing import route
from eval.types import RouterName

ROUTERARENA_REPO = "RouteWorks/RouterArena"
SPLIT_TO_FILE = {
    "sub_10": "data/sub_10-00000-of-00001.parquet",
    "full": "data/full-00000-of-00001.parquet",
    "robustness": "data/robustness-00000-of-00001.parquet",
}


@dataclass
class RoutingRow:
    sample_id: str
    domain: str
    difficulty: str
    dataset_name: str
    picked_model: str
    input_tokens: int
    output_tokens: int
    latency_ms: int
    error: str = ""
    # Live-mode-only fields. Empty string / False / 0.0 when shortcut mode.
    response_text: str = ""
    score: float = 0.0  # 0..1 continuous; what the leaderboard averages
    correct: bool = False  # score >= 0.5; convenience binary
    gradeable: bool = False
    grade_mode: str = ""


@dataclass
class RouterArenaSummary:
    router: str
    split: str
    n_attempted: int
    n_routed: int  # successful routing decisions
    coverage: float
    pick_distribution: dict[str, int]
    pick_distribution_pct: dict[str, float]
    pick_by_difficulty: dict[str, dict[str, int]]
    pick_by_domain: dict[str, dict[str, int]]
    estimated_cost_per_1k_queries_usd: float
    mean_input_tokens: float
    mean_output_tokens: float
    median_latency_ms: float
    p95_latency_ms: float
    # Accuracy fields. All zero in shortcut mode. ``accuracy`` is the
    # mean of ``score`` over the *attempted* set (matches RouterArena's
    # leaderboard methodology, where ungradeable prompts contribute 0).
    # ``accuracy_gradeable_only`` is the mean over the gradeable
    # subset and is useful when LiveCodeBench (currently ungradeable)
    # is dragging the headline number down — disclose both.
    n_gradeable: int = 0
    n_correct: int = 0
    accuracy: float = 0.0
    accuracy_gradeable_only: float = 0.0
    arena_score: float = 0.0  # RouterArena's "Acc-Cost Arena" composite, β=0.1
    accuracy_by_difficulty: dict[str, dict[str, float | int]] = field(default_factory=dict)
    accuracy_by_domain: dict[str, dict[str, float | int]] = field(default_factory=dict)
    accuracy_by_grade_mode: dict[str, dict[str, float | int]] = field(default_factory=dict)
    rows: list[RoutingRow] = field(default_factory=list)


_PROMPT_TEMPLATES_PATH = Path(__file__).parent / "_routerarena_official" / "prompt_templates.json"
_PROMPT_TEMPLATES: dict[str, Any] = json.loads(_PROMPT_TEMPLATES_PATH.read_text())


def _resolve_template(dataset_name: str) -> str | None:
    """Look up the official prompt template for a dataset, matching
    RouterArena's ``utils.py::load_data`` prefix-contains semantics.
    Returns None if no match (caller falls back to a generic format).
    LiveCodeBench returns the ``not_is_stdin_prompt`` variant — we
    don't have access to the LCB ``is_stdin`` flag without joining in
    their separate ``./dataset/livecodebench`` artifact, so we use the
    function-body prompt for both. Acceptable cost: LCB is currently
    ungraded anyway pending code-execution support.
    """
    if dataset_name in _PROMPT_TEMPLATES:
        tpl = _PROMPT_TEMPLATES[dataset_name]
        if isinstance(tpl, dict):
            return tpl.get("no_stdin", tpl.get("is_stdin"))
        return tpl
    # Composite names like "MMLUPro_chemistry" -> match on prefix.
    for prefix, tpl in _PROMPT_TEMPLATES.items():
        if dataset_name.startswith(prefix + "_") or dataset_name.startswith(prefix):
            if isinstance(tpl, dict):
                return tpl.get("no_stdin", tpl.get("is_stdin"))
            return tpl
    return None


def _escape_braces(text: str) -> str:
    """Match RouterArena's ``utils.py::escape_format_braces``. Single
    ``{`` / ``}`` from question/context/options content gets doubled so
    ``str.format`` doesn't try to interpret them as placeholders."""
    out: list[str] = []
    i = 0
    while i < len(text):
        c = text[i]
        if c in "{}":
            out.append(c * 2)
        else:
            out.append(c)
        i += 1
    return "".join(out)


def _format_prompt(
    *,
    dataset_name: str,
    question: str,
    options: Any,
    context: str,
    answer: Any,
) -> str:
    """Assemble the per-prompt input using the official RouterArena
    template for ``dataset_name``. We mirror their ``load_data`` shim
    in ``llm_evaluation/utils.py``: SuperGLUE-RC takes Question +
    Answer, SuperGLUE-Wic takes Question + Context, MC tasks take
    Context + Question + Options, free-form tasks take Context +
    Question. Falls back to a generic format if no template matches —
    happens only for datasets not in their config dir, which (after
    vendoring) should be empty.
    """
    template = _resolve_template(dataset_name)
    options_str = ""
    if options is not None and len(options) > 0:
        options_str = "".join(
            f"{chr(65 + i)}. {opt}\n" for i, opt in enumerate(options)
        )

    if template is None:
        # Generic fallback: behaves like our previous Final-answer
        # format. Should not fire in practice once vendoring is
        # complete.
        parts: list[str] = []
        if context:
            parts.append(f"Context:\n{context}\n")
        parts.append((question or "").strip())
        if options_str:
            parts.append(f"\nOptions:\n{options_str}")
        parts.append(
            "\nProvide your final answer in \\boxed{X} format."
        )
        return "\n".join(parts)

    # Match RouterArena's per-dataset arg shape.
    ctx = _escape_braces(context if context else "None")
    q = _escape_braces(question or "")
    opts = _escape_braces(options_str)
    ans = _escape_braces("" if answer is None else str(answer))

    if dataset_name == "SuperGLUE-RC":
        return template.format(Question=q, Answer=ans)
    if dataset_name == "SuperGLUE-Wic":
        return template.format(Question=q, Context=ctx)
    if dataset_name == "LiveCodeBench":
        return template.format(Question=q)
    if not options_str:
        return template.format(Context=ctx, Question=q)
    return template.format(Context=ctx, Question=q, Options=opts)


async def _route_one(
    *,
    router: RouterName,
    row: dict[str, Any],
    semaphore: asyncio.Semaphore,
    max_output_tokens: int,
    do_grade: bool,
) -> RoutingRow:
    prompt = _format_prompt(
        dataset_name=row.get("Dataset name", ""),
        question=row.get("Question", "") or "",
        options=row.get("Options"),
        context=row.get("Context", "") or "",
        answer=row.get("Answer"),
    )
    async with semaphore:
        # In shortcut mode (max_output_tokens=1) we keep cost on the
        # input side; the cluster scorer still runs the full embedding
        # + argmax, so the routing decision matches production. In live
        # mode we cap at the caller-supplied budget so RouterArena
        # responses come back in full and the grader has something to
        # work with.
        res = await route(router=router, prompt=prompt, max_output_tokens=max_output_tokens)

    grade_result: GradeResult | None = None
    if do_grade and not res.error:
        grade_result = grade(
            dataset_name=row.get("Dataset name", ""),
            gold_answer=row.get("Answer"),
            response=res.output_text,
            options=row.get("Options"),
        )

    return RoutingRow(
        sample_id=row.get("Global Index", ""),
        domain=row.get("Domain", ""),
        difficulty=row.get("Difficulty", ""),
        dataset_name=row.get("Dataset name", ""),
        picked_model=res.model_used,
        input_tokens=res.input_tokens,
        output_tokens=res.output_tokens,
        latency_ms=res.latency_ms,
        error=res.error or "",
        response_text=(res.output_text or "")[:2000] if do_grade else "",
        score=grade_result.score if grade_result else 0.0,
        correct=grade_result.correct if grade_result else False,
        gradeable=grade_result.gradeable if grade_result else False,
        grade_mode=grade_result.mode if grade_result else "",
    )


async def evaluate(
    *,
    router: RouterName,
    split: str = "sub_10",
    n: int | None = None,
    concurrency: int = 8,
    max_output_tokens: int = 1,
    output_path: Path | None = None,
    progress_every: int = 25,
    only_dataset_prefix: str | None = None,
) -> RouterArenaSummary:
    from huggingface_hub import hf_hub_download
    import pandas as pd

    if split not in SPLIT_TO_FILE:
        raise ValueError(f"unknown split {split!r}; expected one of {list(SPLIT_TO_FILE)}")
    parquet_path = hf_hub_download(
        ROUTERARENA_REPO, SPLIT_TO_FILE[split], repo_type="dataset"
    )
    df = pd.read_parquet(parquet_path)
    if n is not None and n < len(df):
        df = df.iloc[:n]

    rows = df.to_dict(orient="records")
    # Optional dataset prefix filter — used by LCB-only re-runs that
    # need a higher max_output_tokens budget than the full sweep.
    if only_dataset_prefix:
        rows = [r for r in rows if str(r.get("Dataset name", "")).startswith(only_dataset_prefix)]
        print(f"[routerarena] filtered to dataset_name startswith {only_dataset_prefix!r}: n={len(rows)}", file=sys.stderr)
    semaphore = asyncio.Semaphore(concurrency)

    do_grade = max_output_tokens > 1
    mode = "live+grade" if do_grade else "shortcut"
    print(
        f"[routerarena] router={router} split={split} n={len(rows)} "
        f"concurrency={concurrency} mode={mode} max_out={max_output_tokens}",
        file=sys.stderr,
    )
    started = time.monotonic()
    tasks = [
        _route_one(
            router=router,
            row=r,
            semaphore=semaphore,
            max_output_tokens=max_output_tokens,
            do_grade=do_grade,
        )
        for r in rows
    ]
    results: list[RoutingRow] = []
    for i, fut in enumerate(asyncio.as_completed(tasks), start=1):
        try:
            results.append(await fut)
        except Exception as e:  # surface staging-path bugs but keep going
            results.append(RoutingRow(
                sample_id="", domain="", difficulty="", dataset_name="",
                picked_model="", input_tokens=0, output_tokens=0,
                latency_ms=0, error=f"unhandled: {type(e).__name__}: {e}",
            ))
        if i % progress_every == 0 or i == len(rows):
            elapsed = time.monotonic() - started
            rate = i / elapsed if elapsed > 0 else 0
            graded = sum(1 for r in results if r.gradeable)
            correct = sum(1 for r in results if r.correct)
            acc_str = f" acc={correct}/{graded}" if do_grade and graded else ""
            print(f"[routerarena] {i}/{len(rows)} rows ({rate:.1f}/s){acc_str}", file=sys.stderr)

    summary = _summarize(router=router, split=split, results=results)
    if output_path is not None:
        output_path.parent.mkdir(parents=True, exist_ok=True)
        payload = asdict(summary)
        # Trim heavy rows by default — keep only essentials in summary.
        payload["rows"] = [
            {k: v for k, v in asdict(r).items() if k != "context"}
            for r in summary.rows
        ]
        output_path.write_text(json.dumps(payload, indent=2))
        print(f"[routerarena] wrote {output_path}", file=sys.stderr)
    return summary


def _summarize(
    *,
    router: str,
    split: str,
    results: list[RoutingRow],
) -> RouterArenaSummary:
    routed = [r for r in results if r.picked_model and not r.error]
    n_attempted = len(results)
    n_routed = len(routed)

    pick_counter = Counter(r.picked_model for r in routed)
    total_picks = sum(pick_counter.values()) or 1
    pick_pct = {m: round(c / total_picks * 100, 2) for m, c in pick_counter.items()}

    by_difficulty: dict[str, Counter[str]] = defaultdict(Counter)
    by_domain: dict[str, Counter[str]] = defaultdict(Counter)
    for r in routed:
        by_difficulty[r.difficulty][r.picked_model] += 1
        by_domain[r.domain][r.picked_model] += 1

    # Cost estimate: input tokens × picked-model input price + 1 output
    # token × picked-model output price. We capped output at 1, so the
    # cost-side comparison is input-dominated and apples-to-apples
    # across routers.
    total_cost = 0.0
    n_costed = 0
    for r in routed:
        cin = COST_PER_1K_INPUT.get(r.picked_model)
        cout = COST_PER_1K_OUTPUT.get(r.picked_model)
        if cin is None or cout is None:
            continue
        total_cost += (r.input_tokens / 1000.0) * cin + (r.output_tokens / 1000.0) * cout
        n_costed += 1
    cost_per_1k = (total_cost / n_costed * 1000.0) if n_costed else 0.0

    latencies = sorted(r.latency_ms for r in routed)

    # Authoritative accuracy = mean(score) over all attempted rows.
    # Ungradeable prompts contribute 0 — matches the leaderboard's
    # treatment, where every prompt counts (LiveCodeBench too, even
    # when our local harness can't grade it). Errored / non-routed
    # rows have score=0 and are included in the denominator, so a
    # router that fails on N requests is penalized the same as one
    # that answers them incorrectly. We also report
    # ``accuracy_gradeable_only`` for readers who want to know what we
    # got on the prompts we *can* grade.
    gradeable = [r for r in routed if r.gradeable]
    n_gradeable = len(gradeable)
    n_correct = sum(1 for r in gradeable if r.correct)
    n_attempted_for_acc = n_attempted or 1
    accuracy = sum(r.score for r in results) / n_attempted_for_acc
    accuracy_gradeable_only = (
        sum(r.score for r in gradeable) / n_gradeable if n_gradeable else 0.0
    )
    arena = arena_score(accuracy, cost_per_1k)

    def _slice(rows: list[RoutingRow], key: str) -> dict[str, dict[str, float | int]]:
        out: dict[str, dict[str, float | int]] = {}
        buckets: dict[str, list[RoutingRow]] = defaultdict(list)
        for r in rows:
            buckets[getattr(r, key)].append(r)
        for k, vs in buckets.items():
            n = len(vs)
            mean_score = sum(r.score for r in vs) / n if n else 0.0
            c = sum(1 for r in vs if r.correct)
            out[k] = {
                "n": n,
                "correct": c,
                "accuracy": round(mean_score, 4),
                "binary_accuracy": round(c / n, 4) if n else 0.0,
            }
        return out

    return RouterArenaSummary(
        router=router,
        split=split,
        n_attempted=n_attempted,
        n_routed=n_routed,
        coverage=n_routed / n_attempted if n_attempted else 0.0,
        pick_distribution=dict(pick_counter),
        pick_distribution_pct=pick_pct,
        pick_by_difficulty={k: dict(v) for k, v in by_difficulty.items()},
        pick_by_domain={k: dict(v) for k, v in by_domain.items()},
        estimated_cost_per_1k_queries_usd=round(cost_per_1k, 4),
        mean_input_tokens=round(statistics.fmean(r.input_tokens for r in routed) if routed else 0.0, 1),
        mean_output_tokens=round(statistics.fmean(r.output_tokens for r in routed) if routed else 0.0, 1),
        median_latency_ms=float(statistics.median(latencies)) if latencies else 0.0,
        p95_latency_ms=float(latencies[int(len(latencies) * 0.95)] if latencies else 0.0),
        n_gradeable=n_gradeable,
        n_correct=n_correct,
        accuracy=round(accuracy, 4),
        accuracy_gradeable_only=round(accuracy_gradeable_only, 4),
        arena_score=round(arena, 4),
        # Bucketed accuracy is computed over the routed set so
        # ungradeable prompts (e.g. LiveCodeBench) contribute 0,
        # matching the leaderboard. The per-mode slice below stays on
        # the gradeable subset since "ungradeable" is its own bucket.
        accuracy_by_difficulty=_slice(routed, "difficulty"),
        accuracy_by_domain=_slice(routed, "domain"),
        accuracy_by_grade_mode=_slice(gradeable, "grade_mode"),
        rows=results,
    )


def _main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--router", required=True, help="e.g. v0.6-cluster, heuristic, always-opus")
    parser.add_argument("--split", default="sub_10", choices=list(SPLIT_TO_FILE))
    parser.add_argument("--n", type=int, default=None, help="cap rows for a smoke run")
    parser.add_argument("--concurrency", type=int, default=8)
    parser.add_argument(
        "--max-output-tokens",
        type=int,
        default=1,
        help="1 = shortcut (routing-only); >1 = live inference + grading",
    )
    parser.add_argument(
        "--only-dataset-prefix",
        type=str,
        default=None,
        help="Filter prompts to those whose Dataset name starts with this prefix",
    )
    parser.add_argument("--out", type=Path, default=None)
    args = parser.parse_args()

    summary = asyncio.run(
        evaluate(
            router=args.router,
            split=args.split,
            n=args.n,
            concurrency=args.concurrency,
            max_output_tokens=args.max_output_tokens,
            output_path=args.out,
            only_dataset_prefix=args.only_dataset_prefix,
        )
    )
    # Compact stdout report.
    print(json.dumps({
        "router": summary.router,
        "split": summary.split,
        "n_attempted": summary.n_attempted,
        "n_routed": summary.n_routed,
        "coverage": round(summary.coverage, 4),
        "pick_distribution_pct": summary.pick_distribution_pct,
        "estimated_cost_per_1k_queries_usd": summary.estimated_cost_per_1k_queries_usd,
        "mean_input_tokens": summary.mean_input_tokens,
        "mean_output_tokens": summary.mean_output_tokens,
        "median_latency_ms": summary.median_latency_ms,
        "p95_latency_ms": summary.p95_latency_ms,
        "n_gradeable": summary.n_gradeable,
        "n_correct": summary.n_correct,
        "accuracy": summary.accuracy,
        "accuracy_gradeable_only": summary.accuracy_gradeable_only,
        "arena_score": summary.arena_score,
        "accuracy_by_difficulty": summary.accuracy_by_difficulty,
        "accuracy_by_grade_mode": summary.accuracy_by_grade_mode,
    }, indent=2))


if __name__ == "__main__":
    _main()
