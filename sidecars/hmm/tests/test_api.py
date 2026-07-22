from __future__ import annotations

from fastapi.testclient import TestClient

from hmm_sidecar.api import app
from hmm_sidecar.schemas import RoutePreviewResult


class RejectingPolicy:
    async def route(self, payload: dict[str, object]) -> None:
        del payload
        raise ValueError("private artifact path: /secret/model.npz")


class PreviewingPolicy:
    async def preview(self, payload: dict[str, object]) -> object:
        assert payload["execution_mode"] == "preview"
        return RoutePreviewResult(
            route_id="route",
            policy_artifact_id="artifact",
            policy_artifact_sha256="a" * 64,
            roster_sha256="b" * 64,
            hmm_state_id=1,
            hmm_state_path=(0, 1),
            hmm_state_probabilities=(0.3, 0.7),
            class_order=("fast", "maximum"),
            class_probabilities={"fast": 0.6, "maximum": 0.4},
            ranked_fallback=(
                {
                    "group": "fast",
                    "probability": 0.6,
                    "roster_arms": ("provider/a",),
                    "eligible_arms": ("provider/a",),
                },
                {
                    "group": "maximum",
                    "probability": 0.4,
                    "roster_arms": ("provider/b",),
                    "eligible_arms": (),
                },
            ),
            selected_group="fast",
            eligible_roster_ids=("provider/a",),
        )


class RosterPolicy:
    clusters = {
        "maximum": {"arms": ["provider/opus", "provider/fable"]},
        "fast": {"arms": ["provider/haiku"]},
    }
    roster_version = "c" * 64


def test_liveness_does_not_depend_on_model_readiness() -> None:
    with TestClient(app) as client:
        response = client.get("/livez")

    assert response.status_code == 200
    assert response.json()["status"] == "ok"


def test_capabilities_are_frozen_and_do_not_request_content_callbacks() -> None:
    with TestClient(app) as client:
        response = client.get("/capabilities")

    assert response.status_code == 200
    payload = response.json()
    assert payload["schema_version"] == "policy_router_v1"
    assert payload["reports_outcomes"] is False
    assert payload["reports_feedback"] is False
    assert payload["supports_shadow"] is True
    assert payload["reports_ranked_fallback"] is True
    assert payload["learning"]["state"] == "frozen_policy"


def test_roster_returns_ordered_cluster_arms() -> None:
    with TestClient(app) as client:
        app.state.policy = RosterPolicy()
        app.state.artifacts = object()
        response = client.get("/roster")

    assert response.status_code == 200
    payload = response.json()
    assert payload["clusters"]["maximum"] == ["provider/opus", "provider/fable"]
    assert payload["clusters"]["fast"] == ["provider/haiku"]
    assert payload["roster_sha256"] == "c" * 64


def test_roster_fails_closed_without_a_policy() -> None:
    with TestClient(app) as client:
        response = client.get("/roster")

    assert response.status_code == 503


def test_disabled_callbacks_are_contract_compatible_noops() -> None:
    with TestClient(app) as client:
        outcome = client.post("/outcome", json={"response_text": "not retained"})
        feedback = client.post("/feedback", json={"feedback": "not retained"})

    assert outcome.status_code == 204
    assert feedback.status_code == 204
    assert outcome.content == b""
    assert feedback.content == b""


def test_readiness_fails_closed_without_an_artifact() -> None:
    with TestClient(app) as client:
        response = client.get("/readyz")

    assert response.status_code == 503
    assert response.json()["ready"] is False


def test_route_rejections_do_not_expose_internal_exception_text() -> None:
    with TestClient(app) as client:
        app.state.policy = RejectingPolicy()
        response = client.post("/route", json={"schema_version": "policy_router_v1"})

    assert response.status_code == 422
    assert response.json() == {"error": "route request rejected"}
    assert "/secret/model.npz" not in response.text


def test_preview_requires_explicit_mode_and_returns_all_selected_arms() -> None:
    with TestClient(app) as client:
        app.state.policy = PreviewingPolicy()
        rejected = client.post("/preview", json={"schema_version": "policy_router_v1"})
        response = client.post(
            "/preview",
            json={
                "schema_version": "policy_router_v1",
                "execution_mode": "preview",
            },
        )

    assert rejected.status_code == 400
    assert response.status_code == 200
    assert response.json()["eligible_roster_ids"] == ["provider/a"]
