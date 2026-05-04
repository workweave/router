"""Splice an LCB-only re-run back into a full-run results file.

Usage::

    .venv/bin/python -m eval.merge_lcb \\
      --base results/routerarena_v0.6_full_official_regraded.json \\
      --lcb  results/routerarena_v0.6_lcb_only.json \\
      --out  results/routerarena_v0.6_full_with_lcb.json

The LCB-only run uses a higher ``max_output_tokens`` so model
responses include real code blocks instead of truncated CoT. We swap
those rows by ``sample_id`` over the base file's rows; everything
else (other datasets, summary headers) stays untouched. Run
``grade_lcb.py`` on the merged output to actually score the new code.
"""

from __future__ import annotations

import argparse
import json
from pathlib import Path


def main(base_path: Path, lcb_path: Path, out_path: Path) -> None:
    base = json.loads(base_path.read_text())
    lcb = json.loads(lcb_path.read_text())

    lcb_by_sid = {r["sample_id"]: r for r in lcb["rows"]}
    swapped = 0
    for i, row in enumerate(base["rows"]):
        if not row.get("dataset_name", "").startswith("LiveCodeBench"):
            continue
        new = lcb_by_sid.get(row["sample_id"])
        if new is None:
            continue
        # Preserve sample_id / dataset_name / domain / difficulty from
        # the base row (those don't change), but overwrite the response
        # + token usage + latency from the LCB-only run.
        merged = dict(row)
        merged.update({
            "picked_model": new["picked_model"],
            "input_tokens": new["input_tokens"],
            "output_tokens": new["output_tokens"],
            "latency_ms": new["latency_ms"],
            "error": new.get("error", ""),
            "response_text": new.get("response_text", ""),
            # Reset grading fields — grade_lcb.py rewrites them.
            "score": 0.0,
            "correct": False,
            "gradeable": False,
            "grade_mode": "code_accuracy:pending",
        })
        base["rows"][i] = merged
        swapped += 1
    print(f"[merge] swapped {swapped} LCB rows")

    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(json.dumps(base, indent=2))
    print(f"[merge] wrote {out_path}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--base", type=Path, required=True)
    parser.add_argument("--lcb", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    main(args.base, args.lcb, args.out)
