"""Modal app: router-eval-harness.

Three functions:

  - run_inference(run_id, prompt_id, router): one Anthropic / staging call
  - run_judge(run_id, prompt_id, judge, candidate_router): one pairwise judgment
  - aggregate(run_id): pull all rows, build Pareto, write EVAL_RESULTS.md candidate

Local entry point composes the slice (slice_plan.SLICES), uploads
prompts.jsonl + manifest.json to GCS, then fans inference and judging
out via .spawn_map().

Each function is idempotent on (run_id, key) — re-running after a
partial failure is cheap because GCS objects are checked first and
the call short-circuits if the row already exists. Use --force to
overwrite.
"""

from __future__ import annotations

import asyncio
import json
import os
import sys
import uuid
from pathlib import Path
from typing import Any

# `eval` resolves as a Python package only when router/ (the parent of
# eval/) is on sys.path. The harness uses a flat layout
# (package-mode = false in pyproject.toml) so it isn't pip-installed,
# and `cd router/eval && modal run modal_app.py` puts router/eval/ on
# sys.path — but eval/ itself is not a subdirectory of CWD, so
# `add_local_python_source("eval")` below would fail with "eval has no
# spec — might not be installed". Mirror tests/conftest.py: prepend
# router/ so `import eval.X` and Modal's local-source bundling both
# resolve.
_PACKAGE_ROOT = Path(__file__).resolve().parents[1]  # router/
if str(_PACKAGE_ROOT) not in sys.path:
    sys.path.insert(0, str(_PACKAGE_ROOT))

# Load eval/.env before any Modal secret construction below — the
# Vertex toggle (EVAL_GEMINI_VERTEX, GOOGLE_CLOUD_PROJECT, ...) is the
# documented .env knob for forcing Gemini billing through Vertex, and
# vertex_env_secret reads os.environ at import time. Without this, the
# .env values land too late and Modal workers see an empty secret.
from eval import _env  # noqa: F401  side-effect: load .env

import modal

# Shared image for all three functions. Same dependency set as the
# Poetry pyproject — duplicated here because Modal builds a fresh
# image and can't read pyproject directly. Kept in sync by hand;
# CI smoke covers a missing dep with a fast import error.
image = (
    modal.Image.debian_slim(python_version="3.12").uv_pip_install(
        "anthropic>=0.40,<1.0",
        "openai>=1.50,<2.0",
        "google-genai>=0.3,<1.0",
        "httpx>=0.27,<1.0",
        "tenacity>=9.0,<10.0",
        "tiktoken>=0.7,<1.0",
        "numpy>=1.26,<3.0",
        "pandas>=2.2,<3.0",
        "matplotlib>=3.9,<4.0",
        "pydantic>=2.8,<3.0",
        "google-cloud-storage>=2.18,<3.0",
        "huggingface-hub>=0.24,<1.0",
        "datasets>=3.0,<4.0",
        "python-dotenv>=1.0,<2.0",
    )
    # Mount the eval/ source so the workers can import the same
    # benchmarks / judges / rubric modules the local CLI uses.
    .add_local_python_source("eval")
)

app = modal.App("router-eval-harness", image=image)

anthropic_secret = modal.Secret.from_name("anthropic-api-key")
openai_secret = modal.Secret.from_name("openai-api-key")
google_secret = modal.Secret.from_name("google-api-key")
gcp_secret = modal.Secret.from_name("gcp-credentials")
router_secret = modal.Secret.from_name("weave-router-eval-key")

# Forward EVAL_GEMINI_VERTEX / GOOGLE_CLOUD_PROJECT / GOOGLE_CLOUD_LOCATION
# from the local operator environment to Modal workers. Modal secrets
# don't pick up local .env vars automatically, so without this the
# Vertex billing path in GeminiJudge is silently unreachable on Modal.
# Built at module-import time; missing keys are simply omitted.
_VERTEX_ENV_KEYS = (
    "EVAL_GEMINI_VERTEX",
    "GOOGLE_CLOUD_PROJECT",
    "GOOGLE_CLOUD_LOCATION",
)
vertex_env_secret = modal.Secret.from_dict(
    {k: v for k in _VERTEX_ENV_KEYS if (v := os.environ.get(k))}
)

INFERENCE_SECRETS = [anthropic_secret, gcp_secret, router_secret]
JUDGE_SECRETS = [openai_secret, google_secret, gcp_secret, vertex_env_secret]
AGGREGATE_SECRETS = [gcp_secret]

DEFAULT_GCS_PREFIX = "gs://workweave-prod-01-models-v2-training/router-eval"


# ---------------------------------------------------------------------------
# Modal-side helpers (need to import inside the function so the worker
# image, not the local Python, hosts them).
# ---------------------------------------------------------------------------


def _gcs_prefix() -> str:
    return os.environ.get("EVAL_GCS_PREFIX", DEFAULT_GCS_PREFIX).rstrip("/")


def _ensure_gcp_adc() -> None:
    """Bridge Workweave's Modal secret shape to Google ADC.

    The `gcp-credentials` Modal secret exposes the service-account
    JSON as `GOOGLE_APPLICATION_CREDENTIALS_JSON`, but the google-cloud
    SDKs expect `GOOGLE_APPLICATION_CREDENTIALS` pointing at a file.
    Materialise the JSON to a temp file once per worker container and
    point the SDK at it. No-op if ADC is already wired (local dev with
    `gcloud auth application-default login`)."""
    import atexit
    import tempfile

    if os.environ.get("GOOGLE_APPLICATION_CREDENTIALS"):
        return
    blob = os.environ.get("GOOGLE_APPLICATION_CREDENTIALS_JSON")
    if not blob:
        return
    fd, path = tempfile.mkstemp(suffix=".json", prefix="adc-")
    with os.fdopen(fd, "w") as f:
        f.write(blob)
    os.environ["GOOGLE_APPLICATION_CREDENTIALS"] = path
    # mkstemp creates the file 0600, so exposure is bounded to the
    # process UID, but unlinking on exit shrinks the window further.
    atexit.register(lambda p=path: os.unlink(p) if os.path.exists(p) else None)


def _gcs_blob(path: str):
    """Return a google-cloud-storage Blob handle for `path`."""
    from google.cloud import storage  # type: ignore[import-untyped]

    if not path.startswith("gs://"):
        raise ValueError(f"GCS path must start with gs://; got {path}")
    _ensure_gcp_adc()
    bucket_name, _, key = path[len("gs://") :].partition("/")
    client = storage.Client()
    return client.bucket(bucket_name).blob(key)


def _gcs_exists(path: str) -> bool:
    return _gcs_blob(path).exists()


def _gcs_write_text(path: str, text: str) -> None:
    _gcs_blob(path).upload_from_string(text)


def _gcs_append_jsonl(path: str, row: dict[str, Any]) -> None:
    """Append-by-overwrite: each (run_id, key) row writes a unique
    sub-blob, and the aggregate step concatenates them. Modal
    workers running in parallel can't safely append to a single GCS
    object."""
    _gcs_write_text(path, json.dumps(row) + "\n")


# ---------------------------------------------------------------------------
# run_inference
# ---------------------------------------------------------------------------


@app.function(
    secrets=INFERENCE_SECRETS,
    timeout=300,
    max_containers=50,
    cpu=4,
)
async def run_inference(args: dict[str, Any]) -> str:
    """Returns the GCS object key written.

    Args is a dict (single positional) so the local entrypoint can
    fan out via `run_inference.map(args_list)` — Modal's `.map` /
    `.starmap` don't bind keyword-only parameters, so per-call
    arguments are packed into a dict here.
    """
    _ensure_gcp_adc()
    from eval.pricing import estimate_cost
    from eval.routing import route
    from eval.types import InferenceRow

    run_id = args["run_id"]
    prompt_id = args["prompt_id"]
    prompt_text = args["prompt_text"]
    router = args["router"]
    force = args.get("force", False)

    key = f"{_gcs_prefix()}/{run_id}/inference/{prompt_id}__{router}.jsonl"
    if not force and _gcs_exists(key):
        return key

    res = await route(router=router, prompt=prompt_text)  # type: ignore[arg-type]
    cost = (
        estimate_cost(res.model_used, res.input_tokens, res.output_tokens)
        if res.model_used
        else 0.0
    )
    row = InferenceRow(
        run_id=run_id,
        prompt_id=prompt_id,
        router=router,  # type: ignore[arg-type]
        model_used=res.model_used,
        output_text=res.output_text,
        input_tokens=res.input_tokens,
        output_tokens=res.output_tokens,
        latency_ms=res.latency_ms,
        cost_usd=cost,
        error=res.error,
    )
    _gcs_append_jsonl(key, row.model_dump())
    return key


# ---------------------------------------------------------------------------
# run_judge
# ---------------------------------------------------------------------------


@app.function(
    secrets=JUDGE_SECRETS,
    timeout=300,
    max_containers=50,
    cpu=4,
)
async def run_judge(args: dict[str, Any]) -> str:
    """One pairwise judgment. Single-dict positional arg for the same
    Modal `.map` reason as `run_inference`."""
    _ensure_gcp_adc()
    from eval.judges import Judge
    from eval.judges.gemini import GeminiJudge
    from eval.judges.gpt5 import GPT5Judge
    from eval.judges.ensemble import judge_pair_ensemble

    run_id = args["run_id"]
    prompt_id = args["prompt_id"]
    prompt_text = args["prompt_text"]
    judge = args["judge"]
    candidate_router = args["candidate_router"]
    candidate_text = args["candidate_text"]
    baseline_router = args["baseline_router"]
    baseline_text = args["baseline_text"]
    force = args.get("force", False)

    key = f"{_gcs_prefix()}/{run_id}/judgments/{prompt_id}__{judge}__{candidate_router}.jsonl"
    if not force and _gcs_exists(key):
        return key

    judges: list[Judge]
    if judge == "gpt5":
        judges = [GPT5Judge()]
    elif judge == "gemini":
        judges = [GeminiJudge()]
    else:
        raise ValueError(f"unknown judge {judge!r}")

    # Even with a single judge the ensemble helper does the swap-and-
    # parse mechanics; we just don't get a disagreement signal.
    result = await judge_pair_ensemble(
        judges=judges,
        prompt=prompt_text,
        baseline_text=baseline_text,
        candidate_text=candidate_text,
        prompt_id=prompt_id,
        run_id=run_id,
        candidate_router=candidate_router,  # type: ignore[arg-type]
        baseline_router=baseline_router,  # type: ignore[arg-type]
    )
    # One row per judgment — the ensemble entry point is shared with
    # the local CLI but here we only fan out one judge per call.
    _gcs_append_jsonl(key, result.judgments[0].model_dump())
    return key


# ---------------------------------------------------------------------------
# aggregate
# ---------------------------------------------------------------------------


@app.function(secrets=AGGREGATE_SECRETS, timeout=1800, cpu=1)
def aggregate(*, run_id: str) -> str:
    """Pulls all inference + judgment rows, builds RouterResults,
    writes Pareto + per-router table to GCS. Returns the GCS path
    of the rendered EVAL_RESULTS.md candidate."""
    _ensure_gcp_adc()
    from io import BytesIO

    from google.cloud import storage  # type: ignore[import-untyped]

    from eval.aggregation import aggregate_to_router_results
    from eval.pareto import render_plot, render_table, to_points
    from eval.types import InferenceRow, JudgmentRow

    bucket_name, _, prefix_path = _gcs_prefix()[len("gs://") :].partition("/")
    client = storage.Client()
    bucket = client.bucket(bucket_name)

    inference_rows = list(
        _load_jsonl(bucket, f"{prefix_path}/{run_id}/inference/", InferenceRow)
    )
    judgment_rows = list(
        _load_jsonl(bucket, f"{prefix_path}/{run_id}/judgments/", JudgmentRow)
    )

    results = aggregate_to_router_results(
        inference_rows, judgment_rows, baseline_router="always-opus"
    )
    # Render + upload artifacts.
    fig = render_plot(
        to_points(results), title=f"router-eval/{run_id}: cost vs quality"
    )
    buf = BytesIO()
    fig.savefig(buf, format="png", dpi=150)
    buf.seek(0)
    pareto_key = f"{prefix_path}/{run_id}/pareto.png"
    bucket.blob(pareto_key).upload_from_string(buf.getvalue(), content_type="image/png")

    md_key = f"{prefix_path}/{run_id}/eval_results.md"
    md_body = (
        f"# router-eval/{run_id}\n\n"
        f"![Pareto plot](pareto.png)\n\n"
        f"{render_table(results)}\n"
    )
    bucket.blob(md_key).upload_from_string(md_body)

    aggregated_key = f"{prefix_path}/{run_id}/aggregated.json"
    bucket.blob(aggregated_key).upload_from_string(
        json.dumps([r.model_dump() for r in results], indent=2)
    )
    return f"gs://{bucket_name}/{md_key}"


def _load_jsonl(bucket, prefix: str, cls):
    for blob in bucket.list_blobs(prefix=prefix):
        if not blob.name.endswith(".jsonl"):
            continue
        for line in blob.download_as_text().splitlines():
            line = line.strip()
            if not line:
                continue
            yield cls.model_validate_json(line)


# ---------------------------------------------------------------------------
# Local entrypoint: orchestrates the whole run.
# ---------------------------------------------------------------------------


@app.local_entrypoint()
def main(
    smoke: bool = False,
    run_id: str | None = None,
    force: bool = False,
    aggregate_only: bool = False,
) -> None:
    """Stand up a fresh run.

    --smoke: 10 prompts, [opus, heuristic, v0.2-cluster] x [gpt5] only.
             End-to-end Modal invocation; cheap; proves secrets mount,
             GCS write, output parses.

    --aggregate-only: skip inference + judging; just run aggregate()
             on the supplied --run-id. Used to recover after a local
             heartbeat blip — Modal kept the workers running and wrote
             every row to GCS, but the local entrypoint died before
             dispatching aggregate(). Pass the existing run_id and the
             aggregate function reads back the rows and emits the
             Pareto plot + EVAL_RESULTS.md candidate.

    Without --smoke or --aggregate-only, runs the full slice composition
    from eval.slice_plan with all five routers and both judges.
    """
    from eval.benchmarks import get
    from eval.judges.ensemble import (
        DISAGREEMENT_THRESHOLD,
    )  # noqa: F401  documents the protocol
    from eval.manifest import build_manifest
    from eval.slice_plan import SLICES
    from eval.types import RouterName

    if aggregate_only:
        if not run_id:
            raise SystemExit("--aggregate-only requires --run-id")
        print(f"[{run_id}] aggregate-only mode")
        result_path = aggregate.remote(run_id=run_id)
        print(f"[{run_id}] DONE  EVAL_RESULTS.md candidate at {result_path}")
        return

    run_id = run_id or f"run-{uuid.uuid4().hex[:10]}"
    print(f"[{run_id}] starting; smoke={smoke}, force={force}")

    if smoke:
        prompts = []
        for spec in SLICES[:3]:
            loader = get(spec.loader)
            try:
                prompts.extend(loader.load(n=4, seed=42))
            except Exception as e:
                print(f"[{run_id}] WARN smoke skipping slice {spec.slice}: {e}")
            if len(prompts) >= 10:
                break
        prompts = prompts[:10]
        routers: list[RouterName] = ["always-opus", "always-haiku", "v0.6-cluster"]
        judges = ["gpt5"]
    else:
        prompts = []
        for spec in SLICES:
            loader = get(spec.loader)
            try:
                prompts.extend(loader.load(n=spec.count, seed=42))
            except Exception as e:
                # Mirror the smoke path: skip slices whose loader fails
                # (e.g. gated HF datasets like Aider-AI/polyglot-benchmark
                # without an HF_TOKEN). The other slices give plenty of
                # signal; failing the whole run on one missing dataset
                # is overkill.
                print(f"[{run_id}] WARN skipping slice {spec.slice}: {e}")
        # Older artifacts (v0.1–v0.4) and the heuristic are covered by
        # the bench-holdout regret eval (`scripts/holdout_eval.py`); the
        # Modal harness adds the judge-ensemble dimension only. v0.6 is
        # the current latest (Phase 4 per-cluster α retrain); v0.5 is
        # included for the head-to-head comparison the EVAL_RESULTS.md
        # writeup references. always-opus is the judge baseline;
        # always-haiku is the cheap-tier Pareto anchor.
        routers = [
            "always-opus",
            "always-haiku",
            "v0.5-cluster",
            "v0.6-cluster",
        ]
        judges = ["gpt5", "gemini"]

    print(
        f"[{run_id}] loaded {len(prompts)} prompts across {len(routers)} routers x {len(judges)} judges"
    )

    # Upload prompts.jsonl + manifest.json so a re-run can read them
    # back without re-loading from HF.
    base = f"{_gcs_prefix()}/{run_id}"
    _gcs_write_text(
        f"{base}/prompts.jsonl",
        "\n".join(p.model_dump_json() for p in prompts) + "\n",
    )
    _gcs_write_text(
        f"{base}/manifest.json",
        json.dumps(build_manifest(run_id=run_id, prompts=prompts), indent=2),
    )

    # Fan out inference. Each (prompt, router) is one Modal call.
    inf_args = [
        {
            "run_id": run_id,
            "prompt_id": p.prompt_id,
            "prompt_text": p.prompt_text,
            "router": r,
            "force": force,
        }
        for p in prompts
        for r in routers
    ]
    print(f"[{run_id}] dispatching {len(inf_args)} inference calls")
    list(run_inference.map(inf_args))

    # Pull inference outputs back so we can build the judge fan-out.
    print(f"[{run_id}] reading inference results back from GCS")
    inference_by = _read_inference_back(run_id, [p.prompt_id for p in prompts], routers)
    baseline_router: RouterName = "always-opus"

    # Fan out judging. Each (prompt, candidate, judge) is one Modal call.
    candidates = [r for r in routers if r != baseline_router]
    judge_args = []
    for p in prompts:
        baseline_text = inference_by.get((p.prompt_id, baseline_router), "")
        for c in candidates:
            cand_text = inference_by.get((p.prompt_id, c), "")
            for j in judges:
                judge_args.append(
                    {
                        "run_id": run_id,
                        "prompt_id": p.prompt_id,
                        "prompt_text": p.prompt_text,
                        "judge": j,
                        "candidate_router": c,
                        "candidate_text": cand_text,
                        "baseline_router": baseline_router,
                        "baseline_text": baseline_text,
                        "force": force,
                    }
                )
    print(f"[{run_id}] dispatching {len(judge_args)} judge calls")
    list(run_judge.map(judge_args))

    print(f"[{run_id}] aggregating")
    md_path = aggregate.remote(run_id=run_id)
    print(f"[{run_id}] DONE  EVAL_RESULTS.md candidate at {md_path}")


def _read_inference_back(
    run_id: str, prompt_ids: list[str], routers: list[str]
) -> dict[tuple[str, str], str]:
    """For the local entry point: pull back each inference row's text
    so the judge dispatch can include it in the call args (Modal
    workers shouldn't have to download per-row blobs again)."""
    from google.cloud import storage  # type: ignore[import-untyped]

    bucket_name, _, prefix_path = _gcs_prefix()[len("gs://") :].partition("/")
    client = storage.Client()
    bucket = client.bucket(bucket_name)
    out: dict[tuple[str, str], str] = {}
    for pid in prompt_ids:
        for r in routers:
            key = f"{prefix_path}/{run_id}/inference/{pid}__{r}.jsonl"
            blob = bucket.blob(key)
            if not blob.exists():
                continue
            for line in blob.download_as_text().splitlines():
                line = line.strip()
                if not line:
                    continue
                row = json.loads(line)
                out[(pid, r)] = row.get("output_text", "")
    return out
