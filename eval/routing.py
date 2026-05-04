"""Router-under-test clients.

Routers evaluated on the same prompts:

    always-opus / always-sonnet / always-haiku        → direct Anthropic call
    always-gpt55 / always-gpt55-mini / always-gpt-4.1 → direct OpenAI call
    always-gemini3-pro / always-gemini3-flash /
    always-gemini3-flash-lite                          → direct Google (Gemini)
                                                        OpenAI-compat call
    heuristic                                         → staging /v1/messages with
                                                        x-weave-disable-cluster: true
    v<X.Y>-cluster / v<X.Y>-cluster-last-user         → staging /v1/messages with
                                                        x-weave-cluster-version:
                                                        v<X.Y> so the staging
                                                        Multiversion router
                                                        dispatches to that
                                                        committed artifact bundle.
                                                        The -last-user variants
                                                        also add
                                                        x-weave-embed-last-user-message: true.

    Cluster routers are *adapters*: any version directory committed at
    router/internal/router/cluster/artifacts/v<X.Y>/ is reachable by
    name without code changes here. ``ROUTER_BASE_URL_V01`` remains an
    override knob for pinning v0.1 at a separate frozen deployment if
    one exists; otherwise every cluster version answers from
    ROUTER_BASE_URL via the per-request version header.

The always-X routers don't touch the staging deployment; they exercise
each provider's API directly so the cost / latency baseline isn't
inflated by router overhead.
"""

from __future__ import annotations

import asyncio
import os
import time
from dataclasses import dataclass

import httpx

from eval.inference import (
    GOOGLE_MODELS,
    OPENAI_MODELS,
    InferenceResult,
    call_anthropic,
    call_openai_chat,
)
from eval.types import (
    RouterName,
    is_staging_router,
    parse_cluster_router,
    validate_router_name,
)

DEFAULT_TIMEOUT_S = 180.0  # generous; staging cold-start can be slow.


@dataclass
class RoutedResult(InferenceResult):
    """Same fields as InferenceResult plus model_used (the model the
    router actually picked, read from x-router-model)."""

    model_used: str = ""


# always-X router → deployed model name. Update both this map and
# eval/types.py RouterName when adding a new always-X router. Frontier
# IDs (gpt-5.5, gemini-3.x) reflect the April 2026 model lineups; bump
# them when vendors ship the next generation.
_ALWAYS_X_MODEL = {
    # Anthropic
    "always-opus":               "claude-opus-4-7",
    "always-sonnet":             "claude-sonnet-4-5",
    "always-haiku":              "claude-haiku-4-5",
    # OpenAI (April 2026)
    "always-gpt55":              "gpt-5.5",
    "always-gpt55-mini":         "gpt-5.5-mini",
    "always-gpt-4.1":            "gpt-4.1",
    # Google (Gemini OpenAI-compat surface, April 2026; -preview suffix
    # required while the 3.x family is in preview)
    "always-gemini3-pro":        "gemini-3.1-pro-preview",
    "always-gemini3-flash":      "gemini-3-flash-preview",
    "always-gemini3-flash-lite": "gemini-3.1-flash-lite-preview",
}


async def route(
    *,
    router: RouterName,
    prompt: str,
    requested_model: str = "claude-opus-4-7",
    max_output_tokens: int = 2048,
    timeout_s: float = DEFAULT_TIMEOUT_S,
) -> RoutedResult:
    """Dispatch a single (router, prompt) pair and return the result.

    Validates the router name against the always-X / heuristic /
    cluster-version shapes before dispatching. Unknown shapes raise
    ValueError so a typo'd version name fails loud at the harness layer
    instead of silently no-op'ing through a 404 from staging.
    """
    validate_router_name(router)
    if router in _ALWAYS_X_MODEL:
        return await _route_always_x(router, prompt, max_output_tokens, timeout_s)
    if is_staging_router(router):
        return await _route_via_staging(router, prompt, requested_model, max_output_tokens, timeout_s)
    raise ValueError(f"unknown router: {router!r}")


async def _route_always_x(
    router: RouterName, prompt: str, max_output_tokens: int, timeout_s: float
) -> RoutedResult:
    model = _ALWAYS_X_MODEL[router]
    if model in OPENAI_MODELS or model in GOOGLE_MODELS:
        res = await call_openai_chat(
            model=model, prompt=prompt, max_output_tokens=max_output_tokens, timeout_s=timeout_s
        )
    else:
        res = await call_anthropic(
            model=model, prompt=prompt, max_output_tokens=max_output_tokens, timeout_s=timeout_s
        )
    return RoutedResult(
        output_text=res.output_text,
        input_tokens=res.input_tokens,
        output_tokens=res.output_tokens,
        latency_ms=res.latency_ms,
        error=res.error,
        model_used=model,
    )


async def _route_via_staging(
    router: RouterName,
    prompt: str,
    requested_model: str,
    max_output_tokens: int,
    timeout_s: float,
) -> RoutedResult:
    default_base = os.environ.get("ROUTER_BASE_URL", "https://router-staging.workweave.ai").rstrip("/")
    parsed = parse_cluster_router(router)
    cluster_version: str | None = None
    last_user = False
    if parsed is not None:
        cluster_version, last_user = parsed
    if cluster_version == "v0.1":
        # Legacy 3-Anthropic cluster: routable through the staging
        # Multiversion router by default (the v0.1 bundle ships in the
        # binary), but if a frozen older deployment is pinned via
        # ROUTER_BASE_URL_V01 we send the request there instead so
        # comparisons against the historical scorer see the historical
        # serving environment as well.
        base_url = os.environ.get("ROUTER_BASE_URL_V01", default_base).rstrip("/")
    else:
        base_url = default_base
    api_key = os.environ["ROUTER_EVAL_API_KEY"]
    url = f"{base_url}/v1/messages"

    headers = {
        "x-api-key": api_key,
        "anthropic-version": "2023-06-01",
        "content-type": "application/json",
    }
    if router == "heuristic":
        headers["x-weave-disable-cluster"] = "true"
    if cluster_version is not None:
        # Per-request artifact-version pin. Read by
        # middleware.WithClusterVersionOverride and forwarded onto the
        # cluster.Multiversion router's ctx. Only honored when the
        # installation is is_eval_allowlisted; customer traffic with
        # this header set is silently ignored.
        headers["x-weave-cluster-version"] = cluster_version
    if last_user:
        # Trusted header gated on installation.is_eval_allowlisted by
        # the server middleware (WithEmbedLastUserMessageOverride). Mis-
        # set on a customer key it's silently ignored — the harness key
        # is the only one that should be allowlisted.
        headers["x-weave-embed-last-user-message"] = "true"

    body = {
        "model": requested_model,
        "max_tokens": max_output_tokens,
        "messages": [{"role": "user", "content": prompt}],
    }

    started = time.monotonic()
    try:
        async with httpx.AsyncClient(timeout=httpx.Timeout(timeout_s)) as client:
            r = await client.post(url, headers=headers, json=body)
        r.raise_for_status()
    except (httpx.HTTPError, asyncio.TimeoutError) as e:
        latency_ms = int((time.monotonic() - started) * 1000)
        return RoutedResult(
            output_text="",
            input_tokens=0,
            output_tokens=0,
            latency_ms=latency_ms,
            error=str(e),
            model_used="",
        )
    latency_ms = int((time.monotonic() - started) * 1000)
    try:
        payload = r.json()
    except ValueError as e:
        # Staging occasionally returns HTML error bodies; treat as a
        # row-level error rather than crashing the run.
        return RoutedResult(
            output_text="",
            input_tokens=0,
            output_tokens=0,
            latency_ms=latency_ms,
            error=f"non-JSON response from router: {e}",
            model_used="",
        )
    parts: list[str] = []
    for block in payload.get("content", []):
        if isinstance(block, dict) and "text" in block:
            parts.append(block["text"])
    usage = payload.get("usage", {}) or {}
    return RoutedResult(
        output_text="".join(parts),
        input_tokens=int(usage.get("input_tokens", 0) or 0),
        output_tokens=int(usage.get("output_tokens", 0) or 0),
        latency_ms=latency_ms,
        model_used=r.headers.get("x-router-model", payload.get("model", "")),
    )
