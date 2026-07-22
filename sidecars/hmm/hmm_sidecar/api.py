from __future__ import annotations

import logging
import os
from contextlib import asynccontextmanager
from typing import Any

from fastapi import FastAPI
from fastapi.responses import JSONResponse

from . import SCHEMA_VERSION
from .artifacts import FrozenArtifacts, resolve_artifacts
from .embeddings import (
    EmbeddingError,
    build_embedder,
    verify_embedding_contract,
)
from .policy import FrozenPolicy

log = logging.getLogger(__name__)
VERSION = os.environ.get("VERSION", "dev")


@asynccontextmanager
async def lifespan(application: FastAPI):
    try:
        artifacts = resolve_artifacts()
        embedder = build_embedder(artifacts.manifest.embedding_contract)
        similarity = await verify_embedding_contract(
            embedder,
            artifacts.manifest.embedding_contract,
            artifacts.probe_vector,
        )
        application.state.artifacts = artifacts
        application.state.policy = FrozenPolicy(artifacts, embedder)
        application.state.embedding_probe_similarity = similarity
        application.state.startup_error = None
    except Exception as exc:  # fail readiness while preserving liveness diagnostics
        log.exception("HMM sidecar failed to initialize")
        application.state.artifacts = None
        application.state.policy = None
        application.state.embedding_probe_similarity = None
        application.state.startup_error = f"{type(exc).__name__}: {exc}"
    yield


app = FastAPI(title="WorkWeave frozen HMM sidecar", lifespan=lifespan)


def _artifacts() -> FrozenArtifacts | None:
    return getattr(app.state, "artifacts", None)


@app.get("/livez")
def livez() -> JSONResponse:
    return JSONResponse({"status": "ok", "service": "hmm-sidecar", "version": VERSION})


@app.get("/health")
@app.get("/readyz")
def ready() -> JSONResponse:
    artifacts = _artifacts()
    loaded = getattr(app.state, "policy", None) is not None
    body = {
        "ready": loaded,
        "status": "healthy" if loaded else "unhealthy",
        "runtime_state": "frozen_policy" if loaded else "unservable",
        "service": "hmm-sidecar",
        "version": VERSION,
        "schema_version": SCHEMA_VERSION,
        "policy_artifact_id": artifacts.manifest.model_id if artifacts else None,
        "policy_artifact_sha256": artifacts.package_sha256 if artifacts else None,
        "embedding_probe_similarity": getattr(
            app.state, "embedding_probe_similarity", None
        ),
        "error": getattr(app.state, "startup_error", None),
    }
    return JSONResponse(body, status_code=200 if loaded else 503)


@app.get("/capabilities")
def capabilities() -> JSONResponse:
    return JSONResponse(
        {
            "schema_version": SCHEMA_VERSION,
            "reports_outcomes": False,
            "reports_feedback": False,
            "honors_preferred_models": False,
            "honors_quality_price_bias": False,
            "supports_debug_route_detail": True,
            "supports_preview": True,
            "supports_shadow": True,
            "reports_ranked_fallback": True,
            "authoritative_per_turn_selection": False,
            "learning": {
                "enabled": False,
                "state": "frozen_policy",
                "reason": "self-hosted sidecar serves immutable artifacts",
            },
        }
    )


@app.post("/outcome", status_code=204)
def outcome() -> None:
    return None


@app.post("/feedback", status_code=204)
def feedback() -> None:
    return None


@app.get("/roster")
def roster() -> JSONResponse:
    """Return the frozen roster: per-cluster ordered arm lists + sha256."""
    artifacts = _artifacts()
    policy: FrozenPolicy | None = getattr(app.state, "policy", None)
    if policy is None or artifacts is None:
        return JSONResponse({"error": "policy not loaded"}, status_code=503)
    clusters: dict[str, list[str]] = {
        label: [str(arm) for arm in cluster.get("arms") or []]
        for label, cluster in policy.clusters.items()
    }
    return JSONResponse(
        {
            "schema_version": SCHEMA_VERSION,
            "clusters": clusters,
            "roster_sha256": policy.roster_version,
        }
    )


@app.post("/route")
async def route(payload: dict[str, Any]) -> JSONResponse:
    policy: FrozenPolicy | None = getattr(app.state, "policy", None)
    if policy is None:
        return JSONResponse({"error": "policy not loaded"}, status_code=503)
    if payload.get("schema_version") not in (None, SCHEMA_VERSION):
        return JSONResponse({"error": "unsupported policy schema"}, status_code=400)
    try:
        result = await policy.route(payload)
    except (ValueError, EmbeddingError) as exc:
        log.warning("HMM route request rejected: %s", exc)
        return JSONResponse({"error": "route request rejected"}, status_code=422)
    except Exception as exc:
        log.exception("HMM route failed")
        return JSONResponse(
            {"error": f"route failed: {type(exc).__name__}"}, status_code=503
        )
    return JSONResponse(
        {
            "schema_version": SCHEMA_VERSION,
            **result.model_dump(mode="json"),
        }
    )


@app.post("/preview")
async def preview(payload: dict[str, Any]) -> JSONResponse:
    policy: FrozenPolicy | None = getattr(app.state, "policy", None)
    if policy is None:
        return JSONResponse({"error": "policy not loaded"}, status_code=503)
    if payload.get("schema_version") not in (None, SCHEMA_VERSION):
        return JSONResponse({"error": "unsupported policy schema"}, status_code=400)
    if payload.get("execution_mode") != "preview":
        return JSONResponse(
            {"error": "preview execution mode required"}, status_code=400
        )
    try:
        result = await policy.preview(payload)
    except (ValueError, EmbeddingError) as exc:
        log.warning("HMM preview request rejected: %s", exc)
        return JSONResponse({"error": "preview request rejected"}, status_code=422)
    except Exception as exc:
        log.exception("HMM preview failed")
        return JSONResponse(
            {"error": f"preview failed: {type(exc).__name__}"}, status_code=503
        )
    return JSONResponse(
        {
            "schema_version": SCHEMA_VERSION,
            **result.model_dump(mode="json"),
        }
    )
