"""MMLU loader (router-canon knowledge consensus)."""

from __future__ import annotations

from typing import Any, ClassVar

from eval.benchmarks import BenchmarkLoader, register
from eval.benchmarks._hf import hf_sample
from eval.types import BenchmarkPrompt, Reference


def _to_mmlu(row: dict[str, Any]) -> tuple[str, Reference | None, dict[str, Any]]:
    question = row.get("question", "")
    choices = row.get("choices", []) or [
        row.get("A", ""),
        row.get("B", ""),
        row.get("C", ""),
        row.get("D", ""),
    ]
    answer_idx = row.get("answer")
    correct_letter: str | None
    if isinstance(answer_idx, str) and len(answer_idx) == 1 and answer_idx in "ABCD":
        correct_letter = answer_idx
    elif isinstance(answer_idx, int) and 0 <= answer_idx < 4:
        correct_letter = "ABCD"[answer_idx]
    else:
        correct_letter = None
    formatted = "\n".join(f"{l}. {c}" for l, c in zip("ABCD", choices))
    prompt = f"{question}\n\n{formatted}\n\nAnswer with a single letter (A, B, C, or D)."
    # Skip reference grading for rows missing a parseable answer; the
    # LLM judge is still the authority and a "?" sentinel would just
    # produce false negatives in `numeric_match`.
    reference: Reference
    if correct_letter is None:
        reference = Reference(kind="none", payload={})
    else:
        reference = Reference(kind="numeric_match", payload={"answer": correct_letter})
    return prompt, reference, {"subject": row.get("subject")}


@register
class MMLU(BenchmarkLoader):
    """Pulls from MMLU's `all` split, sampled across subjects."""

    name: ClassVar[str] = "mmlu"

    def load(self, n: int, seed: int) -> list[BenchmarkPrompt]:
        return hf_sample(
            slice_name="knowledge-mmlu",
            source="mmlu",
            dataset_path="cais/mmlu",
            config_name="all",
            split="test",
            n=n,
            seed=seed,
            to_prompt=_to_mmlu,
        )
