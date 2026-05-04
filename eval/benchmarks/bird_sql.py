"""BIRD-SQL loader (realistic schema-grounded SQL)."""

from __future__ import annotations

from typing import Any, ClassVar

from eval.benchmarks import BenchmarkLoader, register
from eval.benchmarks._hf import hf_sample
from eval.types import BenchmarkPrompt, Reference


def _to_bird(row: dict[str, Any]) -> tuple[str, Reference | None, dict[str, Any]]:
    question = row.get("question", "")
    schema = row.get("schema") or row.get("db_schema", "")
    evidence = row.get("evidence", "")
    prompt = (
        f"Schema:\n{schema}\n\n"
        f"Domain hints: {evidence}\n\n"
        f"Question: {question}\n\n"
        f"Return ONLY the final SQL query (no markdown fences)."
    )
    sql = row.get("SQL") or row.get("ground_truth_sql", "")
    reference = Reference(kind="numeric_match", payload={"answer": sql})
    return prompt, reference, {"db_id": row.get("db_id")}


@register
class BirdSQL(BenchmarkLoader):
    name: ClassVar[str] = "bird-sql"

    def load(self, n: int, seed: int) -> list[BenchmarkPrompt]:
        return hf_sample(
            slice_name="coding-sql",
            source="bird-sql",
            dataset_path="xlangai/BIRD",
            config_name=None,
            split="train",
            n=n,
            seed=seed,
            to_prompt=_to_bird,
        )
