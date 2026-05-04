"""Cohen's kappa spot-check CLI.

Pulls a stratified sample of (prompt, candidate) pairs from
judgments.jsonl, prompts the human for per-dimension scores, computes
Cohen's kappa between the human means and the ensemble medians, and
writes the responses out for audit.

Run:
    python -m eval.spot_check --judgments path/to/judgments.jsonl \\
                              --prompts   path/to/prompts.jsonl \\
                              --inference path/to/inference.jsonl \\
                              --n 30 --out spot_check.jsonl

Kappa < 0.6 means the rubric is broken — fix and re-run before reporting.
"""

from __future__ import annotations

import argparse
import json
import random
from collections import defaultdict
from datetime import UTC, datetime
from pathlib import Path

from eval.rubric import DIMENSIONS, aggregate
from eval.types import BenchmarkPrompt, InferenceRow, JudgmentRow, RubricScores

# Fixed sampling seed: kappa numbers should be reproducible across runs of
# the spot-check CLI on the same judgments.jsonl input.
_STRATIFIED_SAMPLE_SEED = 0xCAFE


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--judgments", required=True, type=Path)
    p.add_argument("--prompts", required=True, type=Path)
    p.add_argument("--inference", required=True, type=Path)
    p.add_argument("--n", type=int, default=30, help="number of rows to spot-check")
    p.add_argument("--out", type=Path, default=Path("spot_check.jsonl"))
    args = p.parse_args()

    judgments = _load_jsonl(args.judgments, JudgmentRow)
    prompts = {p.prompt_id: p for p in _load_jsonl(args.prompts, BenchmarkPrompt)}
    inference = _index_inference(_load_jsonl(args.inference, InferenceRow))

    sample = _stratified_sample(judgments, n=args.n)
    print(f"Spot-checking {len(sample)} rows...\n")

    human_scores: list[float] = []
    ensemble_scores: list[float] = []
    out_rows: list[dict] = []
    for i, j in enumerate(sample, start=1):
        prompt = prompts.get(j.prompt_id)
        candidate_text = inference.get((j.prompt_id, j.candidate_router))
        baseline_text = inference.get((j.prompt_id, j.baseline_router))
        if prompt is None or candidate_text is None or baseline_text is None:
            print(f"[skip] missing prompt/inference for {j.prompt_id} / {j.candidate_router}")
            continue
        print(f"\n=== {i}/{len(sample)}  prompt={j.prompt_id}  candidate={j.candidate_router} ===")
        print(f"PROMPT:\n{prompt.prompt_text[:800]}\n")
        print(f"BASELINE (always-opus):\n{baseline_text[:800]}\n")
        print(f"CANDIDATE ({j.candidate_router}):\n{candidate_text[:800]}\n")
        print(
            f"Ensemble said: candidate score={j.score:.2f} "
            f"(judge={j.judge}, rubric={j.rubric.model_dump()})"
        )
        human = _read_human_scores()
        # Use the same normalization as the ensemble so the kappa
        # bucketing on both sides operates over [0, 1].
        h_score = aggregate(human)
        human_scores.append(h_score)
        ensemble_scores.append(j.score)
        out_rows.append(
            {
                "prompt_id": j.prompt_id,
                "candidate_router": j.candidate_router,
                "judge": j.judge,
                "ensemble_score": j.score,
                "human_score": h_score,
                "human_rubric": human.model_dump(),
            }
        )

    args.out.write_text("\n".join(json.dumps(r) for r in out_rows))

    kappa = _cohens_kappa_quartiles(human_scores, ensemble_scores)
    print(f"\nSpot-check complete. n={len(human_scores)}  kappa={kappa:.3f}  (gate: kappa >= 0.6)")
    print(f"Audit log written to {args.out}")


def _read_human_scores() -> RubricScores:
    """Prompt the user for 5 integer scores."""
    print("Enter your scores (1-5) for each dimension. Defaults to '3' if blank.")
    fields = {}
    for d in DIMENSIONS:
        while True:
            raw = input(f"  {d}: ").strip() or "3"
            try:
                v = int(raw)
                if not 1 <= v <= 5:
                    raise ValueError
                fields[d] = v
                break
            except ValueError:
                print("    must be an integer 1..5")
    return RubricScores(**fields)


def _stratified_sample(judgments: list[JudgmentRow], *, n: int) -> list[JudgmentRow]:
    """Spread the sample across (candidate_router, judge) buckets.

    Within each bucket we randomly sample with a fixed seed so the kappa
    numbers reproduce across CLI invocations but aren't biased by the
    file-order the harness happened to write rows in.
    """
    by_bucket: dict[tuple[str, str], list[JudgmentRow]] = defaultdict(list)
    for j in judgments:
        by_bucket[(j.candidate_router, j.judge)].append(j)
    if not by_bucket:
        return []
    rng = random.Random(_STRATIFIED_SAMPLE_SEED)
    per = max(1, n // len(by_bucket))
    out: list[JudgmentRow] = []
    for rows in by_bucket.values():
        if len(rows) <= per:
            out.extend(rows)
        else:
            out.extend(rng.sample(rows, per))
    return out[:n]


def _index_inference(rows: list[InferenceRow]) -> dict[tuple[str, str], str]:
    return {(r.prompt_id, r.router): r.output_text for r in rows}


def _load_jsonl(path: Path, cls):
    out = []
    with path.open() as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            out.append(cls.model_validate_json(line))
    return out


def _cohens_kappa_quartiles(a: list[float], b: list[float]) -> float:
    """Coarsen each [0,1] score into 4 buckets and run quadratic-weighted kappa.

    The judge ensemble emits continuous scores; humans give integer
    rubric values that aggregate into a finer grid. Bucketing into
    quartiles is the standard reduction in the LLM-judge literature.
    Implemented inline (no sklearn dep) since the grid is tiny.
    """
    if not a or len(a) != len(b):
        return 0.0

    def bucket(x: float) -> int:
        if x < 0.25:
            return 0
        if x < 0.50:
            return 1
        if x < 0.75:
            return 2
        return 3

    ratings_a = [bucket(x) for x in a]
    ratings_b = [bucket(x) for x in b]
    return _quadratic_weighted_kappa(ratings_a, ratings_b, k=4)


def _quadratic_weighted_kappa(a: list[int], b: list[int], *, k: int) -> float:
    """Quadratic-weighted Cohen's kappa for ordinal ratings in [0, k)."""
    n = len(a)
    if n == 0:
        return 0.0
    obs = [[0] * k for _ in range(k)]
    hist_a = [0] * k
    hist_b = [0] * k
    for x, y in zip(a, b):
        obs[x][y] += 1
        hist_a[x] += 1
        hist_b[y] += 1
    weights = [[((i - j) ** 2) / ((k - 1) ** 2) for j in range(k)] for i in range(k)]
    expected = [[hist_a[i] * hist_b[j] / n for j in range(k)] for i in range(k)]
    num = sum(weights[i][j] * obs[i][j] for i in range(k) for j in range(k))
    den = sum(weights[i][j] * expected[i][j] for i in range(k) for j in range(k))
    if den == 0:
        return 0.0
    return 1.0 - num / den


if __name__ == "__main__":
    main()
