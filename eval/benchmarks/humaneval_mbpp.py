"""HumanEval + MBPP loader (router-canon coding consensus).

Half-and-half from openai_humaneval and google-research-datasets/mbpp.
For a 25-prompt slice this is ~12 + ~13. Lock the exact split during
scaffolding by adjusting `_humaneval_share`.
"""

from __future__ import annotations

from typing import Any, ClassVar

from eval.benchmarks import BenchmarkLoader, register
from eval.benchmarks._hf import hf_sample
from eval.types import BenchmarkPrompt, Reference

_humaneval_share = 0.48  # ~12/25 → HumanEval; rest → MBPP


def _to_humaneval(row: dict[str, Any]) -> tuple[str, Reference | None, dict[str, Any]]:
    prompt = row.get("prompt", "")
    test = row.get("test", "")
    entry_point = row.get("entry_point", "")
    reference = Reference(
        kind="code_tests",
        payload={"language": "python", "tests": test, "entry_point": entry_point},
    )
    return prompt, reference, {"task_id": row.get("task_id")}


def _to_mbpp(row: dict[str, Any]) -> tuple[str, Reference | None, dict[str, Any]]:
    text = row.get("text") or row.get("prompt", "")
    test_list = row.get("test_list", [])
    reference = Reference(
        kind="code_tests",
        payload={"language": "python", "tests": "\n".join(test_list)},
    )
    return text, reference, {"task_id": row.get("task_id")}


@register
class HumanEvalMBPP(BenchmarkLoader):
    name: ClassVar[str] = "humaneval-mbpp"

    def load(self, n: int, seed: int) -> list[BenchmarkPrompt]:
        n_humaneval = round(n * _humaneval_share)
        n_mbpp = n - n_humaneval
        humaneval = hf_sample(
            slice_name="coding-humaneval-mbpp",
            source="openai_humaneval",
            dataset_path="openai/openai_humaneval",
            config_name=None,
            split="test",
            n=n_humaneval,
            seed=seed,
            to_prompt=_to_humaneval,
        )
        mbpp = hf_sample(
            slice_name="coding-humaneval-mbpp",
            source="mbpp",
            dataset_path="google-research-datasets/mbpp",
            config_name="full",
            split="test",
            n=n_mbpp,
            seed=seed + 1,
            to_prompt=_to_mbpp,
        )
        return (humaneval + mbpp)[:n]
