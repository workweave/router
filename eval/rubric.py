"""Five-dim rubric used by the LLM-judge ensemble.

Lives as a pure module so unit tests cover the parsing + aggregation
without standing up any judge. The judge prompt template is below;
each judge wraps it with provider-specific call shape.
"""

from __future__ import annotations

import json
import re
from typing import Any

from eval.types import RubricScores

# Five dimensions, each scored 1..5. Mean / 5 → [0, 1].
DIMENSIONS = (
    "correctness",
    "completeness",
    "code_quality",
    "style",
    "follows_instructions",
)

JUDGE_PROMPT_TEMPLATE = """\
You are an impartial judge comparing two assistant responses to the same user prompt.
Score each response on five dimensions, each on a 1-5 integer scale:

  1. correctness          — factually / logically right
  2. completeness         — covers what the prompt asks for
  3. code_quality         — clean / idiomatic if code; "5" for non-code prompts
  4. style                — tone, clarity, format
  5. follows_instructions — does what the prompt asked, no scope creep

Respond ONLY with a JSON object of the form:

{{
  "response_a": {{
    "correctness": <1-5>,
    "completeness": <1-5>,
    "code_quality": <1-5>,
    "style": <1-5>,
    "follows_instructions": <1-5>,
    "rationale": "<one sentence>"
  }},
  "response_b": {{
    "correctness": <1-5>,
    "completeness": <1-5>,
    "code_quality": <1-5>,
    "style": <1-5>,
    "follows_instructions": <1-5>,
    "rationale": "<one sentence>"
  }}
}}

Do NOT include any text outside the JSON object. Do NOT use markdown
fences. Identity of the two responses has been stripped — judge on
content only.

----- USER PROMPT -----
{prompt}

----- RESPONSE A -----
{response_a}

----- RESPONSE B -----
{response_b}
"""


def aggregate(scores: RubricScores) -> float:
    """Mean of the five dims, normalized to [0, 1].

    Each rubric dimension is on a 1-5 integer scale, so the lowest
    achievable mean is 1.0 (all ones) and the highest is 5.0 (all
    fives). Subtracting the lower bound and dividing by the range
    maps the mean linearly onto [0, 1] — feeding directly into
    ``DISAGREEMENT_THRESHOLD`` and into ``spot_check.py``'s quartile
    bucketing so neither truncates against an asymmetric range.
    """
    values = [getattr(scores, d) for d in DIMENSIONS]
    mean = sum(values) / len(values)
    return (mean - 1.0) / 4.0


# ----- parsing -----


def parse_judge_response(raw: str) -> tuple[RubricScores, RubricScores, str]:
    """Returns (scores_for_a, scores_for_b, rationale).

    `raw` is the judge's full text output. We extract the JSON object
    (tolerating a leading code fence or stray prose) and validate.
    Raises ValueError on malformed output so the ensemble can flag it.
    """
    obj = _extract_json(raw)
    if obj is None:
        raise ValueError(f"judge response did not contain a JSON object; got: {raw[:200]}...")
    a = _scores_from(obj.get("response_a", {}))
    b = _scores_from(obj.get("response_b", {}))
    rationale_a = str(obj.get("response_a", {}).get("rationale", ""))
    rationale_b = str(obj.get("response_b", {}).get("rationale", ""))
    rationale = f"A: {rationale_a} | B: {rationale_b}"
    return a, b, rationale


def _scores_from(obj: dict[str, Any]) -> RubricScores:
    fields = {}
    for dim in DIMENSIONS:
        v = obj.get(dim)
        if v is None:
            raise ValueError(f"judge response missing dimension {dim!r}")
        try:
            f = float(v)
        except (TypeError, ValueError) as e:
            raise ValueError(f"non-numeric value for {dim!r}: {v!r}") from e
        if not (1.0 <= f <= 5.0):
            raise ValueError(f"out-of-range value for {dim!r}: {f}")
        fields[dim] = f
    return RubricScores(**fields)


_JSON_DECODER = json.JSONDecoder()


def _extract_json(raw: str) -> dict[str, Any] | None:
    """Pull the first decodable JSON object out of `raw`.

    Uses ``raw_decode`` rather than a greedy ``\\{.*\\}`` regex so we can
    recover when the judge appends trailing prose (or a closing fence)
    after the object — the regex would swallow the trailing text and
    fail to parse.
    """
    # Strip fences if the judge ignored instructions and added them.
    fence_stripped = re.sub(r"```(?:json)?\s*", "", raw, flags=re.IGNORECASE)
    fence_stripped = fence_stripped.replace("```", "")
    start = fence_stripped.find("{")
    while start != -1:
        try:
            loaded, _ = _JSON_DECODER.raw_decode(fence_stripped[start:])
        except json.JSONDecodeError:
            start = fence_stripped.find("{", start + 1)
            continue
        return loaded if isinstance(loaded, dict) else None
    return None
