"""Regrade a saved RouterArena run from cached responses.

Usage::

    .venv/bin/python -m eval.regrade results/routerarena_v0.6_full_official.json

Doesn't touch the staging deployment or any provider — purely local.
Useful when the grader logic changes (metric mapping fix, new metric
added) and we want to refresh accuracy without re-running inference.
Writes ``<input>_regraded.json`` next to the input.
"""

from __future__ import annotations

import json
import sys
from collections import defaultdict
from pathlib import Path

import pandas as pd
from huggingface_hub import hf_hub_download

import eval._env  # noqa: F401
from eval.arena_score import arena_score
from eval.grade import grade
from eval.routerarena import ROUTERARENA_REPO, SPLIT_TO_FILE


def main(src_path: str) -> None:
    src = Path(src_path)
    data = json.loads(src.read_text())
    rows = data["rows"]

    parquet_path = hf_hub_download(
        ROUTERARENA_REPO, SPLIT_TO_FILE["full"], repo_type="dataset"
    )
    df = pd.read_parquet(parquet_path)
    gi_to_row = {str(r["Global Index"]): r for _, r in df.iterrows()}

    regraded = []
    for r in rows:
        sid = r["sample_id"]
        src_row = gi_to_row.get(sid)
        if src_row is None or r.get("error") or not r.get("response_text"):
            regraded.append({**r, "score": 0.0, "correct": False, "gradeable": False, "grade_mode": "ungradeable"})
            continue
        gr = grade(
            dataset_name=src_row["Dataset name"],
            gold_answer=src_row["Answer"],
            response=r["response_text"],
            options=src_row["Options"],
        )
        regraded.append({
            **r,
            "score": gr.score,
            "correct": gr.correct,
            "gradeable": gr.gradeable,
            "grade_mode": gr.mode,
        })

    routed = [r for r in regraded if r.get("picked_model") and not r.get("error")]
    gradeable = [r for r in routed if r["gradeable"]]
    n_attempted = len(regraded)
    accuracy = sum(r["score"] for r in regraded) / n_attempted if n_attempted else 0.0
    accuracy_gradeable_only = (
        sum(r["score"] for r in gradeable) / len(gradeable) if gradeable else 0.0
    )
    cost_per_1k = data.get("estimated_cost_per_1k_queries_usd", 0.0)
    arena = arena_score(accuracy, cost_per_1k)

    by_diff = defaultdict(lambda: [0, 0.0, 0])  # n, sum_score, n_correct
    by_mode = defaultdict(lambda: [0, 0.0, 0])
    by_domain = defaultdict(lambda: [0, 0.0, 0])
    for r in routed:
        for key, bucket in (("difficulty", by_diff), ("domain", by_domain)):
            v = r.get(key, "")
            bucket[v][0] += 1
            bucket[v][1] += r["score"]
            if r["correct"]:
                bucket[v][2] += 1
    for r in gradeable:
        m = r["grade_mode"]
        by_mode[m][0] += 1
        by_mode[m][1] += r["score"]
        if r["correct"]:
            by_mode[m][2] += 1

    def _bucket(b):
        return {k: {"n": n, "correct": c, "accuracy": round(s / n, 4) if n else 0.0}
                for k, (n, s, c) in b.items()}

    data["rows"] = regraded
    data["n_gradeable"] = len(gradeable)
    data["n_correct"] = sum(1 for r in gradeable if r["correct"])
    data["accuracy"] = round(accuracy, 4)
    data["accuracy_gradeable_only"] = round(accuracy_gradeable_only, 4)
    data["arena_score"] = round(arena, 4)
    data["accuracy_by_difficulty"] = _bucket(by_diff)
    data["accuracy_by_grade_mode"] = _bucket(by_mode)
    data["accuracy_by_domain"] = _bucket(by_domain)

    out = src.with_name(src.stem + "_regraded.json")
    out.write_text(json.dumps(data, indent=2))
    print(f"wrote {out}")
    print(json.dumps({k: v for k, v in data.items() if k != "rows"}, indent=2))


if __name__ == "__main__":
    main(sys.argv[1])
