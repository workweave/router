"""Tests for ingest.swebench._walk_submissions.

Fixture-driven; no networking. Builds a fake
``evaluation/verified/<submission>/`` tree in tmpdir, runs the walker,
and asserts on the emitted JSON shape and content. These would fail
if the walker stopped emitting files, mismapped IDs, or scored every
record the same.
"""

from __future__ import annotations

import json
import logging
from pathlib import Path

from ingest import swebench


def _write_metadata(submission_dir: Path, *, name: str, model: str) -> None:
    """Write SWE-bench-style metadata.yaml. The walker reads
    ``tags.model[0]`` for the canonical model identity; ``info.name``
    is the human-readable label and is irrelevant to alias resolution.
    """
    submission_dir.mkdir(parents=True, exist_ok=True)
    (submission_dir / "metadata.yaml").write_text(
        f"info:\n  name: {name}\ntags:\n  model:\n    - {model}\n  org:\n    - Test\n",
        encoding="utf-8",
    )


def _write_results(
    submission_dir: Path,
    *,
    resolved_ids: list[str],
    unresolved_ids: list[str],
) -> None:
    """Tests use the legacy ``resolved_ids`` / ``unresolved_ids``
    shape — both formats are supported by ``_read_results``.
    """
    results_dir = submission_dir / "results"
    results_dir.mkdir(parents=True, exist_ok=True)
    (results_dir / "results.json").write_text(
        json.dumps(
            {"resolved_ids": resolved_ids, "unresolved_ids": unresolved_ids}
        ),
        encoding="utf-8",
    )


def test_walk_emits_records_for_mapped_submission(tmp_path: Path) -> None:
    verified = tmp_path / "verified"
    sub = verified / "claude-sonnet-4-5__openhands"
    _write_metadata(sub, name="claude-sonnet-4-5__openhands", model="claude-sonnet-4-5")
    _write_results(sub, resolved_ids=["instance-a", "instance-c"], unresolved_ids=["instance-b"])

    out_root = tmp_path / "out"
    problems = {
        "instance-a": "Fix the off-by-one in foo.py",
        "instance-b": "Make the test pass for bar.py",
        "instance-c": "Resolve the import error in baz",
    }

    summary = swebench._walk_submissions(
        verified_dir=verified,
        out_root=out_root,
        problem_statements=problems,
    )

    assert summary.mapped == 1
    assert summary.unmapped == 0
    assert summary.total_records == 3

    out_path = out_root / "claude-sonnet-4-5" / "claude-sonnet-4-5__openhands.json"
    assert out_path.is_file()
    doc = json.loads(out_path.read_text())
    assert doc["model_name"] == "claude-sonnet-4-5"
    assert doc["dataset_name"] == "swebench-verified"
    assert doc["counts"] == 3
    # Records sort by instance_id and carry the canonical 0/1 score
    # mapping so a flipped resolver implementation would alter this list.
    by_id = {r["instance_id"]: r["score"] for r in doc["records"]}
    assert by_id == {"instance-a": 1.0, "instance-b": 0.0, "instance-c": 1.0}
    assert all(r["prompt"] == problems[r["instance_id"]] for r in doc["records"])


def test_walk_skips_and_logs_unmapped_submission(
    tmp_path: Path, caplog
) -> None:
    verified = tmp_path / "verified"
    sub = verified / "frobnicate__weird-agent"
    _write_metadata(sub, name="frobnicate__weird-agent", model="frobnicate-v9")
    _write_results(sub, resolved_ids=["x"], unresolved_ids=[])

    out_root = tmp_path / "out"
    with caplog.at_level(logging.WARNING, logger="ingest.swebench"):
        summary = swebench._walk_submissions(
            verified_dir=verified,
            out_root=out_root,
            problem_statements={"x": "anything"},
        )

    assert summary.mapped == 0
    assert summary.unmapped == 1
    assert not out_root.exists() or not any(out_root.rglob("*.json"))
    # The unmapped-model log message uses %-formatting so check both
    # the message template and the formatted output.
    assert any(
        "frobnicate-v9" in rec.getMessage() for rec in caplog.records
    ), "expected an 'unmapped model' warning in caplog"


def test_walk_drops_records_without_problem_statement(tmp_path: Path) -> None:
    """Instance IDs that aren't in the HF problem-statement cache must
    not produce zero-prompt records — the embedder would treat that as
    an empty string and skew clustering geometry."""
    verified = tmp_path / "verified"
    sub = verified / "gpt5_run"
    _write_metadata(sub, name="gpt5_run", model="gpt-5")
    _write_results(sub, resolved_ids=["known-id"], unresolved_ids=["mystery-id"])

    out_root = tmp_path / "out"
    summary = swebench._walk_submissions(
        verified_dir=verified,
        out_root=out_root,
        problem_statements={"known-id": "Known prompt"},
    )

    out_path = out_root / "gpt-5" / "gpt5_run.json"
    doc = json.loads(out_path.read_text())
    assert doc["counts"] == 1
    assert doc["records"][0]["instance_id"] == "known-id"
    assert summary.total_records == 1


def test_truncate_prompt_handles_zero_max_chars() -> None:
    """``text[-0:]`` is ``text[0:]`` in Python — returns the full
    string. ``truncate_prompt`` must guard against ``max_chars <= 0``
    so a misconfigured cap can't silently leak the entire prompt back
    into the bench file (cubic-flagged P2).
    """
    from ingest.common import truncate_prompt

    assert truncate_prompt("hello world", max_chars=0) == ""
    assert truncate_prompt("hello world", max_chars=-5) == ""
    # Sanity: positive caps still tail-truncate correctly.
    assert truncate_prompt("hello world", max_chars=5) == "world"
    assert truncate_prompt("short", max_chars=100) == "short"


def test_walk_handles_modern_resolved_only_schema(tmp_path: Path) -> None:
    """Modern SWE-bench results.json uses ``{"resolved": [...]}`` and
    derives unresolved IDs from the difference against the Verified
    split. Regression catch: the original parser only accepted the
    legacy ``resolved_ids`` / ``unresolved_ids`` shape and returned
    zero records for every modern submission.
    """
    verified = tmp_path / "verified"
    sub = verified / "modern_format_run"
    _write_metadata(sub, name="modern_format_run", model="claude-opus-4-5")
    results_dir = sub / "results"
    results_dir.mkdir(parents=True)
    (results_dir / "results.json").write_text(
        json.dumps({"resolved": ["a", "c"], "no_generation": [], "no_logs": []}),
        encoding="utf-8",
    )

    out_root = tmp_path / "out"
    summary = swebench._walk_submissions(
        verified_dir=verified,
        out_root=out_root,
        problem_statements={"a": "Q-a", "b": "Q-b", "c": "Q-c"},
    )
    assert summary.mapped == 1
    assert summary.total_records == 3
    doc = json.loads((out_root / "claude-opus-4-5" / "modern_format_run.json").read_text())
    by_id = {r["instance_id"]: r["score"] for r in doc["records"]}
    assert by_id == {"a": 1.0, "b": 0.0, "c": 1.0}


def test_walk_truncates_long_prompts_keeping_tail(tmp_path: Path) -> None:
    """Truncation must keep the prompt tail to match the runtime
    ``cluster.scorer.tailTruncate`` invariant. Head-truncation here
    would silently shift the embedded text region between training
    and serving and break the cluster geometry guarantee.
    """
    verified = tmp_path / "verified"
    sub = verified / "long-prompt-run"
    _write_metadata(sub, name="long-prompt-run", model="claude-sonnet-4-5")
    _write_results(sub, resolved_ids=["long"], unresolved_ids=[])

    out_root = tmp_path / "out"
    head = "HEAD-MARKER " * 1000
    tail = "TAIL-MARKER-the-actual-ask"
    long_text = head + tail
    assert len(long_text) > 8192

    swebench._walk_submissions(
        verified_dir=verified,
        out_root=out_root,
        problem_statements={"long": long_text},
        max_prompt_chars=8192,
    )
    doc = json.loads((out_root / "claude-sonnet-4-5" / "long-prompt-run.json").read_text())
    emitted = doc["records"][0]["prompt"]
    assert len(emitted) == 8192
    assert emitted.endswith(tail), "tail-truncation must preserve the prompt suffix"
    assert "HEAD-MARKER" not in emitted[-len(tail) :], "tail must not include head text"
