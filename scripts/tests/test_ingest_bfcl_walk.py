"""Tests for ingest.bfcl._walk_drop.

Fixture-driven; no networking. Builds a fake
``<drop>/score/<model>/`` and ``<drop>/result/<model>/`` tree in tmpdir,
runs the walker, asserts on the emitted nested layout
(``<bench-name>/<category>/<bench_column>/<drop>.json``) and content.
"""

from __future__ import annotations

import json
import logging
from pathlib import Path

from ingest import bfcl


def _write_jsonl(path: Path, rows: list[dict]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("\n".join(json.dumps(r) for r in rows) + "\n", encoding="utf-8")


def _scaffold_drop(
    drop_dir: Path,
    *,
    model: str,
    category: str,
    score_rows: list[dict],
    result_rows: list[dict],
) -> None:
    score_path = drop_dir / "score" / model / f"BFCL_v3_{category}_score.json"
    result_path = drop_dir / "result" / model / f"BFCL_v3_{category}_result.json"
    _write_jsonl(score_path, score_rows)
    _write_jsonl(result_path, result_rows)


def test_walk_emits_nested_layout_per_category_and_model(tmp_path: Path) -> None:
    drop_dir = tmp_path / "2025-12-16"
    _scaffold_drop(
        drop_dir,
        model="claude-sonnet-4-5-20250929-FC",
        category="simple",
        score_rows=[
            {"accuracy": 0.5, "total_count": 2},  # summary header (no id)
            {"id": "simple_0", "valid": True},
            {"id": "simple_1", "valid": False},
        ],
        result_rows=[
            {"id": "simple_0", "question": "What is 1 + 1?"},
            {"id": "simple_1", "question": [{"role": "user", "content": "Tell me about cats."}]},
        ],
    )

    out_root = tmp_path / "bench-release" / "bfcl-v4"
    summary = bfcl._walk_drop(
        drop_dir=drop_dir,
        out_root=out_root,
        drop_label=drop_dir.name,
    )

    assert summary.mapped_models == 1
    assert summary.files_emitted == 1
    assert summary.total_records == 2

    out_path = out_root / "simple" / "claude-sonnet-4-5" / "2025-12-16.json"
    assert out_path.is_file()
    doc = json.loads(out_path.read_text())
    assert doc["model_name"] == "claude-sonnet-4-5"
    assert doc["dataset_name"] == "bfcl-v4-simple"
    assert doc["counts"] == 2
    by_id = {r["test_id"]: r for r in doc["records"]}
    assert by_id["simple_0"]["score"] == 1.0
    assert by_id["simple_1"]["score"] == 0.0
    # Confirm the chat-message-list prompt was flattened to a single
    # string — otherwise the embedder would receive a Python list and
    # the json.dumps would emit garbage tokens.
    assert by_id["simple_1"]["prompt"] == "Tell me about cats."


def test_walk_skips_non_fc_models(tmp_path: Path) -> None:
    drop_dir = tmp_path / "2025-12-16"
    _scaffold_drop(
        drop_dir,
        model="claude-sonnet-4-5-20250929",  # non-FC variant — must be skipped
        category="simple",
        score_rows=[{"id": "simple_0", "valid": True}],
        result_rows=[{"id": "simple_0", "question": "anything"}],
    )

    out_root = tmp_path / "out"
    summary = bfcl._walk_drop(drop_dir=drop_dir, out_root=out_root, drop_label="2025-12-16")
    assert summary.mapped_models == 0
    assert summary.skipped_non_fc == 1
    assert summary.files_emitted == 0
    assert not out_root.exists() or not any(out_root.rglob("*.json"))


def test_walk_logs_unmapped_fc_models(tmp_path: Path, caplog) -> None:
    drop_dir = tmp_path / "2025-12-16"
    _scaffold_drop(
        drop_dir,
        model="frobnicate-v9-FC",
        category="simple",
        score_rows=[{"id": "simple_0", "valid": True}],
        result_rows=[{"id": "simple_0", "question": "anything"}],
    )

    out_root = tmp_path / "out"
    with caplog.at_level(logging.WARNING, logger="ingest.bfcl"):
        summary = bfcl._walk_drop(
            drop_dir=drop_dir, out_root=out_root, drop_label="2025-12-16"
        )

    assert summary.unmapped_models == 1
    assert summary.mapped_models == 0
    assert any(
        'unmapped model "frobnicate-v9-FC"' in rec.message for rec in caplog.records
    )


def test_walk_handles_multiple_categories_for_one_model(tmp_path: Path) -> None:
    drop_dir = tmp_path / "2025-12-16"
    _scaffold_drop(
        drop_dir,
        model="claude-haiku-4-5-20251001-FC",
        category="simple",
        score_rows=[{"id": "simple_0", "valid": True}],
        result_rows=[{"id": "simple_0", "question": "q1"}],
    )
    _scaffold_drop(
        drop_dir,
        model="claude-haiku-4-5-20251001-FC",
        category="multi_turn",
        score_rows=[{"id": "mt_0", "valid": False}],
        result_rows=[
            {
                "id": "mt_0",
                # Multi-turn nests turns as a list-of-lists; the
                # walker must flatten one level.
                "question": [
                    [{"role": "user", "content": "first turn"}],
                    [{"role": "user", "content": "second turn"}],
                ],
            }
        ],
    )

    out_root = tmp_path / "out"
    summary = bfcl._walk_drop(
        drop_dir=drop_dir, out_root=out_root, drop_label="2025-12-16"
    )
    assert summary.files_emitted == 2

    simple = out_root / "simple" / "claude-haiku-4-5" / "2025-12-16.json"
    multi = out_root / "multi_turn" / "claude-haiku-4-5" / "2025-12-16.json"
    assert simple.is_file() and multi.is_file()

    multi_doc = json.loads(multi.read_text())
    assert multi_doc["dataset_name"] == "bfcl-v4-multi_turn"
    assert "first turn\nsecond turn" in multi_doc["records"][0]["prompt"]


def test_walk_drops_summary_rows_without_id(tmp_path: Path) -> None:
    """The first row of a BFCL score file is the per-category summary
    (``accuracy``, ``total_count``). It has no ``id`` and must not be
    promoted into a bench record — that would inject a numeric scalar
    masquerading as a prompt at the top of every emitted file."""
    drop_dir = tmp_path / "2025-12-16"
    _scaffold_drop(
        drop_dir,
        model="gpt-5-2025-08-07-FC",
        category="parallel",
        score_rows=[
            {"accuracy": 1.0, "total_count": 1},
            {"id": "parallel_0", "valid": True},
        ],
        result_rows=[{"id": "parallel_0", "question": "do two things at once"}],
    )

    out_root = tmp_path / "out"
    bfcl._walk_drop(drop_dir=drop_dir, out_root=out_root, drop_label="2025-12-16")
    doc = json.loads(
        (out_root / "parallel" / "gpt-5" / "2025-12-16.json").read_text()
    )
    assert doc["counts"] == 1
    assert doc["records"][0]["test_id"] == "parallel_0"
