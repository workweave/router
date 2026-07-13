from __future__ import annotations

import io
import tarfile
from pathlib import Path

import pytest

from hmm_sidecar.artifacts import _safe_extract, resolve_artifacts
from scripts.export_artifact import (
    deterministic_tar,
    public_classifier_metadata,
    public_roster,
)


def test_public_classifier_metadata_is_a_strict_runtime_allowlist() -> None:
    metadata = public_classifier_metadata(
        {
            "classes": ["fast", "maximum"],
            "feature_dim": 3431,
            "private_provenance": {"fingerprint": "not-public"},
            "private_training_metadata": {"value": "not-public"},
        }
    )

    assert metadata == {"classes": ["fast", "maximum"], "feature_dim": 3431}


def test_public_roster_keeps_only_runtime_model_arms() -> None:
    roster = public_roster(
        {
            "private_routing_metadata": "not-public",
            "clusters": {
                "fast": {
                    "arms": ["provider/fast"],
                    "private_cluster_metadata": "not-public",
                },
                "maximum": {
                    "arms": ["provider/maximum"],
                    "private_cluster_metadata": "not-public",
                },
            },
        },
        ["fast", "maximum"],
    )

    assert roster == {
        "schema_version": "hmm_router_public_roster_v1",
        "clusters": {
            "fast": {"arms": ["provider/fast"]},
            "maximum": {"arms": ["provider/maximum"]},
        },
    }


def test_public_archive_is_byte_for_byte_reproducible(tmp_path: Path) -> None:
    source = tmp_path / "source"
    source.mkdir()
    (source / "model.json").write_text('{"weights": [1, 2, 3]}\n')
    first = tmp_path / "first.tar.gz"
    second = tmp_path / "second.tar.gz"

    deterministic_tar(source, first)
    deterministic_tar(source, second)

    assert first.read_bytes() == second.read_bytes()


def test_rejects_archive_path_traversal(tmp_path: Path) -> None:
    archive = tmp_path / "malicious.tar.gz"
    with tarfile.open(archive, "w:gz") as payload:
        info = tarfile.TarInfo("../escape.txt")
        content = b"escape"
        info.size = len(content)
        payload.addfile(info, io.BytesIO(content))

    with pytest.raises(ValueError, match="unsafe archive member path"):
        _safe_extract(archive, tmp_path / "output")

    assert not (tmp_path / "escape.txt").exists()


def test_rejects_outer_package_digest_mismatch(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    archive = tmp_path / "package.tar.gz"
    archive.write_bytes(b"not-the-pinned-package")
    monkeypatch.setenv("HMM_PACKAGE_PATH", str(archive))
    monkeypatch.delenv("HMM_PACKAGE_URL", raising=False)
    monkeypatch.setenv("HMM_PACKAGE_SHA256", "0" * 64)
    monkeypatch.setenv("HMM_ARTIFACT_CACHE_DIR", str(tmp_path / "cache"))

    with pytest.raises(ValueError, match="package sha256 mismatch"):
        resolve_artifacts()


def test_url_requires_a_pinned_digest(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("HMM_PACKAGE_PATH", raising=False)
    monkeypatch.setenv("HMM_PACKAGE_URL", "https://example.test/model.tar.gz")
    monkeypatch.delenv("HMM_PACKAGE_SHA256", raising=False)

    with pytest.raises(ValueError, match="required with HMM_PACKAGE_URL"):
        resolve_artifacts()
