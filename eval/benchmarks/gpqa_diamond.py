"""GPQA-Diamond loader.

Idavidrein/gpqa with the "gpqa_diamond" config — the highest-difficulty
graduate-level science QA subset. Multiple-choice; reference grades by
matching the model's chosen letter to the ground-truth answer.
"""

from __future__ import annotations

import hashlib
import random
from typing import Any, ClassVar

from eval.benchmarks import BenchmarkLoader, register
from eval.benchmarks._hf import hf_sample
from eval.types import BenchmarkPrompt, Reference


def _to_gpqa(row: dict[str, Any]) -> tuple[str, Reference | None, dict[str, Any]]:
    question = row.get("Question", "")
    correct = row.get("Correct Answer", "")
    incorrects = [
        row.get("Incorrect Answer 1", ""),
        row.get("Incorrect Answer 2", ""),
        row.get("Incorrect Answer 3", ""),
    ]
    # Shuffle deterministically by the question text so two runs see
    # the same option ordering (avoids judge-side position bias on
    # already-published GPQA answers). `hash()` is salted per Python
    # process; use a stable digest so reruns share the same ordering.
    digest = hashlib.sha256(question.encode("utf-8")).digest()
    rng = random.Random(int.from_bytes(digest[:8], "big"))
    options = [correct] + incorrects
    rng.shuffle(options)
    correct_letter = "ABCD"[options.index(correct)]
    formatted = "\n".join(f"{l}. {opt}" for l, opt in zip("ABCD", options))
    prompt = f"{question}\n\n{formatted}\n\nAnswer with a single letter (A, B, C, or D)."
    reference = Reference(kind="numeric_match", payload={"answer": correct_letter})
    return prompt, reference, {"domain": row.get("High-level domain"), "subdomain": row.get("Subdomain")}


@register
class GPQADiamond(BenchmarkLoader):
    name: ClassVar[str] = "gpqa-diamond"

    def load(self, n: int, seed: int) -> list[BenchmarkPrompt]:
        return hf_sample(
            slice_name="math-gpqa",
            source="gpqa-diamond",
            dataset_path="Idavidrein/gpqa",
            config_name="gpqa_diamond",
            split="train",
            n=n,
            seed=seed,
            to_prompt=_to_gpqa,
        )
