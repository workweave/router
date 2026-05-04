"""Cross-family judge ensemble: median + disagreement flag.

Anti-bias measures from the LLM-judge literature applied here:

  - Order randomization (per-prompt, seeded by prompt_hash so re-runs
    are deterministic).
  - Identity stripping (the rubric prompt template only labels
    responses A / B, never names the model).
  - Two judges, take median of normalized scores.
  - Flag when |gpt5.score - gemini.score| > DISAGREEMENT_THRESHOLD so
    spot_check.py can prioritize them.
"""

from __future__ import annotations

import hashlib
import random
import statistics
from dataclasses import dataclass

from eval.judges import Judge
from eval.rubric import aggregate
from eval.types import JudgmentRow, RouterName, RubricScores

DISAGREEMENT_THRESHOLD = 0.30


@dataclass
class EnsembleResult:
    median_score: float
    judgments: list[JudgmentRow]  # one per judge
    flag: bool


def _seeded_swap(prompt_hash: str) -> bool:
    """Deterministic coin flip for "should we swap A/B before judging?"

    Returns True iff baseline (always-Opus) goes into slot B; False if
    it stays in slot A. Per-prompt seeded so re-runs are stable.
    """
    rng = random.Random(int(hashlib.sha256(prompt_hash.encode()).hexdigest(), 16))
    return rng.random() < 0.5


async def judge_pair_ensemble(
    *,
    judges: list[Judge],
    prompt: str,
    baseline_text: str,
    candidate_text: str,
    prompt_id: str,
    run_id: str,
    candidate_router: RouterName,
    baseline_router: RouterName,
) -> EnsembleResult:
    """Fan both judges over a (baseline, candidate) pair and ensemble.

    The swap mechanic decides whether the candidate is shown in slot A
    or slot B; the resulting scores are read back from the
    correct-for-the-candidate slot. The swap is seeded by prompt_id so
    runs are reproducible.
    """
    swap = _seeded_swap(prompt_id)
    a_text, b_text = (baseline_text, candidate_text) if swap else (candidate_text, baseline_text)

    rows: list[JudgmentRow] = []
    candidate_scores: list[float] = []

    # Sequential rather than asyncio.gather to keep error attribution
    # simple — the judges are minutes apart in latency, parallelism
    # is fine but a single slow judge shouldn't block the row.
    for judge in judges:
        try:
            scores_a, scores_b, rationale, raw = await judge.judge_pair(
                prompt=prompt, response_a=a_text, response_b=b_text
            )
            candidate_rubric: RubricScores = scores_b if swap else scores_a
            score = aggregate(candidate_rubric)
            rows.append(
                JudgmentRow(
                    run_id=run_id,
                    prompt_id=prompt_id,
                    judge=judge.name,  # type: ignore[arg-type]
                    candidate_router=candidate_router,
                    baseline_router=baseline_router,
                    rubric=candidate_rubric,
                    score=score,
                    rationale=rationale,
                    raw_response=raw,
                )
            )
            candidate_scores.append(score)
        except Exception as e:
            rows.append(
                JudgmentRow(
                    run_id=run_id,
                    prompt_id=prompt_id,
                    judge=judge.name,  # type: ignore[arg-type]
                    candidate_router=candidate_router,
                    baseline_router=baseline_router,
                    # Use the rubric minimum (1) rather than 0 so any
                    # downstream re-aggregation yields the floor of [0, 1]
                    # (0.0) instead of a negative sentinel like -0.25.
                    rubric=RubricScores(
                        correctness=1, completeness=1, code_quality=1, style=1, follows_instructions=1
                    ),
                    score=0.0,
                    rationale="",
                    raw_response="",
                    error=str(e),
                )
            )
    if not candidate_scores:
        return EnsembleResult(median_score=0.0, judgments=rows, flag=True)
    median = statistics.median(candidate_scores)
    flag = (max(candidate_scores) - min(candidate_scores)) > DISAGREEMENT_THRESHOLD
    return EnsembleResult(median_score=median, judgments=rows, flag=flag)
