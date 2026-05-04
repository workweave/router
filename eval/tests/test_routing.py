"""Unit tests for routing.py — focus on the staging HTTP shape."""

import os

import pytest
import respx
from httpx import Response

from eval.routing import route


@pytest.fixture(autouse=True)
def _eval_env(monkeypatch):
    monkeypatch.setenv("ROUTER_BASE_URL", "https://router-test.invalid")
    monkeypatch.setenv("ROUTER_EVAL_API_KEY", "wv_test_key")


@respx.mock
@pytest.mark.asyncio
async def test_heuristic_router_sends_disable_cluster_header():
    captured: dict = {}

    def respond(request):
        captured["headers"] = dict(request.headers)
        return Response(
            200,
            json={
                "content": [{"type": "text", "text": "hi"}],
                "model": "claude-haiku-4-5",
                "usage": {"input_tokens": 5, "output_tokens": 1},
            },
            headers={"x-router-model": "claude-haiku-4-5"},
        )

    respx.post("https://router-test.invalid/v1/messages").mock(side_effect=respond)
    res = await route(router="heuristic", prompt="hello")
    assert res.error is None
    assert captured["headers"].get("x-weave-disable-cluster") == "true"
    assert captured["headers"].get("x-api-key") == "wv_test_key"
    assert res.model_used == "claude-haiku-4-5"


@respx.mock
@pytest.mark.asyncio
async def test_v01_cluster_last_user_sends_embed_header():
    """The v0.1-cluster-last-user router must send the embed-last-user
    header so the staging server flips PromptText to the most recent
    user-typed message. Mirrors the cluster-vs-heuristic A/B pattern but
    on the feature-extraction axis."""
    captured: dict = {}

    def respond(request):
        captured["headers"] = dict(request.headers)
        return Response(
            200,
            json={
                "content": [{"type": "text", "text": "ok"}],
                "model": "claude-sonnet-4-5",
                "usage": {"input_tokens": 1, "output_tokens": 1},
            },
            headers={"x-router-model": "claude-sonnet-4-5"},
        )

    respx.post("https://router-test.invalid/v1/messages").mock(side_effect=respond)
    res = await route(router="v0.1-cluster-last-user", prompt="hi")
    assert res.error is None
    assert captured["headers"].get("x-weave-embed-last-user-message") == "true"
    assert "x-weave-disable-cluster" not in captured["headers"], (
        "the embed-last-user variant must NOT also disable the cluster scorer; "
        "those are independent strategy axes"
    )
    assert res.model_used == "claude-sonnet-4-5"


@respx.mock
@pytest.mark.asyncio
async def test_v01_cluster_router_sends_version_header_only():
    captured: dict = {}

    def respond(request):
        captured["headers"] = dict(request.headers)
        return Response(
            200,
            json={
                "content": [{"type": "text", "text": "ok"}],
                "model": "claude-opus-4-7",
                "usage": {"input_tokens": 1, "output_tokens": 1},
            },
            headers={"x-router-model": "claude-opus-4-7"},
        )

    respx.post("https://router-test.invalid/v1/messages").mock(side_effect=respond)
    res = await route(router="v0.1-cluster", prompt="hi")
    assert res.error is None
    assert captured["headers"].get("x-weave-cluster-version") == "v0.1"
    assert "x-weave-disable-cluster" not in captured["headers"]
    assert "x-weave-embed-last-user-message" not in captured["headers"], (
        "plain v0.1-cluster must NOT send the embed-last-user header; "
        "that's the v0.1-cluster-last-user variant's job"
    )
    assert res.model_used == "claude-opus-4-7"


@respx.mock
@pytest.mark.asyncio
async def test_v02_cluster_routes_to_default_base_url():
    """v0.2-cluster hits ROUTER_BASE_URL with x-weave-cluster-version set
    so staging's Multiversion router dispatches to the v0.2 bundle."""
    captured: dict = {}

    def respond(request):
        captured["url"] = str(request.url)
        captured["headers"] = dict(request.headers)
        return Response(
            200,
            json={
                "content": [{"type": "text", "text": "ok"}],
                "model": "gpt-5.5",
                "usage": {"input_tokens": 1, "output_tokens": 1},
            },
            headers={"x-router-model": "gpt-5.5"},
        )

    respx.post("https://router-test.invalid/v1/messages").mock(side_effect=respond)
    res = await route(router="v0.2-cluster", prompt="hi")
    assert res.error is None
    assert captured["url"] == "https://router-test.invalid/v1/messages"
    assert captured["headers"].get("x-weave-cluster-version") == "v0.2"
    assert "x-weave-disable-cluster" not in captured["headers"]
    assert "x-weave-embed-last-user-message" not in captured["headers"]
    assert res.model_used == "gpt-5.5"


@respx.mock
@pytest.mark.asyncio
async def test_arbitrary_future_cluster_version_routes_through():
    """The cluster adapter accepts ANY vN.M-cluster — adding a v0.3
    artifact directory shouldn't require touching routing.py. The
    harness sends the version header; staging is responsible for
    rejecting unbuilt versions."""
    captured: dict = {}

    def respond(request):
        captured["headers"] = dict(request.headers)
        return Response(
            200,
            json={
                "content": [{"type": "text", "text": "ok"}],
                "model": "claude-opus-4-7",
                "usage": {"input_tokens": 1, "output_tokens": 1},
            },
            headers={"x-router-model": "claude-opus-4-7"},
        )

    respx.post("https://router-test.invalid/v1/messages").mock(side_effect=respond)
    res = await route(router="v0.3-cluster", prompt="hi")
    assert res.error is None
    assert captured["headers"].get("x-weave-cluster-version") == "v0.3"


@respx.mock
@pytest.mark.asyncio
async def test_v02_cluster_last_user_sends_embed_header():
    captured: dict = {}

    def respond(request):
        captured["headers"] = dict(request.headers)
        return Response(
            200,
            json={
                "content": [{"type": "text", "text": "ok"}],
                "model": "gemini-3.1-pro-preview",
                "usage": {"input_tokens": 1, "output_tokens": 1},
            },
            headers={"x-router-model": "gemini-3.1-pro-preview"},
        )

    respx.post("https://router-test.invalid/v1/messages").mock(side_effect=respond)
    res = await route(router="v0.2-cluster-last-user", prompt="hi")
    assert res.error is None
    assert captured["headers"].get("x-weave-embed-last-user-message") == "true"


@respx.mock
@pytest.mark.asyncio
async def test_v01_cluster_uses_v01_base_url_override(monkeypatch):
    """When ROUTER_BASE_URL_V01 is set, v0.1-cluster routes there
    instead of ROUTER_BASE_URL — that's the knob for pinning the
    legacy 3-Anthropic cluster at a frozen older deployment."""
    monkeypatch.setenv("ROUTER_BASE_URL_V01", "https://router-legacy.invalid")
    captured: dict = {}

    def respond(request):
        captured["url"] = str(request.url)
        return Response(
            200,
            json={
                "content": [{"type": "text", "text": "ok"}],
                "model": "claude-opus-4-7",
                "usage": {"input_tokens": 1, "output_tokens": 1},
            },
            headers={"x-router-model": "claude-opus-4-7"},
        )

    respx.post("https://router-legacy.invalid/v1/messages").mock(side_effect=respond)
    res = await route(router="v0.1-cluster", prompt="hi")
    assert res.error is None
    assert captured["url"] == "https://router-legacy.invalid/v1/messages"


@respx.mock
@pytest.mark.asyncio
async def test_routing_captures_error_on_5xx():
    respx.post("https://router-test.invalid/v1/messages").mock(return_value=Response(500))
    res = await route(router="heuristic", prompt="hi")
    assert res.error is not None
    assert res.output_text == ""


@pytest.mark.asyncio
async def test_unknown_router_raises_value_error():
    with pytest.raises(ValueError):
        await route(router="not-a-router", prompt="x")  # type: ignore[arg-type]
