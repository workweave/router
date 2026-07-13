from __future__ import annotations

import hashlib
import json
import os
import fcntl
import shutil
import tarfile
import tempfile
import urllib.parse
from dataclasses import dataclass
from pathlib import Path

import httpx
import numpy as np

from .schemas import FrozenPackageManifest

MAX_ARCHIVE_BYTES = 128 * 1024 * 1024
MAX_ARCHIVE_MEMBERS = 128
MAX_MEMBER_BYTES = 64 * 1024 * 1024
DEFAULT_CACHE_DIR = Path("/tmp/workweave-hmm-artifacts")


@dataclass(frozen=True)
class FrozenArtifacts:
    root: Path
    manifest: FrozenPackageManifest
    package_sha256: str
    probe_vector: np.ndarray


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _expected_sha256() -> str | None:
    value = os.environ.get("HMM_PACKAGE_SHA256", "").strip().lower()
    if not value:
        return None
    if len(value) != 64 or any(char not in "0123456789abcdef" for char in value):
        raise ValueError("HMM_PACKAGE_SHA256 must be 64 lowercase hex characters")
    return value


def _cache_dir() -> Path:
    return Path(os.environ.get("HMM_ARTIFACT_CACHE_DIR", str(DEFAULT_CACHE_DIR)))


def _download(url: str, destination: Path) -> None:
    parsed = urllib.parse.urlparse(url)
    if parsed.scheme != "https":
        raise ValueError("HMM_PACKAGE_URL must use https")
    destination.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(dir=destination.parent, delete=False) as handle:
        temporary = Path(handle.name)
        total = 0
        try:
            with httpx.stream(
                "GET", url, follow_redirects=True, timeout=120.0
            ) as response:
                response.raise_for_status()
                if response.url.scheme != "https":
                    raise ValueError("HMM_PACKAGE_URL redirected to a non-HTTPS URL")
                for chunk in response.iter_bytes():
                    total += len(chunk)
                    if total > MAX_ARCHIVE_BYTES:
                        raise ValueError("HMM package exceeds maximum archive size")
                    handle.write(chunk)
            temporary.replace(destination)
        finally:
            temporary.unlink(missing_ok=True)


def _safe_extract(archive: Path, destination: Path) -> None:
    if archive.stat().st_size > MAX_ARCHIVE_BYTES:
        raise ValueError("HMM package exceeds maximum archive size")
    with tarfile.open(archive, "r:*") as payload:
        members = payload.getmembers()
        if len(members) > MAX_ARCHIVE_MEMBERS:
            raise ValueError("HMM package has too many archive members")
        root = destination.resolve()
        for member in members:
            if not member.isfile() and not member.isdir():
                raise ValueError(f"unsupported archive member: {member.name}")
            if member.size > MAX_MEMBER_BYTES:
                raise ValueError(f"archive member is too large: {member.name}")
            target = (destination / member.name).resolve()
            if root not in (target, *target.parents):
                raise ValueError(f"unsafe archive member path: {member.name}")
        payload.extractall(destination, members=members, filter="data")


def _verify_manifest(root: Path) -> FrozenPackageManifest:
    manifest_path = root / "manifest.json"
    manifest = FrozenPackageManifest.model_validate_json(manifest_path.read_text())
    for relative, expected in manifest.files.items():
        path = root / relative
        if not path.is_file():
            raise ValueError(f"package file is missing: {relative}")
        actual = sha256_file(path)
        if actual != expected:
            raise ValueError(
                f"package file sha256 mismatch for {relative}: "
                f"expected {expected}, got {actual}"
            )
    return manifest


def _materialize_archive(archive: Path, package_sha256: str, cache: Path) -> Path:
    root = cache / f"package-{package_sha256[:16]}"
    marker = root / ".complete"
    lock_path = cache / f"package-{package_sha256[:16]}.lock"
    with lock_path.open("w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        if marker.is_file():
            return root
        temporary = cache / f"extract-{package_sha256[:16]}-{os.getpid()}"
        shutil.rmtree(temporary, ignore_errors=True)
        try:
            temporary.mkdir(parents=True)
            _safe_extract(archive, temporary)
            _verify_manifest(temporary)
            (temporary / ".complete").write_text("complete\n")
            shutil.rmtree(root, ignore_errors=True)
            temporary.replace(root)
        finally:
            shutil.rmtree(temporary, ignore_errors=True)
    return root


def resolve_artifacts() -> FrozenArtifacts:
    package_path_raw = os.environ.get("HMM_PACKAGE_PATH", "").strip()
    package_url = os.environ.get("HMM_PACKAGE_URL", "").strip()
    if bool(package_path_raw) == bool(package_url):
        raise ValueError("set exactly one of HMM_PACKAGE_PATH or HMM_PACKAGE_URL")
    expected = _expected_sha256()
    cache = _cache_dir()
    cache.mkdir(parents=True, exist_ok=True)
    if package_url:
        if expected is None:
            raise ValueError("HMM_PACKAGE_SHA256 is required with HMM_PACKAGE_URL")
        archive = cache / f"download-{expected[:16]}.tar.gz"
        if not archive.is_file():
            _download(package_url, archive)
    else:
        archive = Path(package_path_raw)
    if not archive.is_file():
        raise FileNotFoundError(f"HMM package not found: {archive}")
    actual = sha256_file(archive)
    if expected is not None and actual != expected:
        raise ValueError(
            f"HMM package sha256 mismatch: expected {expected}, got {actual}"
        )
    root = _materialize_archive(archive, actual, cache)
    manifest = _verify_manifest(root)
    probe_path = root / manifest.embedding_contract.probe_vector_file
    probe = np.asarray(np.load(probe_path, allow_pickle=False), dtype=np.float64)
    if probe.shape != (manifest.embedding_contract.dimensions,):
        raise ValueError(
            f"embedding probe has shape {probe.shape}; expected "
            f"({manifest.embedding_contract.dimensions},)"
        )
    return FrozenArtifacts(root, manifest, actual, probe)
