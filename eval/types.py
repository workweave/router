"""Shared row schemas for the eval harness.

Every JSONL row written to GCS uses one of the pydantic models below so
the Modal-side serialization, GCS round-trip, and aggregation share one
truth. Keep this file small — only types and the router-name parser.

Router naming convention
------------------------
``RouterName`` is a free-form string validated by the
``ALWAYS_X_ROUTERS`` set (Anthropic / OpenAI / Google direct calls),
the literal ``"heuristic"``, or the cluster-version pattern
``vX.Y-cluster`` / ``vX.Y-cluster-last-user``. Cluster routers are
adapters: any version string committed under
``router/internal/router/cluster/artifacts/`` is a valid router name —
the harness sends it to staging via ``x-weave-cluster-version`` and
staging dispatches to the matching artifact bundle. This mirrors
``models/v2``'s eval-adapter pattern (each per-task version is a
pluggable adapter; eval results join offline).
"""

from __future__ import annotations

import re
from typing import Any, Literal

from pydantic import BaseModel, Field


# ----- Reference grading (ground-truth pass/fail for code/math/tools) -----

ReferenceKind = Literal[
    "code_tests",       # payload: {"language": str, "tests": str}
    "numeric_match",    # payload: {"answer": str | float}
    "tool_call_match",  # payload: {"name": str, "arguments": dict}
    "none",             # no ground truth — LLM judge only
]


class Reference(BaseModel):
    kind: ReferenceKind
    payload: dict[str, Any] = Field(default_factory=dict)


# ----- Benchmark prompts (output of BenchmarkLoader.load) -----


class BenchmarkPrompt(BaseModel):
    prompt_id: str
    slice: str
    source: str
    prompt_text: str
    reference: Reference | None = None
    metadata: dict[str, Any] = Field(default_factory=dict)


# ----- Inference output (one row per (prompt, router)) -----

# RouterName is a string. Validation is centralized via
# ``validate_router_name`` (called by ``route()`` in routing.py) rather
# than encoded as a pydantic Literal — adding a new cluster artifact
# version (e.g. v0.3) shouldn't require touching this file. The
# always-X set IS still closed; new always-X routers must be added
# explicitly because each one needs an entry in routing.py's
# _ALWAYS_X_MODEL map.
RouterName = str

# Direct-call (no staging hop) baselines. Each entry maps to a deployed
# model name in routing.py's _ALWAYS_X_MODEL — adding here without
# adding there is a runtime ValueError. Updating both: see routing.py.
ALWAYS_X_ROUTERS: frozenset[str] = frozenset({
    # Anthropic
    "always-opus",
    "always-sonnet",
    "always-haiku",
    # OpenAI (April 2026 frontier)
    "always-gpt55",
    "always-gpt55-mini",
    "always-gpt-4.1",
    # Google (Gemini OpenAI-compat)
    "always-gemini3-pro",
    "always-gemini3-flash",
    "always-gemini3-flash-lite",
})

# Cluster routers parse as ``v<major>.<minor>-cluster`` with an optional
# ``-last-user`` suffix. Any committed artifact directory under
# ``router/internal/router/cluster/artifacts/`` is reachable through
# this naming. The ``-last-user`` variants set the
# x-weave-embed-last-user-message header on top of the version override
# so the harness can A/B feature-extraction shape × artifact version
# orthogonally on one staging deployment.
CLUSTER_ROUTER_PATTERN = re.compile(r"^(v\d+\.\d+)-cluster(-last-user)?$")


def is_cluster_router(name: str) -> bool:
    """True if ``name`` is a cluster-version adapter (any committed
    artifact version, present or future). The router itself rejects
    versions it didn't build at boot — this regex doesn't enforce
    existence, only shape."""
    return CLUSTER_ROUTER_PATTERN.match(name) is not None


def parse_cluster_router(name: str) -> tuple[str, bool] | None:
    """Return (artifact_version, last_user_flag) for cluster names
    or None for everything else. ``v0.2-cluster-last-user`` →
    ``("v0.2", True)``; ``always-opus`` → ``None``."""
    m = CLUSTER_ROUTER_PATTERN.match(name)
    if not m:
        return None
    return m.group(1), m.group(2) is not None


def is_staging_router(name: str) -> bool:
    """Routers that dispatch through the staging /v1/messages endpoint
    rather than calling a provider's API directly. Heuristic + every
    cluster-version adapter."""
    return name == "heuristic" or is_cluster_router(name)


def validate_router_name(name: str) -> None:
    """Raise ValueError if ``name`` doesn't match a known shape:
    always-X, ``heuristic``, or ``vX.Y-cluster(-last-user)?``. Called
    by ``route()`` so the harness fails loud on typos."""
    if name in ALWAYS_X_ROUTERS or name == "heuristic" or is_cluster_router(name):
        return
    raise ValueError(
        f"unknown router: {name!r} — expected one of {sorted(ALWAYS_X_ROUTERS)}, "
        f"'heuristic', or 'vX.Y-cluster' / 'vX.Y-cluster-last-user'."
    )


# Backwards-compatibility shim. Existing call sites can keep using
# ``r in STAGING_ROUTERS`` membership checks; new code should call
# ``is_staging_router(r)``. The two ``v0.{1,2}-cluster`` baselines
# stay enumerated here for readability — anything else falls through
# to the regex via __contains__.
class _StagingRouters:
    _BASE = {"heuristic", "v0.1-cluster", "v0.1-cluster-last-user",
             "v0.2-cluster", "v0.2-cluster-last-user"}

    def __contains__(self, item: str) -> bool:
        return is_staging_router(item)

    def __iter__(self):
        # Used by formatting in error messages; surface the explicit
        # baseline set so docs stay readable.
        return iter(sorted(self._BASE))


STAGING_ROUTERS: _StagingRouters = _StagingRouters()


class InferenceRow(BaseModel):
    run_id: str
    prompt_id: str
    router: RouterName
    # The model the router actually picked. For always-X this equals the
    # forced model; for heuristic / v0.{1,2}-cluster it's read from
    # the x-router-model header on the staging response.
    model_used: str
    output_text: str
    input_tokens: int
    output_tokens: int
    latency_ms: int
    cost_usd: float
    error: str | None = None


# ----- Judge output (one row per (prompt, judge, candidate router)) -----

JudgeName = Literal["gpt5", "gemini"]


class RubricScores(BaseModel):
    """Five-dim rubric (each 1..5)."""

    correctness: float
    completeness: float
    code_quality: float
    style: float
    follows_instructions: float


class JudgmentRow(BaseModel):
    run_id: str
    prompt_id: str
    judge: JudgeName
    # Pairwise: candidate is judged against baseline (always-Opus).
    candidate_router: RouterName
    baseline_router: RouterName  # always "always-opus" in PR 2
    rubric: RubricScores
    score: float  # mean(rubric values) / 5.0 — normalized [0, 1]
    rationale: str
    raw_response: str
    error: str | None = None


# ----- Aggregated per-router result -----


class RouterResult(BaseModel):
    """One row per router, ready for the Pareto plot + EVAL_RESULTS table."""

    router: RouterName
    n_prompts: int
    total_cost_usd: float
    mean_quality: float            # ensemble-median across all prompts
    reference_pass_rate: float | None  # None when no slices supplied references
    p50_latency_ms: int
    p95_latency_ms: int
    # Per-router model-pick distribution (for heuristic / cluster
    # variants). Keys are model names; values are counts.
    model_picks: dict[str, int] = Field(default_factory=dict)
