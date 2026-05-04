"""LiveCodeBench grading via vendored RouterArena harness.

Loads ``lighteval/code_generation_lite`` (release_v2, the source LCB
release RouterArena's prep_datasets.py uses), decodes the private
test cases, matches against our cached responses by ``Question``
content, and runs each model response through their
``check_correctness`` sandboxed runner. Updates ``score`` / ``correct``
/ ``gradeable`` / ``grade_mode`` for every LiveCodeBench row in the
input JSON, then recomputes the headline accuracy + arena_score.

Concurrency: their runner spawns one multiprocessing.Process per
problem with a per-test timeout; we fan out across a small process
pool so 385 prompts × ~3 tests × 6s timeout doesn't take an hour
serially. ``--workers 4`` is conservative for a laptop.

Safety: model-generated code runs locally. The vendored
``reliability_guard`` blocks file/network/process syscalls, and each
worker is a separate process killed on timeout. Same pipeline the
RouterArena leaderboard uses.
"""

from __future__ import annotations

import argparse
import copy
import json
import re
import sys
import time
from collections import defaultdict
from concurrent.futures import ProcessPoolExecutor, as_completed
from pathlib import Path

import pandas as pd
from huggingface_hub import hf_hub_download

from eval.arena_score import arena_score
from eval.routerarena import ROUTERARENA_REPO, SPLIT_TO_FILE

# Vendor path setup before importing.
_VENDOR = Path(__file__).parent / "_routerarena_official"
sys.path.insert(0, str(_VENDOR))

from livecodebench_util import (  # noqa: E402
    check_correctness,
    has_code,
    has_test_type,
    post_process_code,
    translate_private_test_cases,
)

_WS_RE = re.compile(r"\s+")


def _normalize(text: str) -> str:
    """Collapse runs of whitespace so trivial formatting diffs match.

    The RouterArena ``Question`` column and LiveCodeBench's
    ``question_content`` are nominally identical, but a stray trailing
    newline or doubled space would otherwise break exact lookup.
    """
    return _WS_RE.sub(" ", text).strip()


def _build_lcb_index() -> dict[str, dict]:
    """Return a {question_content: problem_dict} index over LCB v2.

    Each value is a dict with ``test`` (list of test cases) and
    ``is_stdin`` (bool) — the shape ``check_correctness`` expects.
    """
    from datasets import load_dataset
    print("[lcb] loading lighteval/code_generation_lite release_v2...", file=sys.stderr)
    ds = load_dataset(
        "lighteval/code_generation_lite", "release_v2",
        split="test", trust_remote_code=True,
    )
    out: dict[str, dict] = {}
    for row in ds:
        try:
            private = translate_private_test_cases(row["private_test_cases"])
        except Exception as e:
            print(f"[lcb] skip {row.get('question_id')}: decode error {e}", file=sys.stderr)
            continue
        public = json.loads(row["public_test_cases"]) if row["public_test_cases"] else []
        is_stdin = has_test_type(row["public_test_cases"], "stdin")
        out[_normalize(row["question_content"])] = {
            "test": list(public) + list(private),
            "is_stdin": is_stdin,
            "_question_id": row["question_id"],
        }
    print(f"[lcb] indexed {len(out)} problems", file=sys.stderr)
    return out


def _grade_one(args: tuple[str, dict, str]) -> tuple[str, float, str]:
    """Worker: returns (sample_id, score, mode)."""
    sample_id, problem, response = args
    code_blocks = has_code(response)
    if not code_blocks:
        return sample_id, 0.0, "code_accuracy:no_code"
    code = post_process_code(code_blocks[-1])
    try:
        score = check_correctness(
            problem=copy.deepcopy(problem),
            completion=code,
            timeout=6.0,
            runtime_debug=False,
            is_extracted=not problem["is_stdin"],
        )
    except Exception as e:
        return sample_id, 0.0, f"code_accuracy:error:{type(e).__name__}"
    return sample_id, float(score), "code_accuracy"


def main(input_path: str, workers: int) -> None:
    src = Path(input_path)
    data = json.loads(src.read_text())
    rows = data["rows"]

    # Map our LCB rows by Question content (RouterArena's Question
    # field equals LCB's question_content). The dataset parquet has
    # the Question field too, but we already saved enough on the rows;
    # however ``response_text`` is what we need plus the matching key.
    # We re-load the parquet for the Question column.
    parquet_path = hf_hub_download(
        ROUTERARENA_REPO, SPLIT_TO_FILE["full"], repo_type="dataset"
    )
    df = pd.read_parquet(parquet_path)
    sid_to_question = {str(r["Global Index"]): r["Question"] for _, r in df.iterrows()}

    lcb_index = _build_lcb_index()

    work_items: list[tuple[str, dict, str]] = []
    matched, unmatched = 0, 0
    for r in rows:
        if not r.get("dataset_name", "").startswith("LiveCodeBench"):
            continue
        if r.get("error") or not r.get("response_text"):
            r["score"] = 0.0
            r["correct"] = False
            r["gradeable"] = True  # we have a response slot, just no code
            r["grade_mode"] = "code_accuracy:empty"
            continue
        question = sid_to_question.get(r["sample_id"])
        if question is None:
            unmatched += 1
            continue
        # Look up by exact normalized-whitespace key. The previous
        # ``startswith`` / ``in`` fallback could collide when two LCB
        # problems share a long common preamble, so we restrict to a
        # collision-free lookup.
        problem = lcb_index.get(_normalize(question))
        if problem is None:
            unmatched += 1
            r["score"] = 0.0
            r["correct"] = False
            r["gradeable"] = False
            r["grade_mode"] = "code_accuracy:no_problem_match"
            continue
        matched += 1
        work_items.append((r["sample_id"], problem, r["response_text"]))

    print(f"[lcb] matched={matched} unmatched={unmatched}", file=sys.stderr)
    print(f"[lcb] grading {len(work_items)} responses with {workers} workers...", file=sys.stderr)

    sid_to_result: dict[str, tuple[float, str]] = {}
    started = time.monotonic()
    with ProcessPoolExecutor(max_workers=workers) as pool:
        futures = [pool.submit(_grade_one, item) for item in work_items]
        for i, fut in enumerate(as_completed(futures), start=1):
            sid, score, mode = fut.result()
            sid_to_result[sid] = (score, mode)
            if i % 25 == 0 or i == len(work_items):
                elapsed = time.monotonic() - started
                rate = i / elapsed if elapsed > 0 else 0
                correct = sum(1 for s, _ in sid_to_result.values() if s > 0)
                print(
                    f"[lcb] {i}/{len(work_items)} ({rate:.1f}/s) correct={correct}",
                    file=sys.stderr,
                )

    # Patch rows.
    for r in rows:
        if r["sample_id"] in sid_to_result:
            score, mode = sid_to_result[r["sample_id"]]
            r["score"] = score
            r["correct"] = score >= 0.5
            r["gradeable"] = True
            r["grade_mode"] = mode

    # Recompute headline. Denominator is every attempted prompt
    # (including errored / non-routed rows) — those contribute 0,
    # matching the leaderboard's treatment per RouterArena's
    # ``compute_scores.py``.
    routed = [r for r in rows if r.get("picked_model") and not r.get("error")]
    n_attempted = len(rows) or 1
    accuracy = sum(r["score"] for r in rows) / n_attempted
    gradeable = [r for r in routed if r["gradeable"]]
    accuracy_gradeable_only = (
        sum(r["score"] for r in gradeable) / len(gradeable) if gradeable else 0.0
    )
    cost_per_1k = data.get("estimated_cost_per_1k_queries_usd", 0.0)
    arena = arena_score(accuracy, cost_per_1k)

    by_mode = defaultdict(lambda: [0, 0.0, 0])
    by_diff = defaultdict(lambda: [0, 0.0, 0])
    for r in gradeable:
        m = r["grade_mode"]
        by_mode[m][0] += 1
        by_mode[m][1] += r["score"]
        if r["correct"]:
            by_mode[m][2] += 1
    for r in routed:
        d = r.get("difficulty", "")
        by_diff[d][0] += 1
        by_diff[d][1] += r["score"]
        if r["correct"]:
            by_diff[d][2] += 1

    def _bucket(b):
        return {k: {"n": n, "correct": c, "accuracy": round(s / n, 4) if n else 0.0}
                for k, (n, s, c) in b.items()}

    data["rows"] = rows
    data["n_gradeable"] = len(gradeable)
    data["n_correct"] = sum(1 for r in gradeable if r["correct"])
    data["accuracy"] = round(accuracy, 4)
    data["accuracy_gradeable_only"] = round(accuracy_gradeable_only, 4)
    data["arena_score"] = round(arena, 4)
    data["accuracy_by_grade_mode"] = _bucket(by_mode)
    data["accuracy_by_difficulty"] = _bucket(by_diff)

    out = src.with_name(src.stem.replace("_regraded", "") + "_lcb.json")
    out.write_text(json.dumps(data, indent=2))
    print(f"[lcb] wrote {out}", file=sys.stderr)
    summary = {k: v for k, v in data.items() if k != "rows"}
    print(json.dumps(summary, indent=2))


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("input")
    parser.add_argument("--workers", type=int, default=4)
    args = parser.parse_args()
    main(args.input, args.workers)
