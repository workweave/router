"""SWE-bench Verified loader (princeton-nlp/SWE-bench_Verified).

500 hand-validated GitHub-issue → patch tasks. The official benchmark
scores models by **executing** their proposed patches against the
project's test suite inside per-instance docker images and counting
FAIL_TO_PASS / PASS_TO_PASS transitions. **That execution path is not
available in this harness** — `compare.py` is single-shot
prompt → response → LLM-judge pairwise. So this loader gives a
*directional* SWE-bench-style coding signal: judges grade the proposed
patch's plausibility against the rubric (and the gold patch carried in
``metadata.gold_patch``), not whether the patch actually passes tests.

For real pass-rate scoring use the upstream `swebench` CLI / docker
harness; this loader is only useful for "does the router pick a model
whose patch the LLM-judge ensemble prefers on SWE-bench Verified
instances?"

Prompt shape: repo + base commit + problem statement + (optional)
hints, asking for a unified-diff patch. The gold patch lives in
``metadata.gold_patch`` so a future judge upgrade can include it as
ground truth in the rubric prompt without changing the loader.
"""

from __future__ import annotations

from typing import Any, ClassVar

from eval.benchmarks import BenchmarkLoader, register
from eval.benchmarks._hf import hf_sample
from eval.types import BenchmarkPrompt, Reference


SWE_BENCH_HF_DATASET = "princeton-nlp/SWE-bench_Verified"


def _to_prompt(row: dict[str, Any]) -> tuple[str, Reference | None, dict[str, Any]]:
    repo = row.get("repo", "")
    base_commit = row.get("base_commit", "")
    problem_statement = row.get("problem_statement", "")
    hints_text = (row.get("hints_text") or "").strip()
    instance_id = row.get("instance_id", "")

    # Single-shot prompt: no repo browsing, no test execution. Asks for
    # a unified-diff patch so the judge has a structurally-comparable
    # output across models.
    parts = [
        f"You are looking at the `{repo}` repository at commit `{base_commit}`.",
        "",
        "Issue:",
        problem_statement.strip(),
    ]
    if hints_text:
        parts.extend(["", "Hints:", hints_text])
    parts.extend([
        "",
        "Propose a patch that fixes this issue. Output the patch in unified "
        "diff format (`diff --git a/... b/...`), with no surrounding prose. "
        "Only modify files necessary to fix the issue.",
    ])
    prompt_text = "\n".join(parts)

    # Reference.kind="none" — the harness's reference graders
    # (code_tests, numeric_match, tool_call_match) can't execute
    # SWE-bench's test transitions. The gold patch is parked on metadata
    # so a future rubric/judge change can promote it without breaking
    # the loader.
    reference = Reference(kind="none", payload={})
    metadata = {
        "instance_id": instance_id,
        "repo": repo,
        "base_commit": base_commit,
        "gold_patch": row.get("patch", ""),
        "test_patch": row.get("test_patch", ""),
        "fail_to_pass": row.get("FAIL_TO_PASS", ""),
        "pass_to_pass": row.get("PASS_TO_PASS", ""),
    }
    return prompt_text, reference, metadata


@register
class SWEBenchVerified(BenchmarkLoader):
    name: ClassVar[str] = "swebench-verified"

    def load(self, n: int, seed: int) -> list[BenchmarkPrompt]:
        return hf_sample(
            slice_name="swebench-verified",
            source="swebench-verified",
            dataset_path=SWE_BENCH_HF_DATASET,
            config_name=None,
            split="test",
            n=n,
            seed=seed,
            to_prompt=_to_prompt,
        )
