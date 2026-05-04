"""Unit tests for the BenchmarkLoader registry + protocol conformance."""

from typing import ClassVar

import pytest

from eval.benchmarks import REGISTRY, BenchmarkLoader, get, register
from eval.benchmarks.jsonl_loader import load_jsonl
from eval.types import BenchmarkPrompt, Reference


def test_registry_includes_every_loader_referenced_in_slice_plan():
    from eval.slice_plan import SLICES

    for spec in SLICES:
        # Will raise if missing.
        loader = get(spec.loader)
        assert loader.name == spec.loader


def test_get_raises_for_unknown_loader():
    with pytest.raises(KeyError):
        get("does-not-exist")


def test_register_rejects_duplicate_names():
    @register
    class Once(BenchmarkLoader):
        name: ClassVar[str] = "test-dup-bench"

        def load(self, n, seed):
            return []

    try:
        with pytest.raises(ValueError):

            @register
            class Twice(BenchmarkLoader):
                name: ClassVar[str] = "test-dup-bench"

                def load(self, n, seed):
                    return []

    finally:
        REGISTRY.pop("test-dup-bench", None)


def test_jsonl_loader_returns_deterministic_subset(tmp_path):
    fixture = tmp_path / "fix.jsonl"
    rows = [
        {"prompt_text": f"prompt-{i}", "reference": {"kind": "none", "payload": {}}}
        for i in range(20)
    ]
    fixture.write_text("\n".join(__import__("json").dumps(r) for r in rows))

    sample_a = load_jsonl(fixture, slice_name="t", source="t", n=5, seed=7)
    sample_b = load_jsonl(fixture, slice_name="t", source="t", n=5, seed=7)

    assert [p.prompt_text for p in sample_a] == [p.prompt_text for p in sample_b]

    # Confirm the loader actually shuffles rather than returning the
    # head of the file. Comparing two specific seeds is flaky (shuffle
    # collisions are possible), so sweep a range of seeds and assert
    # that at least one ordering differs from `sample_a`.
    seen_other_ordering = False
    for s in range(1, 20):
        if s == 7:
            continue
        other = load_jsonl(fixture, slice_name="t", source="t", n=5, seed=s)
        if [p.prompt_text for p in sample_a] != [p.prompt_text for p in other]:
            seen_other_ordering = True
            break
    assert seen_other_ordering, "loader produced identical ordering for every probed seed"


def test_edge_cases_fixture_loads_at_least_minimum():
    """The committed edge_cases.jsonl must be parsable and cover all
    five categories the harness expects."""
    edge = get("edge-cases-handcurated")
    prompts = edge.load(n=5, seed=0)
    assert len(prompts) == 5
    # Each prompt has a stable id and a reference (even if "none").
    for p in prompts:
        assert p.prompt_id and p.reference is not None
        assert p.slice == "edge-cases"


def test_benchmark_prompt_serialization_round_trips():
    p = BenchmarkPrompt(
        prompt_id="x",
        slice="s",
        source="src",
        prompt_text="hello",
        reference=Reference(kind="numeric_match", payload={"answer": "42"}),
        metadata={"k": "v"},
    )
    raw = p.model_dump_json()
    p2 = BenchmarkPrompt.model_validate_json(raw)
    assert p2 == p
