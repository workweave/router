"""BenchmarkLoader interface + registry.

Each benchmark file declares a `@register` class implementing
BenchmarkLoader; the harness composes the 500-prompt eval set by
walking slice_plan.SLICES and looking up each loader by name.

The Phase 1b extension point is a new file in this directory: a real
Claude Code prompt loader (or a JSONL file loader, or anything else)
becomes one new BenchmarkLoader subclass — no changes to the harness.
"""

from __future__ import annotations

from abc import ABC, abstractmethod
from typing import ClassVar

from eval.types import BenchmarkPrompt


class BenchmarkLoader(ABC):
    """Loads a slice of N benchmark prompts."""

    name: ClassVar[str]

    @abstractmethod
    def load(self, n: int, seed: int) -> list[BenchmarkPrompt]:
        """Return exactly n prompts. Deterministic on (n, seed)."""


REGISTRY: dict[str, type[BenchmarkLoader]] = {}


def register(cls: type[BenchmarkLoader]) -> type[BenchmarkLoader]:
    """Class decorator: registers the loader under cls.name."""
    if not getattr(cls, "name", None):
        raise ValueError(f"{cls.__name__} must set ClassVar 'name'")
    if cls.name in REGISTRY:
        raise ValueError(f"BenchmarkLoader name collision: {cls.name}")
    REGISTRY[cls.name] = cls
    return cls


def get(name: str) -> BenchmarkLoader:
    """Look up a loader by name and instantiate it. Raises if missing."""
    if name not in REGISTRY:
        raise KeyError(f"unknown BenchmarkLoader: {name!r}; registered={sorted(REGISTRY)}")
    return REGISTRY[name]()


# Side-effect imports register every shipped loader. Adding a new loader
# is one new import line plus a new file.
from eval.benchmarks import (  # noqa: E402, F401
    aider_polyglot,
    bfcl_v4,
    bird_sql,
    edge_cases,
    gpqa_diamond,
    gsm8k,
    humaneval_mbpp,
    jsonl_loader,
    livecodebench,
    mmlu,
    mt_bench,
    multilingual,
    swebench,
    tau_bench,
    xsum,
)
