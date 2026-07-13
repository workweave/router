#!/usr/bin/env python3
"""Export a trusted WorkWeave HMM package into the public data-only format.

This is an offline release-maintainer tool. It deliberately accepts the legacy
trusted pickle as input, but the emitted package contains no pickle and the
runtime never deserializes executable Python objects.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import pickle
import shutil
import tarfile
import tempfile
from pathlib import Path
from typing import Any

import numpy as np

PACKAGE_SCHEMA = "hmm_router_frozen_package_v1"
PATH_KEYS = {
    "input_path",
    "output_path",
    "raw_conversation_csv",
    "corpus",
    "corpus_weavedata",
    "source_path",
}


class _StateShim:
    """Data-only target for trusted private classes referenced by the pickle."""

    def __new__(cls, *args: Any, **kwargs: Any) -> "_StateShim":
        del args, kwargs
        return super().__new__(cls)

    def __setstate__(self, state: dict[str, Any]) -> None:
        self.__dict__.update(state)


class _TrustedModelUnpickler(pickle.Unpickler):
    _SHIM_MODULES = {
        "ml_dev.agent_flow.llm_classifier.codebook",
        "ml_dev.agent_flow.llm_classifier.bayes_hmm",
    }

    def find_class(self, module: str, name: str) -> Any:
        if module in self._SHIM_MODULES:
            return _StateShim
        return super().find_class(module, name)


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def sanitize(value: Any) -> Any:
    if isinstance(value, dict):
        return {
            key: sanitize(item)
            for key, item in value.items()
            if key not in PATH_KEYS and not key.endswith("_path")
        }
    if isinstance(value, list):
        return [sanitize(item) for item in value]
    if isinstance(value, str) and value.startswith("/"):
        return "[redacted-local-path]"
    return value


def write_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")


def export_hmm(source: Path, destination: Path) -> int:
    with source.open("rb") as handle:
        payload = _TrustedModelUnpickler(handle).load()
    projector = payload.projector
    model = payload.model
    samples = model.parameter_samples_
    map_index = model.map_sample_index_
    if map_index is None:
        scores = [
            (
                float(sample.log_likelihood)
                if sample.log_likelihood is not None
                else float("-inf")
            )
            for sample in samples
        ]
        map_index = int(np.argmax(scores))
    sample = samples[map_index]
    if str(getattr(model, "covariance_type", "diagonal")) != "diagonal":
        raise ValueError("the public v1 runtime supports diagonal covariance only")
    np.savez_compressed(
        destination,
        pca_mean=np.asarray(projector.mean_, dtype=np.float64),
        pca_components=np.asarray(projector.components_, dtype=np.float64),
        pca_whiten=np.asarray(bool(projector.whiten)),
        pca_explained_variance=np.asarray(
            projector.explained_variance_, dtype=np.float64
        ),
        startprob=np.asarray(sample.startprob, dtype=np.float64),
        transmat=np.asarray(sample.transmat, dtype=np.float64),
        means=np.asarray(sample.means, dtype=np.float64),
        variances=np.asarray(sample.variances, dtype=np.float64),
    )
    return int(np.asarray(sample.startprob).shape[0])


def deterministic_tar(source: Path, output: Path) -> None:
    output.parent.mkdir(parents=True, exist_ok=True)
    with tarfile.open(output, "w:gz", format=tarfile.PAX_FORMAT) as archive:
        for path in sorted(source.rglob("*")):
            if not path.is_file():
                continue
            info = archive.gettarinfo(str(path), arcname=str(path.relative_to(source)))
            info.uid = info.gid = 0
            info.uname = info.gname = ""
            info.mtime = 0
            with path.open("rb") as handle:
                archive.addfile(info, handle)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--probe-vector", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--model-id", default="hmm-router-frozen-v1")
    parser.add_argument("--probe-text", required=True)
    parser.add_argument("--probe-min-cosine", type=float, default=0.99999)
    args = parser.parse_args()

    source = args.source.resolve()
    with tempfile.TemporaryDirectory(prefix="hmm-public-export-") as temporary:
        root = Path(temporary)
        classifier_out = root / "classifier"
        classifier_out.mkdir()
        for filename in ("model.json", "classes.json"):
            shutil.copyfile(source / "classifier" / filename, classifier_out / filename)
        metadata = sanitize(
            json.loads((source / "classifier" / "metadata.json").read_text())
        )
        write_json(classifier_out / "metadata.json", metadata)

        hmm_out = root / "hmm" / "model.npz"
        hmm_out.parent.mkdir()
        states = export_hmm(
            source / "hmm_bundle" / "models" / "latent" / "model.pkl",
            hmm_out,
        )
        shutil.copyfile(source / "roster.json", root / "roster.json")
        shutil.copyfile(
            source / "hmm_bundle" / "cards" / "state_cards.json",
            root / "state_cards.json",
        )
        probe = np.asarray(
            np.load(args.probe_vector, allow_pickle=False), dtype=np.float32
        )
        embedding_dimensions = int(metadata["embedding_dim"])
        if probe.shape != (embedding_dimensions,):
            raise ValueError(
                f"probe vector has shape {probe.shape}; expected ({embedding_dimensions},)"
            )
        np.save(root / "embedding_probe.f32.npy", probe, allow_pickle=False)

        private_manifest = json.loads((source / "manifest.json").read_text())
        files = {
            str(path.relative_to(root)): sha256_file(path)
            for path in sorted(root.rglob("*"))
            if path.is_file()
        }
        manifest = {
            "schema_version": PACKAGE_SCHEMA,
            "model_id": args.model_id,
            "package_sha256": None,
            "embedding_contract": {
                "model": metadata["embedding_model"],
                "dimensions": embedding_dimensions,
                "task_type": None,
                "probe_text": args.probe_text,
                "probe_vector_file": "embedding_probe.f32.npy",
                "minimum_cosine_similarity": args.probe_min_cosine,
            },
            "classifier": {
                "path": "classifier",
                "classes": metadata["classes"],
            },
            "hmm": {"path": "hmm/model.npz", "states": states},
            "roster": {"path": "roster.json"},
            "state_cards": {"path": "state_cards.json"},
            "training_privacy": sanitize(private_manifest["training_privacy"]),
            "files": files,
        }
        write_json(root / "manifest.json", manifest)
        deterministic_tar(root, args.output)
    print(
        json.dumps(
            {
                "output": str(args.output),
                "sha256": sha256_file(args.output),
                "model_id": args.model_id,
            },
            indent=2,
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
