"""Reference-based grading (binary pass/fail) for code, math, tool calls.

This complements the LLM-judge rubric. For prompts with a Reference,
both scores are recorded; the LLM-judge mean is the headline for the
gate decision and reference pass-rate is reported as a sanity sidecar
column in EVAL_RESULTS.md.

Reference grading runs in-process — no sandboxes, no external
processes. The code grader does *not* execute model code, it does
substring / structural checks on the generated source. Real test
execution is a Phase 1b upgrade; for the gate decision the LLM judge
is the authority for code correctness and the reference column tracks
the simpler "does it look right" signal.
"""

from __future__ import annotations

import json
import re
from typing import Any

from eval.types import Reference


def grade(reference: Reference | None, output: str) -> bool | None:
    """Returns None if no reference applies; True/False otherwise."""
    if reference is None or reference.kind == "none":
        return None
    if reference.kind == "numeric_match":
        return _grade_numeric(reference.payload, output)
    if reference.kind == "code_tests":
        return _grade_code(reference.payload, output)
    if reference.kind == "tool_call_match":
        return _grade_tool_call(reference.payload, output)
    raise ValueError(f"unknown reference kind: {reference.kind!r}")


def _grade_numeric(payload: dict[str, Any], output: str) -> bool:
    """Match the expected answer to the model output.

    Tolerates surrounding text (e.g. CoT) by extracting the last
    number / letter / SQL statement found in the output. For
    multiple-choice (single-letter answers) the comparison is
    case-insensitive.
    """
    expected = str(payload.get("answer", "")).strip()
    if not expected:
        return False

    # Single-letter answer (multiple-choice).
    if len(expected) == 1 and expected.upper() in "ABCDE":
        m = re.findall(r"\b([A-E])\b", output.upper())
        return bool(m) and m[-1] == expected.upper()

    # Numeric answer — extract the last number-shaped token.
    if _looks_numeric(expected):
        nums = re.findall(r"-?\d+(?:\.\d+)?", output.replace(",", ""))
        if not nums:
            return False
        try:
            return abs(float(nums[-1]) - float(expected.replace(",", ""))) < 1e-6
        except ValueError:
            return False

    # Free-form (e.g. SQL). Do a normalized substring check.
    return _normalize(expected) in _normalize(output)


def _grade_code(payload: dict[str, Any], output: str) -> bool:
    """Heuristic correctness check for code outputs.

    For PR 2 we do NOT execute model code in-process. We check that
    the output (a) parses as a code block in the expected language and
    (b) contains the entry-point identifier when one is given. This
    is intentionally cheap; the LLM judge evaluates correctness in
    depth.
    """
    entry = payload.get("entry_point", "")
    code = _strip_fences(output)
    if entry and entry not in code:
        return False
    # A minimum-viable presence check: the response must contain
    # *some* code-shaped content. Defining-something heuristics over
    # the language matter less than entry-point presence.
    return bool(code.strip())


def _grade_tool_call(payload: dict[str, Any], output: str) -> bool:
    """Compare emitted tool call (JSON anywhere in `output`) to expected.

    BFCL v4 / tau-bench expectations are dict-shaped tool invocations.
    We extract the first JSON object from the model output and compare
    keys + nested keys (not values) to the expected shape; arg-value
    matching is judge-side.
    """
    expected = payload.get("expected") or {}
    if not expected:
        return False
    obj = _first_json_object(output)
    if obj is None:
        return False
    return _structural_subset(expected, obj)


# ----- helpers -----


def _looks_numeric(s: str) -> bool:
    try:
        float(s.replace(",", ""))
        return True
    except ValueError:
        return False


_FENCE_RE = re.compile(r"```(?:[a-zA-Z0-9_+-]*)\n(.*?)```", re.DOTALL)


def _strip_fences(text: str) -> str:
    matches = _FENCE_RE.findall(text)
    if matches:
        return "\n".join(matches)
    return text


def _normalize(s: str) -> str:
    return re.sub(r"\s+", " ", s.strip().lower())


_JSON_DECODER = json.JSONDecoder()


def _first_json_object(text: str) -> dict[str, Any] | None:
    """Find the first decodable top-level JSON object in `text`.

    Uses ``JSONDecoder.raw_decode`` so braces inside quoted strings are
    handled correctly — naive depth-counting falsely closes the object
    when the model emits `{"comment": "}"}`-style payloads.
    """
    start = text.find("{")
    while start != -1:
        try:
            loaded, _ = _JSON_DECODER.raw_decode(text[start:])
        except json.JSONDecodeError:
            start = text.find("{", start + 1)
            continue
        if isinstance(loaded, dict):
            return loaded
        start = text.find("{", start + 1)
    return None


def _structural_subset(expected: dict[str, Any], actual: dict[str, Any]) -> bool:
    """True iff every key-path in `expected` exists in `actual` (values
    are not compared — they're judge-side).
    """
    for k, v in expected.items():
        if k not in actual:
            return False
        if isinstance(v, dict):
            if not isinstance(actual[k], dict):
                return False
            if not _structural_subset(v, actual[k]):
                return False
    return True
