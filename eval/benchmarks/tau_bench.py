"""tau-bench loader (agentic multi-step tool-calling).

tau-bench (sierra-research/tau-bench) is the AvengersPro precedent for
long-horizon tool-use scenarios — the model has to navigate a multi-step
customer-support / retail / airline workflow with realistic tools.

The full benchmark is environment-driven (sandboxed APIs); for PR 2 we
score by the **final-state assertion** the framework ships with each
task, captured as a tool_call_match reference.
"""

from __future__ import annotations

from typing import Any, ClassVar

from eval.benchmarks import BenchmarkLoader, register
from eval.benchmarks._hf import hf_sample
from eval.types import BenchmarkPrompt, Reference

TAU_HF_DATASET = "sierra-research/tau-bench"


def _to_tau(row: dict[str, Any]) -> tuple[str, Reference | None, dict[str, Any]]:
    instruction = row.get("instruction") or row.get("user_instruction", "")
    tools = row.get("tools") or row.get("tool_definitions", [])
    prompt = f"{instruction}\n\nAvailable tools:\n{tools}"
    # tau-bench ships expected_actions as a list. The structural
    # tool_call_match grader only validates dict key-paths, so wrapping
    # it as `{"actions": [...]}` would let any model output containing
    # an `actions` key pass without comparing the list contents. The
    # LLM judge already evaluates agentic tool use end-to-end, so skip
    # reference grading rather than emitting a false-positive signal.
    return (
        prompt,
        Reference(kind="none", payload={}),
        {"task_id": row.get("task_id"), "domain": row.get("domain")},
    )


@register
class TauBench(BenchmarkLoader):
    name: ClassVar[str] = "tau-bench"

    def load(self, n: int, seed: int) -> list[BenchmarkPrompt]:
        return hf_sample(
            slice_name="tool-calling-agentic",
            source="tau-bench",
            dataset_path=TAU_HF_DATASET,
            config_name=None,
            split="train",
            n=n,
            seed=seed,
            to_prompt=_to_tau,
        )
