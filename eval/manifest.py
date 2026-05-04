"""manifest.json writer.

Captures everything needed to interpret a run after the fact:
- Frozen prompt slice composition + total
- prompt_set_hash (SHA-256 of the sorted prompt_id+prompt_text pairs)
- Model versions (Anthropic IDs)
- Judge versions (OpenAI / Google model IDs)
- Cluster artifact hashes (read from router/internal/router/cluster/assets/
  if accessible; left blank otherwise — the operator fills it in)
- Run-time env (which staging URL was hit, etc.)
"""

from __future__ import annotations

import hashlib
import json
import os
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from eval.judges.gpt5 import DEFAULT_MODEL as DEFAULT_GPT5_MODEL
from eval.judges.gemini import DEFAULT_MODEL as DEFAULT_GEMINI_MODEL
from eval.slice_plan import SLICES, TOTAL_PROMPTS
from eval.types import BenchmarkPrompt

# Cluster artifacts (centroids.bin, rankings.json, model_registry.json)
# live directly in the cluster package — they're Go-embedded, not under
# an `assets/` subdirectory.
CLUSTER_ASSETS_DIR = (
    Path(__file__).resolve().parent.parent / "internal" / "router" / "cluster"
)


def build_manifest(*, run_id: str, prompts: list[BenchmarkPrompt]) -> dict[str, Any]:
    """Synthesize the manifest for a single run."""
    prompt_set_hash = _hash_prompts(prompts)
    return {
        "run_id": run_id,
        "created_at": datetime.now(UTC).isoformat(),
        "prompt_count": len(prompts),
        "prompt_count_target": TOTAL_PROMPTS,
        "prompt_set_hash": prompt_set_hash,
        "slices": [
            {"slice": s.slice, "loader": s.loader, "count": s.count, "rationale": s.rationale}
            for s in SLICES
        ],
        "models": {
            "opus": "claude-opus-4-7",
            "sonnet": "claude-sonnet-4-5",
            "haiku": "claude-haiku-4-5",
        },
        "judges": {
            "gpt5": os.environ.get("EVAL_GPT5_MODEL") or DEFAULT_GPT5_MODEL,
            "gemini": os.environ.get("EVAL_GEMINI_MODEL") or DEFAULT_GEMINI_MODEL,
        },
        "router_base_url": os.environ.get("ROUTER_BASE_URL", "https://router-staging.workweave.ai"),
        "cluster_artifacts": _hash_cluster_artifacts(),
    }


def _hash_prompts(prompts: list[BenchmarkPrompt]) -> str:
    h = hashlib.sha256()
    for p in sorted(prompts, key=lambda p: p.prompt_id):
        h.update(p.prompt_id.encode("utf-8"))
        h.update(b"\x00")
        h.update(p.prompt_text.encode("utf-8"))
        h.update(b"\x00")
    return h.hexdigest()


def _hash_cluster_artifacts() -> dict[str, str]:
    """Best-effort SHA-256 of the committed cluster artifacts so a
    re-run is traceable to the exact cluster config under test."""
    out: dict[str, str] = {}
    if not CLUSTER_ASSETS_DIR.exists():
        return out
    for name in ("centroids.bin", "rankings.json", "model_registry.json"):
        path = CLUSTER_ASSETS_DIR / name
        if not path.exists():
            continue
        try:
            data = path.read_bytes()
        except OSError:
            # Best-effort: a permissions / I/O failure shouldn't block
            # manifest generation. The operator can fill in the hash by
            # hand if it matters for the run.
            continue
        h = hashlib.sha256()
        h.update(data)
        out[name] = h.hexdigest()
    return out


def write_manifest(*, run_id: str, prompts: list[BenchmarkPrompt], output_path: Path) -> None:
    manifest = build_manifest(run_id=run_id, prompts=prompts)
    output_path.parent.mkdir(parents=True, exist_ok=True)
    output_path.write_text(json.dumps(manifest, indent=2, sort_keys=True))
