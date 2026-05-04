"""GSM8K loader (router-canon math consensus)."""

from __future__ import annotations

import re
from typing import Any, ClassVar

from eval.benchmarks import BenchmarkLoader, register
from eval.benchmarks._hf import hf_sample
from eval.types import BenchmarkPrompt, Reference

# GSM8K answers are formatted as "<CoT>\n#### <number>".
_ANSWER_PATTERN = re.compile(r"####\s*(-?[\d.,]+)")


def _to_gsm8k(row: dict[str, Any]) -> tuple[str, Reference | None, dict[str, Any]]:
    question = row.get("question", "")
    raw_answer = row.get("answer", "")
    m = _ANSWER_PATTERN.search(raw_answer)
    answer = m.group(1).replace(",", "") if m else raw_answer.strip()
    prompt = f"{question}\n\nReason step by step, then end your answer with `#### <number>`."
    reference = Reference(kind="numeric_match", payload={"answer": answer})
    return prompt, reference, {}


@register
class GSM8K(BenchmarkLoader):
    name: ClassVar[str] = "gsm8k"

    def load(self, n: int, seed: int) -> list[BenchmarkPrompt]:
        return hf_sample(
            slice_name="math-gsm8k",
            source="gsm8k",
            dataset_path="openai/gsm8k",
            config_name="main",
            split="test",
            n=n,
            seed=seed,
            to_prompt=_to_gsm8k,
        )
