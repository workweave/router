"""XSum summarization loader (default summarization slice).

Alternates: CNN/DailyMail (longer sources) or GovReport (multi-page).
Pick during scaffolding; XSum is the default for its tight rubric fit
(one-sentence summaries are easy to judge consistently).
"""

from __future__ import annotations

from typing import Any, ClassVar

from eval.benchmarks import BenchmarkLoader, register
from eval.benchmarks._hf import hf_sample
from eval.types import BenchmarkPrompt, Reference


def _to_xsum(row: dict[str, Any]) -> tuple[str, Reference | None, dict[str, Any]]:
    document = row.get("document", "")
    summary = row.get("summary", "")
    prompt = f"Summarize the following article in one sentence:\n\n{document}"
    # Reference summaries are short but rarely the only correct
    # answer; LLM-judge is the headline grade for this slice.
    return prompt, Reference(kind="none", payload={"reference_summary": summary}), {}


@register
class XSum(BenchmarkLoader):
    name: ClassVar[str] = "xsum"

    def load(self, n: int, seed: int) -> list[BenchmarkPrompt]:
        return hf_sample(
            slice_name="summarization",
            source="xsum",
            dataset_path="EdinburghNLP/xsum",
            config_name=None,
            split="test",
            n=n,
            seed=seed,
            to_prompt=_to_xsum,
        )
