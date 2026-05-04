"""Aider Polyglot loaders (Go / TS / Rust / C++ / Java).

Aider Polyglot (https://github.com/Aider-AI/polyglot-benchmark) is the
canonical multi-language coding eval. It's distributed as a git repo of
exercises with hidden tests. The harness expects the exercises mirrored
to a HF dataset (one row per exercise, language tag, hidden tests
embedded). If the dataset is unavailable, swap in a JSONL via
jsonl_loader.

Prompt shape: a markdown problem statement + the file(s) the model
should edit. Reference grading runs the hidden tests in a sandbox.
"""

from __future__ import annotations

from typing import Any, ClassVar

from eval.benchmarks import BenchmarkLoader, register
from eval.benchmarks._hf import hf_sample
from eval.types import BenchmarkPrompt, Reference

# A community mirror of the Aider Polyglot benchmark on HF. Swap to a
# JSONL fixture (jsonl_loader.JSONLLoader) if this dataset moves.
AIDER_HF_DATASET = "Aider-AI/polyglot-benchmark"


def _to_aider(language: str):
    def adapter(row: dict[str, Any]) -> tuple[str, Reference | None, dict[str, Any]]:
        prompt = row.get("instructions") or row.get("prompt") or row.get("description", "")
        starter = row.get("starter_code") or row.get("solution_template") or ""
        full_prompt = prompt
        if starter:
            full_prompt = f"{prompt}\n\n--- starter file ---\n{starter}"
        tests = row.get("tests") or row.get("hidden_tests") or ""
        reference = Reference(
            kind="code_tests",
            payload={"language": language, "tests": tests if isinstance(tests, str) else str(tests)},
        )
        return full_prompt, reference, {"exercise": row.get("exercise"), "language": language}

    return adapter


def _aider_subset(language: str, *, n: int, seed: int, slice_name: str) -> list[BenchmarkPrompt]:
    """Pull n exercises filtered to a single language."""
    return hf_sample(
        slice_name=slice_name,
        source=f"aider-polyglot-{language}",
        dataset_path=AIDER_HF_DATASET,
        config_name=language,
        split="train",
        n=n,
        seed=seed,
        to_prompt=_to_aider(language),
    )


@register
class AiderPolyglotGo(BenchmarkLoader):
    name: ClassVar[str] = "aider-polyglot-go"

    def load(self, n: int, seed: int) -> list[BenchmarkPrompt]:
        return _aider_subset("go", n=n, seed=seed, slice_name="coding-go")


@register
class AiderPolyglotRustCppJava(BenchmarkLoader):
    """Pulls roughly equal counts from rust / cpp / java; sums to n."""

    name: ClassVar[str] = "aider-polyglot-rust-cpp-java"

    def load(self, n: int, seed: int) -> list[BenchmarkPrompt]:
        per = n // 3
        rem = n - 3 * per
        out: list[BenchmarkPrompt] = []
        out += _aider_subset("rust", n=per + (1 if rem >= 1 else 0), seed=seed, slice_name="coding-rust-cpp-java")
        out += _aider_subset("cpp", n=per + (1 if rem >= 2 else 0), seed=seed + 1, slice_name="coding-rust-cpp-java")
        out += _aider_subset("java", n=per, seed=seed + 2, slice_name="coding-rust-cpp-java")
        return out
