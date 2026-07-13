from __future__ import annotations

from fastapi.testclient import TestClient

from hmm_sidecar.api import app


class RejectingPolicy:
    async def route(self, payload: dict[str, object]) -> None:
        del payload
        raise ValueError("private artifact path: /secret/model.npz")


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
    assert payload["learning"]["state"] == "frozen_policy"


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
