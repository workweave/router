"""Generic JSONL loader.

The Phase 1b real-traffic loader is one new file in this directory that
re-uses `load_jsonl` from here, pointing at a path on local disk or GCS.
For PR 2 it backs the hand-curated edge-cases fixture and is the
designated drop-in for any benchmark we want to ship as flat JSONL.

Each row in the JSONL must have at minimum:
    {"prompt_text": "...", "reference": {"kind": "...", "payload": {...}}}
Optional:
    {"metadata": {...}}
"""

from __future__ import annotations

import hashlib
import json
import random
from pathlib import Path
from typing import ClassVar

from eval.benchmarks import BenchmarkLoader, register
from eval.benchmarks._hf import stable_prompt_id
from eval.types import BenchmarkPrompt, Reference


def load_jsonl(path: Path, *, slice_name: str, source: str, n: int, seed: int) -> list[BenchmarkPrompt]:
    if not path.exists():
        raise FileNotFoundError(
            f"JSONL fixture missing at {path}. "
            f"For PR 2 the edge-cases fixture must be hand-authored before the full run."
        )
    rows: list[dict] = []
    with path.open() as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            rows.append(json.loads(line))
    if n > len(rows):
        raise ValueError(f"requested {n} rows but {path} only has {len(rows)}")
    rng = random.Random(seed)
    rng.shuffle(rows)
    out: list[BenchmarkPrompt] = []
    for i, row in enumerate(rows[:n]):
        prompt_text = row["prompt_text"]
        ref_dict = row.get("reference") or {"kind": "none", "payload": {}}
        reference = Reference(**ref_dict)
        out.append(
            BenchmarkPrompt(
                prompt_id=stable_prompt_id(slice_name, i, prompt_text),
                slice=slice_name,
                source=source,
                prompt_text=prompt_text,
                reference=reference,
                metadata=row.get("metadata", {}),
            )
        )
    return out


@register
class JSONLLoader(BenchmarkLoader):
    """Generic JSONL loader. Configured at registration time? No — this
    class is wrapped by callers (see edge_cases.py) that supply the
    path. It exists in the registry as a sanity-checkable dispatcher."""

    name: ClassVar[str] = "jsonl"

    def load(self, n: int, seed: int) -> list[BenchmarkPrompt]:
        raise NotImplementedError(
            "JSONLLoader is not meant to be looked up by registry; call "
            "load_jsonl(path=...) from a wrapper loader instead."
        )


# Suppress an unused-import warning from the hashing helper — kept here
# so a contributor reading this file sees what stable_prompt_id does.
_ = hashlib
