"""RouterArena response grader — official methodology.

Wraps the vendored RouterArena scoring code (under
``_routerarena_official/``, Apache-2.0) so our harness produces the
same per-row score the leaderboard would. The vendored module is
upstream's source of truth; do not reimplement metrics here.

Per-dataset metric mapping is taken verbatim from RouterArena's
``config/eval_config/zero-shot/*.json`` files (commit pulled from
github.com/RouteWorks/RouterArena around the date of this run; bump
when re-vendoring).

Returns ``GradeResult`` with a continuous ``score`` (0–1) so METEOR
graders contribute partial credit, matching the leaderboard's
"Accuracy" column.
"""

from __future__ import annotations

import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Any

# Vendored package import. Must be added to sys.path before importing
# `metrics` because their files use bare imports (no package prefix).
_VENDOR_DIR = Path(__file__).parent / "_routerarena_official"
if str(_VENDOR_DIR) not in sys.path:
    sys.path.insert(0, str(_VENDOR_DIR))

from metrics import (  # noqa: E402  — import after sys.path mutation
    chess_accuracy,
    exact_match,
    math_metric,
    mcq_accuracy,
    meteor_score,
    superglue_clozetest,
    superglue_exact_match,
)


@dataclass
class GradeResult:
    score: float            # 0..1; 0/1 for MCQ-style, continuous for METEOR
    correct: bool           # score >= CORRECTNESS_THRESHOLD
    gradeable: bool         # False for unsupported datasets / empty responses
    mode: str               # which official metric ran
    extracted: str = ""
    expected: str = ""


# Threshold to convert continuous METEOR scores → binary "correct" for
# our backwards-compat ``correct`` boolean. RouterArena's leaderboard
# uses the continuous score directly in its average, so the
# authoritative number is ``score`` (we just expose ``correct`` for
# anyone consuming the older field).
CORRECTNESS_THRESHOLD = 0.5


# Map from RouterArena ``Dataset name`` (the column in the parquet) to
# the metric function the leaderboard runs. Composite dataset names
# (e.g. ``MMLUPro_computer science``) map by their parent prefix —
# matches their ``utils.py::load_data``'s ``str.contains`` behavior.
_PREFIX_METRICS: dict[str, str] = {
    # MCQ — boxed letter extraction + exact match against gold letter
    "ArcMMLU": "mcq_accuracy",
    "MMLUPro": "mcq_accuracy",
    "MMLU": "mcq_accuracy",
    "OpenTDB": "mcq_accuracy",
    "Ethics": "mcq_accuracy",
    "MathQA": "mcq_accuracy",
    "MedMCQA": "mcq_accuracy",
    "PubMedQA": "mcq_accuracy",
    "SocialiQA": "mcq_accuracy",
    "GeoBench": "mcq_accuracy",
    "MusicTheoryBench": "mcq_accuracy",
    "ChessInstruct_mcq": "mcq_accuracy",
    "SuperGLUE-CausalReasoning": "mcq_accuracy",
    # GPQA — boxed letter MCQ per upstream prompt_templates.json,
    # listed defensively in case the dataset adds GPQA rows in a
    # future version (current snapshot has none).
    "GPQA": "mcq_accuracy",

    # Math — boxed extraction + sympy symbolic equality
    "GSM8K": "math_metric",
    "MATH": "math_metric",
    "AIME": "math_metric",
    "AsDiv": "math_metric",
    "FinQA": "math_metric",

    # QANTA — boxed extraction + normalize_qanta_answer + exact match
    "QANTA": "exact_match",

    # SuperGLUE Yes/No / 0.0/1.0 — boxed + format conversion. RC, Wsc,
    # Wic, Entailment, QA all share the same metric per their config
    # files; only CausalReasoning (above) and ClozeTest are different.
    "SuperGLUE-RC": "superglue_exact_match",
    "SuperGLUE-Wsc": "superglue_exact_match",
    "SuperGLUE-Wic": "superglue_exact_match",
    "SuperGLUE-Entailment": "superglue_exact_match",
    "SuperGLUE-QA": "superglue_exact_match",
    "SuperGLUE-ClozeTest": "superglue_clozetest",

    # METEOR — translation / free-form QA. Continuous 0..1 score.
    "WMT19": "meteor_score",
    "NarrativeQA": "meteor_score",

    # Per their config: GeoGraphyData uses exact_match (QANTA-style
    # boxed extraction + normalize), ChessInstruct uses chess_accuracy
    # (which expects "a2a3"-style coordinate notation in the box).
    "GeoGraphyData": "exact_match",
    "ChessInstruct": "chess_accuracy",

    # LiveCodeBench requires sandboxed code execution against private
    # test cases — implementation pending, tracked separately. For now
    # we leave it ungradeable (same as our shortcut run; downstream
    # treats ``gradeable=False`` as excluded from the headline accuracy
    # rather than counted as 0). Restoring it adds ~385 prompts to the
    # gradeable pool.
    "LiveCodeBench": "ungradeable_pending_codeexec",
}

_METRIC_FNS = {
    "mcq_accuracy": mcq_accuracy,
    "math_metric": math_metric,
    "exact_match": exact_match,
    "meteor_score": meteor_score,
    "superglue_exact_match": superglue_exact_match,
    "superglue_clozetest": superglue_clozetest,
    "chess_accuracy": chess_accuracy,
}


def metric_for(dataset_name: str) -> str:
    """Resolve a RouterArena ``Dataset name`` to its official metric.

    Falls back to ``"unknown"`` if no mapping rule matches — the caller
    should treat that as ungradeable rather than guessing.
    """
    for prefix, metric in _PREFIX_METRICS.items():
        if dataset_name.startswith(prefix):
            return metric
    return "unknown"


def grade(
    *,
    dataset_name: str,
    gold_answer: Any,
    response: str,
    options: Any = None,  # unused; kept for API compat with the old harness
) -> GradeResult:
    metric = metric_for(dataset_name)
    if metric == "unknown" or metric == "ungradeable_pending_codeexec":
        return GradeResult(0.0, False, False, metric, expected=str(gold_answer)[:80])

    gold = "" if gold_answer is None else str(gold_answer)
    resp = (response or "").strip()
    if not resp:
        return GradeResult(0.0, False, False, "empty_response", expected=gold[:80])

    fn = _METRIC_FNS[metric]
    try:
        score, raw = fn([resp], [gold])
    except Exception as e:
        return GradeResult(
            0.0, False, False, f"error:{type(e).__name__}",
            expected=gold[:80], extracted=f"{e}"[:80],
        )

    extracted = ""
    if raw:
        first = raw[0]
        if isinstance(first, dict):
            extracted = str(first.get("extracted_answer", first.get("extracted", "")))[:120]

    score_f = float(score)
    return GradeResult(
        score=score_f,
        correct=score_f >= CORRECTNESS_THRESHOLD,
        gradeable=True,
        mode=metric,
        extracted=extracted,
        expected=gold[:80],
    )
