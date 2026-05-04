"""LiveCodeBench loaders (Python + TS subset).

LiveCodeBench (livecodebench/LiveCodeBench) is a contamination-resistant
coding benchmark — problems collected after each model's training
cutoff. The Python split is the headline; we also pull a TS subset for
Claude Code skew. Reference grading uses the test cases bundled with
each problem.
"""

from __future__ import annotations

from typing import Any, ClassVar

from eval.benchmarks import BenchmarkLoader, register
from eval.benchmarks._hf import hf_sample
from eval.benchmarks.aider_polyglot import _aider_subset
from eval.types import BenchmarkPrompt, Reference


def _to_python(row: dict[str, Any]) -> tuple[str, Reference | None, dict[str, Any]]:
    prompt = row.get("question_content") or row.get("problem_description") or row.get("prompt", "")
    tests = row.get("test_list") or row.get("public_test_cases") or row.get("private_test_cases") or []
    reference = Reference(
        kind="code_tests",
        payload={"language": "python", "tests": tests if isinstance(tests, str) else _serialize_tests(tests)},
    )
    return prompt, reference, {"contest_id": row.get("contest_id"), "platform": row.get("platform")}


def _serialize_tests(tests: list[Any]) -> str:
    """Tests come as a list of {input, output} dicts; serialize to a
    pytest-style block so reference.grade can run them. Best-effort —
    the grader handles partial / missing tests by passing/failing as
    documented."""
    import json

    return json.dumps(tests, ensure_ascii=False)


@register
class LiveCodeBenchPython(BenchmarkLoader):
    name: ClassVar[str] = "livecodebench-python"

    def load(self, n: int, seed: int) -> list[BenchmarkPrompt]:
        return hf_sample(
            slice_name="coding-python",
            source="livecodebench",
            dataset_path="livecodebench/code_generation_lite",
            config_name=None,
            split="test",
            n=n,
            seed=seed,
            to_prompt=_to_python,
        )


@register
class LiveCodeBenchTSAndAiderPolyglotTS(BenchmarkLoader):
    """TypeScript coding slice — sourced entirely from Aider Polyglot TS.

    Loader name preserved for slice_plan compatibility. LiveCodeBench
    does not ship a TypeScript-tagged config, and its rows can't be
    filtered to TS reliably (the test harness is Python-shaped), so
    pulling from LCB risks contaminating coding-ts with non-TS tasks.
    Aider Polyglot has first-class per-language partitions and is the
    right source for this slice.
    """

    name: ClassVar[str] = "livecodebench-ts-and-aider-polyglot-ts"

    def load(self, n: int, seed: int) -> list[BenchmarkPrompt]:
        return _aider_subset("typescript", n=n, seed=seed, slice_name="coding-ts")
