"""Shared helpers for HuggingFace-backed BenchmarkLoaders.

Every loader that pulls from a HF dataset goes through `hf_sample` so the
sampling is identical across loaders (deterministic on `seed`,
deterministic per-prompt `prompt_id`).
"""

from __future__ import annotations

import hashlib
import random
from typing import Any, Callable

from eval.types import BenchmarkPrompt, Reference


def hf_sample(
    *,
    slice_name: str,
    source: str,
    dataset_path: str,
    config_name: str | None,
    split: str,
    n: int,
    seed: int,
    to_prompt: Callable[[dict[str, Any]], tuple[str, Reference | None, dict[str, Any]]],
    streaming: bool = False,
    trust_remote_code: bool = False,
) -> list[BenchmarkPrompt]:
    """Deterministically sample n rows from a HF dataset and adapt them.

    `to_prompt(row) -> (prompt_text, reference, metadata)` performs the
    per-dataset shape conversion. Imported lazily so unit tests don't
    drag in the `datasets` dep just to inspect the registry. Pass
    trust_remote_code=True for datasets that ship custom loader scripts;
    HF's interactive consent prompt arms a SIGALRM that, if not
    cancelled by accepting, fires later and crashes the asyncio loop.
    """
    from datasets import load_dataset  # type: ignore[import-untyped]

    ds = load_dataset(
        dataset_path, config_name, split=split, streaming=streaming, trust_remote_code=trust_remote_code,
    )
    rows: list[dict[str, Any]]
    if streaming:
        # Materialize a bounded prefix; HF streaming + reservoir would
        # also work, but a 10x oversample is fine for the slice sizes
        # we care about and avoids the full download.
        rows = []
        for i, row in enumerate(ds):
            rows.append(row)
            if i >= max(10 * n, 200):
                break
    else:
        rows = list(ds)

    if not rows:
        raise RuntimeError(f"HF dataset {dataset_path}/{config_name}/{split} returned 0 rows")

    rng = random.Random(seed)
    rng.shuffle(rows)
    if n > len(rows):
        raise RuntimeError(
            f"requested {n} prompts but dataset only has {len(rows)} rows in {dataset_path}/{config_name}/{split}"
        )
    chosen = rows[:n]

    out: list[BenchmarkPrompt] = []
    for i, row in enumerate(chosen):
        prompt_text, reference, metadata = to_prompt(row)
        out.append(
            BenchmarkPrompt(
                prompt_id=stable_prompt_id(slice_name, i, prompt_text),
                slice=slice_name,
                source=source,
                prompt_text=prompt_text,
                reference=reference,
                metadata=metadata,
            )
        )
    return out


def stable_prompt_id(slice_name: str, index: int, prompt_text: str) -> str:
    """Deterministic ID: slice + index + first-12 of SHA-256 of text.

    Including a content hash keeps two prompts in different slices
    distinguishable even if the text happens to coincide, and lets us
    detect prompt-set drift between runs.
    """
    h = hashlib.sha256(prompt_text.encode("utf-8")).hexdigest()[:12]
    return f"{slice_name}-{index:03d}-{h}"
