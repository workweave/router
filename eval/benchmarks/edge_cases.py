"""Hand-curated edge cases (refusals, ambiguity, prompt-injection).

These are the cases the heuristic is most likely to mis-route on:

- short prompts that look trivial but actually need Opus-level reasoning
  ("Is P=NP?" — looks like 4 tokens, deserves Opus)
- long prompts that look hard but are Haiku-fine ("paste the dictionary
  and ask for word count")
- refusal prompts that should land at Sonnet (cheap, principled refusal)
- ambiguous prompts where any model is fine but the "obvious" choice
  burns budget

Loaded from a JSONL fixture committed alongside the harness so we own
the exact text. The fixture lives at
`router/eval/benchmarks/data/edge_cases.jsonl` and is editable by hand.
"""

from __future__ import annotations

from pathlib import Path
from typing import ClassVar

from eval.benchmarks import BenchmarkLoader, register
from eval.benchmarks.jsonl_loader import load_jsonl
from eval.types import BenchmarkPrompt

_FIXTURE = Path(__file__).resolve().parent / "data" / "edge_cases.jsonl"


@register
class EdgeCasesHandcurated(BenchmarkLoader):
    name: ClassVar[str] = "edge-cases-handcurated"

    def load(self, n: int, seed: int) -> list[BenchmarkPrompt]:
        return load_jsonl(_FIXTURE, slice_name="edge-cases", source="handcurated", n=n, seed=seed)
