"""Multilingual coding loader.

Translated subset of HumanEval — humaneval-x covers Python / Java /
JavaScript / C++ / Go. Using a translated coding eval is the
sanity-coverage choice because it reuses a contamination-resistant
benchmark and exercises both the model's coding and its multilingual
comprehension.
"""

from __future__ import annotations

from typing import Any, ClassVar

from eval.benchmarks import BenchmarkLoader, register
from eval.benchmarks._hf import hf_sample
from eval.types import BenchmarkPrompt, Reference


def _to_humaneval_x(row: dict[str, Any]) -> tuple[str, Reference | None, dict[str, Any]]:
    prompt = row.get("prompt", "")
    test = row.get("test", "")
    language = row.get("language", "")
    reference = Reference(
        kind="code_tests",
        payload={"language": language, "tests": test, "entry_point": row.get("entry_point", "")},
    )
    return prompt, reference, {"task_id": row.get("task_id"), "language": language}


@register
class MultilingualCoding(BenchmarkLoader):
    """Round-robin across humaneval-x languages."""

    name: ClassVar[str] = "multilingual-coding"

    def load(self, n: int, seed: int) -> list[BenchmarkPrompt]:
        # THUDM/humaneval-x ships configs for Python / Java / JS / C++ /
        # Go only; "rust" is not a published config and would 404 the
        # HF loader.
        languages = ["python", "java", "js", "cpp", "go"]
        per = n // len(languages)
        rem = n - per * len(languages)
        out: list[BenchmarkPrompt] = []
        for i, lang in enumerate(languages):
            count = per + (1 if i < rem else 0)
            if count == 0:
                continue
            out += hf_sample(
                slice_name="multilingual",
                source=f"humaneval-x-{lang}",
                dataset_path="THUDM/humaneval-x",
                config_name=lang,
                split="test",
                n=count,
                seed=seed + i,
                to_prompt=_to_humaneval_x,
                trust_remote_code=True,
            )
        return out[:n]
