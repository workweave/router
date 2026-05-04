"""Tests for ingest.model_aliases.

Real assertions only: every test would fail if the production alias
dicts were emptied or the resolver swapped sources. Spot-check the
specific keys we depend on at v0.3 — the aliases that bring direct
labels to ``claude-sonnet-4-5``, ``claude-haiku-4-5``,
``claude-opus-4-5`` (proxy for opus-4-7), and ``gemini-3-pro-preview``.
"""

from __future__ import annotations

import pytest

from ingest import model_aliases as aliases


def test_swebench_collapses_gpt_5_mini_to_gpt_5() -> None:
    """gpt-5-mini submissions are collapsed onto gpt-5's bench-column
    so they reinforce the gpt-5 ranking row rather than producing a
    column the registry can't reference. If this collapse is lost,
    train_cluster_router emits an empty cell for gpt-5-mini and the
    registry's gpt-5 column gets noisier coverage."""
    assert aliases.resolve("gpt-5-mini", "swebench") == "gpt-5"


def test_swebench_direct_columns_required_for_v03() -> None:
    """The four bench-columns v0.3 promotes from proxy to direct must
    each have at least one SWE-bench-side alias resolving to them."""
    direct = {
        "claude-sonnet-4-5",
        "claude-haiku-4-5",
        "claude-opus-4-5",
        "gemini-3-pro-preview",
    }
    resolved = {
        aliases.resolve(name, "swebench") for name in aliases.SWEBENCH_ALIASES
    }
    missing = direct - resolved
    assert not missing, f"SWEBENCH_ALIASES has no entry resolving to {missing}"


def test_bfcl_known_fc_variants_resolve() -> None:
    """The pinned BFCL-Result FC keys we rely on for v0.3 direct
    labels. If any of these stop matching, ingestion falls back to
    'unmapped' and the v0.3 deployed-models gain no direct labels."""
    expected = {
        "claude-sonnet-4-5-20250929-FC": "claude-sonnet-4-5",
        "claude-haiku-4-5-20251001-FC": "claude-haiku-4-5",
        "claude-opus-4-5-20251101-FC": "claude-opus-4-5",
        "gemini-3-pro-preview-FC": "gemini-3-pro-preview",
    }
    for source_name, bench_column in expected.items():
        assert aliases.resolve(source_name, "bfcl") == bench_column


def test_unknown_alias_returns_none() -> None:
    """Unknown source-side names must surface as None so the caller
    can log + skip rather than silently mismapping."""
    assert aliases.resolve("frobnicate-v9", "swebench") is None
    assert aliases.resolve("frobnicate-v9-FC", "bfcl") is None


def test_unknown_source_raises() -> None:
    """Catch typos in caller-side source identifiers up front."""
    with pytest.raises(ValueError):
        aliases.resolve("anything", "openrouterbench")


def test_swebench_and_bfcl_keys_are_disjoint() -> None:
    """SWE-bench keys are bare model names; BFCL keys carry date-stamp
    + ``-FC`` suffixes. If the namespaces ever collide we'd be
    silently routing the wrong source through the wrong dict at
    ``resolve()`` time."""
    overlap = set(aliases.SWEBENCH_ALIASES) & set(aliases.BFCL_ALIASES)
    assert not overlap, f"alias namespaces overlap: {overlap}"
