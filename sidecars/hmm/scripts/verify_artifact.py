#!/usr/bin/env python3
"""Verify a portable HMM package without contacting an embedding provider."""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path

from hmm_sidecar.artifacts import resolve_artifacts
from hmm_sidecar.classifier import FrozenClassifier
from hmm_sidecar.hmm import FrozenHMM


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("package", type=Path)
    parser.add_argument("--sha256")
    args = parser.parse_args()
    os.environ["HMM_PACKAGE_PATH"] = str(args.package)
    os.environ.pop("HMM_PACKAGE_URL", None)
    if args.sha256:
        os.environ["HMM_PACKAGE_SHA256"] = args.sha256
    artifacts = resolve_artifacts()
    hmm = FrozenHMM(artifacts.root / artifacts.manifest.hmm.path)
    classifier = FrozenClassifier(artifacts.root / artifacts.manifest.classifier.path)
    print(
        json.dumps(
            {
                "model_id": artifacts.manifest.model_id,
                "package_sha256": artifacts.package_sha256,
                "embedding_model": artifacts.manifest.embedding_contract.model,
                "embedding_dimensions": artifacts.manifest.embedding_contract.dimensions,
                "states": hmm.startprob.shape[0],
                "classifier_features": classifier.feature_dim,
            },
            indent=2,
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
