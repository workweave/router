"""MT-Bench loader (router-canon chat / LLM-judge consensus).

LM-Sys MT-Bench (lmsys/mt_bench_human_judgments) contains 80 multi-turn
conversation prompts across 8 categories. We use the first turn only to
keep the request shape uniform with the rest of the eval set.
"""

from __future__ import annotations

import random
from typing import Any, ClassVar

from eval.benchmarks import BenchmarkLoader, register
from eval.benchmarks._hf import stable_prompt_id
from eval.types import BenchmarkPrompt, Reference


def _row_to_prompt(row: dict[str, Any]) -> tuple[str, str, dict[str, Any]]:
    """(prompt_text, question_id, metadata) extractor for an MT-Bench row."""
    turns = row.get("turns") or row.get("conversation_a", [])
    if isinstance(turns, list) and turns:
        first = turns[0]
        if isinstance(first, dict):
            prompt = first.get("content") or first.get("text", "")
        else:
            prompt = str(first)
    else:
        prompt = row.get("prompt", "")
    category = row.get("category") or row.get("question_id_category", "")
    question_id = str(row.get("question_id") or "")
    return prompt, question_id, {"category": category, "question_id": row.get("question_id")}


@register
class MTBench(BenchmarkLoader):
    name: ClassVar[str] = "mt-bench"

    def load(self, n: int, seed: int) -> list[BenchmarkPrompt]:
        # The HF dataset stores one row per human-judgment pair, so the
        # same MT-Bench question shows up many times. Dedup by
        # `question_id` before sampling so the slice is 80 distinct
        # questions, not 80 random rows from the same handful.
        from datasets import load_dataset  # type: ignore[import-untyped]

        ds = load_dataset(
            "lmsys/mt_bench_human_judgments",
            split="human",
            streaming=False,
        )
        seen: set[str] = set()
        unique: list[dict[str, Any]] = []
        for row in ds:
            qid = str(row.get("question_id") or "")
            if not qid or qid in seen:
                continue
            seen.add(qid)
            unique.append(row)

        if not unique:
            raise RuntimeError("MT-Bench dataset returned 0 unique questions")
        if n > len(unique):
            raise RuntimeError(
                f"requested {n} MT-Bench prompts but only {len(unique)} distinct questions exist"
            )

        rng = random.Random(seed)
        rng.shuffle(unique)
        chosen = unique[:n]

        out: list[BenchmarkPrompt] = []
        for i, row in enumerate(chosen):
            prompt_text, _qid, metadata = _row_to_prompt(row)
            out.append(
                BenchmarkPrompt(
                    prompt_id=stable_prompt_id("chat-mt-bench", i, prompt_text),
                    slice="chat-mt-bench",
                    source="mt-bench",
                    prompt_text=prompt_text,
                    reference=Reference(kind="none", payload={}),
                    metadata=metadata,
                )
            )
        return out
