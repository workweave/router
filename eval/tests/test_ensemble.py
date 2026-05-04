"""Unit tests for the judge ensemble (median + disagreement flag)."""

import pytest

from eval.judges import Judge
from eval.judges.ensemble import DISAGREEMENT_THRESHOLD, judge_pair_ensemble
from eval.types import RubricScores


class FakeJudge(Judge):
    """Returns predetermined scores. Tests inspect order-randomization
    by setting different a/b scores in the constructor."""

    def __init__(self, name: str, a_value: float, b_value: float):
        self._name = name
        self._a = a_value
        self._b = b_value
        self.calls: list[tuple[str, str]] = []

    @property
    def name(self) -> str:  # type: ignore[override]
        return self._name

    async def judge_pair(self, *, prompt, response_a, response_b):
        self.calls.append((response_a, response_b))
        a = RubricScores(correctness=self._a, completeness=self._a, code_quality=self._a, style=self._a, follows_instructions=self._a)
        b = RubricScores(correctness=self._b, completeness=self._b, code_quality=self._b, style=self._b, follows_instructions=self._b)
        return a, b, "rationale", "{}"


@pytest.mark.asyncio
async def test_ensemble_takes_median_of_judges():
    j1 = FakeJudge("gpt5", a_value=2.0, b_value=4.0)  # candidate score depends on swap
    j2 = FakeJudge("gemini", a_value=2.0, b_value=4.0)
    res = await judge_pair_ensemble(
        judges=[j1, j2],
        prompt="p", baseline_text="bbb", candidate_text="ccc",
        prompt_id="pid-1", run_id="r", candidate_router="heuristic", baseline_router="always-opus",
    )
    # Both judges score the candidate identically -> median == that score.
    assert res.median_score == res.judgments[0].score
    assert len(res.judgments) == 2


@pytest.mark.asyncio
async def test_ensemble_flags_disagreement_above_threshold():
    j_high = FakeJudge("gpt5", a_value=5.0, b_value=5.0)   # candidate scores 1.0
    j_low = FakeJudge("gemini", a_value=1.0, b_value=1.0)  # candidate scores 0.2
    res = await judge_pair_ensemble(
        judges=[j_high, j_low],
        prompt="p", baseline_text="b", candidate_text="c",
        prompt_id="pid-2", run_id="r", candidate_router="v0.2-cluster", baseline_router="always-opus",
    )
    spread = abs(res.judgments[0].score - res.judgments[1].score)
    assert spread > DISAGREEMENT_THRESHOLD
    assert res.flag is True


@pytest.mark.asyncio
async def test_ensemble_does_not_flag_when_within_threshold():
    j1 = FakeJudge("gpt5", a_value=3.0, b_value=3.0)   # candidate scores 0.6
    j2 = FakeJudge("gemini", a_value=4.0, b_value=4.0)  # candidate scores 0.8
    res = await judge_pair_ensemble(
        judges=[j1, j2],
        prompt="p", baseline_text="b", candidate_text="c",
        prompt_id="pid-3", run_id="r", candidate_router="v0.2-cluster", baseline_router="always-opus",
    )
    spread = abs(res.judgments[0].score - res.judgments[1].score)
    assert spread <= DISAGREEMENT_THRESHOLD
    assert res.flag is False


@pytest.mark.asyncio
async def test_ensemble_swap_is_deterministic_per_prompt_id():
    """Two runs over the same prompt_id must produce the same A/B
    assignment for the judge — otherwise determinism check breaks."""
    judge1 = FakeJudge("gpt5", a_value=3.0, b_value=3.0)
    judge2 = FakeJudge("gpt5", a_value=3.0, b_value=3.0)
    await judge_pair_ensemble(
        judges=[judge1],
        prompt="p", baseline_text="BASELINE", candidate_text="CANDIDATE",
        prompt_id="determinism-test", run_id="r1", candidate_router="v0.2-cluster", baseline_router="always-opus",
    )
    await judge_pair_ensemble(
        judges=[judge2],
        prompt="p", baseline_text="BASELINE", candidate_text="CANDIDATE",
        prompt_id="determinism-test", run_id="r2", candidate_router="v0.2-cluster", baseline_router="always-opus",
    )
    assert judge1.calls[0] == judge2.calls[0]
