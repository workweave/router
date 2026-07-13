from __future__ import annotations

import os
from pathlib import Path

import pytest

from hmm_sidecar.artifacts import resolve_artifacts, sha256_file
from hmm_sidecar.policy import FrozenPolicy, select_roster_arm, selected_margin


class FixedEmbedder:
    def __init__(self, vector: list[float]) -> None:
        self.vector = vector

    async def embed(self, texts: list[str]) -> list[list[float]]:
        return [self.vector for _ in texts]


def test_roster_fallback_reports_the_selected_classifier_group() -> None:
    probabilities = {"fast": 0.2, "maximum": 0.8}

    label, roster_id = select_roster_arm(
        probabilities=probabilities,
        classes=("fast", "maximum"),
        clusters={
            "fast": {"arms": ["provider/fast"]},
            "maximum": {"arms": ["provider/maximum"]},
        },
        available_roster_ids={"provider/fast"},
    )

    assert label == "fast"
    assert roster_id == "provider/fast"
    assert selected_margin(probabilities, label) == pytest.approx(-0.6)


@pytest.mark.skipif(
    not os.environ.get("HMM_TEST_PACKAGE"),
    reason="published package is supplied by the release-artifact CI step",
)
async def test_published_package_routes_an_offered_candidate(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    package = Path(os.environ["HMM_TEST_PACKAGE"])
    monkeypatch.setenv("HMM_PACKAGE_PATH", str(package))
    monkeypatch.delenv("HMM_PACKAGE_URL", raising=False)
    monkeypatch.setenv("HMM_PACKAGE_SHA256", sha256_file(package))
    artifacts = resolve_artifacts()
    policy = FrozenPolicy(artifacts, FixedEmbedder(artifacts.probe_vector.tolist()))
    selected_label = "maximum"
    roster_id = policy.clusters[selected_label]["arms"][0]

    result = await policy.route(
        {
            "schema_version": "policy_router_v1",
            "route_id": "release-smoke-route",
            "prompt_text": "Implement the requested change.",
            "conversation_messages": [
                {"role": "user", "text": "Implement the requested change."}
            ],
            "candidates": [
                {
                    "roster_id": roster_id,
                    "catalog_id": roster_id,
                    "provider": roster_id.split("/", 1)[0],
                    "capabilities": {},
                }
            ],
        }
    )

    assert result.route_id == "release-smoke-route"
    assert result.selected_roster_id == roster_id
    assert result.policy_label == selected_label
    assert result.policy_artifact_sha256 == sha256_file(package)
