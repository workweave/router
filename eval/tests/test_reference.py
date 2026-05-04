"""Unit tests for reference grading."""

import pytest

from eval.reference import grade
from eval.types import Reference


def test_no_reference_returns_none():
    assert grade(None, "anything") is None
    assert grade(Reference(kind="none", payload={}), "anything") is None


def test_numeric_match_extracts_last_number():
    ref = Reference(kind="numeric_match", payload={"answer": "42"})
    assert grade(ref, "Let me think... The answer is 42.") is True
    assert grade(ref, "I think it's 7") is False


def test_numeric_match_handles_negative_and_decimal():
    ref = Reference(kind="numeric_match", payload={"answer": "-3.14"})
    assert grade(ref, "= -3.14") is True


def test_numeric_match_strips_thousands_commas():
    ref = Reference(kind="numeric_match", payload={"answer": "1,200"})
    assert grade(ref, "Approximately 1,200 widgets.") is True


def test_numeric_match_handles_multiple_choice_letter():
    ref = Reference(kind="numeric_match", payload={"answer": "C"})
    assert grade(ref, "After consideration, my final answer is C.") is True
    assert grade(ref, "(A)") is False


def test_numeric_match_falls_back_to_substring_for_freeform_sql():
    ref = Reference(kind="numeric_match", payload={"answer": "SELECT id FROM users"})
    assert grade(ref, "```sql\nSELECT id FROM users\n```") is True
    assert grade(ref, "SELECT * FROM accounts") is False


def test_code_tests_requires_entry_point():
    ref = Reference(
        kind="code_tests",
        payload={"language": "python", "tests": "assert add(1,2) == 3", "entry_point": "add"},
    )
    assert grade(ref, "def add(a, b):\n    return a + b") is True
    assert grade(ref, "def subtract(a, b):\n    return a - b") is False


def test_code_tests_strips_markdown_fences():
    ref = Reference(kind="code_tests", payload={"language": "python", "tests": "", "entry_point": "f"})
    assert grade(ref, "```python\ndef f(): pass\n```") is True


def test_code_tests_passes_when_no_entry_point_specified():
    ref = Reference(kind="code_tests", payload={"language": "python", "tests": "..."})
    assert grade(ref, "def hello(): pass") is True


def test_code_tests_fails_on_empty_output():
    ref = Reference(kind="code_tests", payload={"language": "python", "tests": "", "entry_point": "f"})
    assert grade(ref, "") is False


def test_tool_call_match_structural_subset():
    ref = Reference(
        kind="tool_call_match",
        payload={"expected": {"name": "search", "arguments": {"query": "anything"}}},
    )
    matching = '{"name": "search", "arguments": {"query": "weather"}, "extra": 1}'
    assert grade(ref, "Tool call:\n" + matching) is True


def test_tool_call_match_fails_on_missing_key():
    ref = Reference(
        kind="tool_call_match",
        payload={"expected": {"name": "search", "arguments": {"query": "x"}}},
    )
    not_matching = '{"name": "search"}'
    assert grade(ref, not_matching) is False


def test_tool_call_match_returns_false_when_no_json():
    ref = Reference(kind="tool_call_match", payload={"expected": {"name": "x"}})
    assert grade(ref, "no json here") is False


def test_unknown_kind_raises():
    ref = Reference(kind="numeric_match", payload={"answer": "1"})
    # mutate after construction to bypass Literal validation; test the
    # runtime guard in grade()
    object.__setattr__(ref, "kind", "completely-bogus")
    with pytest.raises(ValueError):
        grade(ref, "1")
