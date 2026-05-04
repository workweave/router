"""Berkeley Function-Calling Leaderboard v4 loaders.

BFCL v4 (gorilla-llm/Berkeley-Function-Calling-Leaderboard) is the best
public tool-calling eval: simple single-call, parallel, multi-turn,
multi-step, and missing-function variants. We use the simple subset and
the parallel/multi-turn subset as separate slices.

Reference grading: BFCL v4 ships expected tool calls per row;
reference.grade compares structurally (tool name + arg-shape, tolerant
of value formatting).
"""

from __future__ import annotations

from typing import Any, ClassVar

from eval.benchmarks import BenchmarkLoader, register
from eval.benchmarks._hf import hf_sample
from eval.types import BenchmarkPrompt, Reference

BFCL_HF_DATASET = "gorilla-llm/Berkeley-Function-Calling-Leaderboard"


def _to_bfcl(row: dict[str, Any]) -> tuple[str, Reference | None, dict[str, Any]]:
    # BFCL rows include "question" + "function" (the available tool
    # schema). The model under test sees both. The "answer" field
    # holds the expected call shape.
    question = row.get("question", "")
    funcs = row.get("function", [])
    prompt = f"{question}\n\nAvailable tools (JSON schema):\n{funcs}"
    expected = row.get("answer", {})
    # `tool_call_match` only knows how to compare dict-shaped expecteds.
    # Some BFCL rows ship a list (parallel calls) or scalar answer; the
    # old `{"raw": str(expected)}` wrapping made the structural grader
    # check for a literal `"raw"` key in the model output and would
    # always mark valid tool-call responses as failures. Fall back to
    # `kind="none"` so the LLM judge stays the authority for those rows.
    reference: Reference
    if isinstance(expected, dict) and expected:
        reference = Reference(kind="tool_call_match", payload={"expected": expected})
    else:
        reference = Reference(kind="none", payload={})
    return prompt, reference, {"id": row.get("id"), "category": row.get("category")}


@register
class BFCLV4Simple(BenchmarkLoader):
    name: ClassVar[str] = "bfcl-v4-simple"

    def load(self, n: int, seed: int) -> list[BenchmarkPrompt]:
        return hf_sample(
            slice_name="tool-calling-single",
            source="bfcl-v4-simple",
            dataset_path=BFCL_HF_DATASET,
            config_name="simple",
            split="train",
            n=n,
            seed=seed,
            to_prompt=_to_bfcl,
        )


@register
class BFCLV4ParallelMulti(BenchmarkLoader):
    name: ClassVar[str] = "bfcl-v4-parallel-multi"

    def load(self, n: int, seed: int) -> list[BenchmarkPrompt]:
        # Pull from parallel + multiple_function configs and merge.
        per = (n + 1) // 2
        a = hf_sample(
            slice_name="tool-calling-parallel-multi",
            source="bfcl-v4-parallel",
            dataset_path=BFCL_HF_DATASET,
            config_name="parallel",
            split="train",
            n=per,
            seed=seed,
            to_prompt=_to_bfcl,
        )
        b = hf_sample(
            slice_name="tool-calling-parallel-multi",
            source="bfcl-v4-multiple-function",
            dataset_path=BFCL_HF_DATASET,
            config_name="multiple_function",
            split="train",
            n=n - len(a),
            seed=seed + 1,
            to_prompt=_to_bfcl,
        )
        return (a + b)[:n]
