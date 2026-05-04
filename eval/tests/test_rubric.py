"""Unit tests for the rubric parser + aggregation."""

import pytest

from eval.rubric import DIMENSIONS, aggregate, parse_judge_response
from eval.types import RubricScores


def _scores(**kwargs):
    base = {d: 3.0 for d in DIMENSIONS}
    base.update(kwargs)
    return RubricScores(**base)


def test_aggregate_normalizes_to_zero_one():
    # 1-5 rubric mapped onto [0, 1] via (mean - 1) / 4.
    assert aggregate(_scores()) == pytest.approx(0.5)  # mean 3 → 0.5
    assert aggregate(_scores(correctness=5, completeness=5, code_quality=5, style=5, follows_instructions=5)) == pytest.approx(1.0)
    assert aggregate(_scores(correctness=1, completeness=1, code_quality=1, style=1, follows_instructions=1)) == pytest.approx(0.0)


def test_aggregate_uses_mean_not_min_or_max():
    # Median of (5, 5, 1, 1, 1) is 1; mean is 2.6 — picking values where
    # mean ≠ median means a median-based implementation would not pass
    # this test.
    s = _scores(correctness=5, completeness=5, code_quality=1, style=1, follows_instructions=1)
    assert aggregate(s) == pytest.approx((2.6 - 1.0) / 4.0)


def _judge_json(a_score: float, b_score: float) -> str:
    return f"""{{
      "response_a": {{
        "correctness": {a_score},
        "completeness": {a_score},
        "code_quality": {a_score},
        "style": {a_score},
        "follows_instructions": {a_score},
        "rationale": "fake A"
      }},
      "response_b": {{
        "correctness": {b_score},
        "completeness": {b_score},
        "code_quality": {b_score},
        "style": {b_score},
        "follows_instructions": {b_score},
        "rationale": "fake B"
      }}
    }}"""


def test_parse_judge_response_extracts_both_sides():
    a, b, rationale = parse_judge_response(_judge_json(4, 2))
    assert a.correctness == 4 and b.correctness == 2
    assert "fake A" in rationale and "fake B" in rationale


def test_parse_judge_response_strips_markdown_fences():
    raw = "```json\n" + _judge_json(3, 5) + "\n```"
    a, b, _ = parse_judge_response(raw)
    assert a.correctness == 3 and b.correctness == 5


def test_parse_judge_response_tolerates_leading_prose():
    raw = "Sure! Here's the rubric:\n" + _judge_json(2, 4)
    a, b, _ = parse_judge_response(raw)
    assert a.correctness == 2 and b.correctness == 4


def test_parse_judge_response_rejects_out_of_range():
    bad = _judge_json(7, 3)
    with pytest.raises(ValueError):
        parse_judge_response(bad)


def test_parse_judge_response_rejects_missing_dim():
    raw = """{
      "response_a": {"correctness": 3, "completeness": 3, "code_quality": 3, "style": 3, "rationale": "x"},
      "response_b": {"correctness": 3, "completeness": 3, "code_quality": 3, "style": 3, "follows_instructions": 3, "rationale": "y"}
    }"""
    with pytest.raises(ValueError):
        parse_judge_response(raw)


def test_parse_judge_response_rejects_no_json():
    with pytest.raises(ValueError):
        parse_judge_response("the model decided not to follow instructions")
