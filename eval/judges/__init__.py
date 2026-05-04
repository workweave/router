"""Judge ABC + ensemble entry point.

Each Judge is a thin async wrapper around a frontier LLM (GPT-5,
Gemini 2.5 Pro). All judges return a per-candidate Judgment (rubric
scores + rationale + raw response). The ensemble.judge_pair() helper
fans both judges in parallel and emits a JudgmentRow per judge plus a
disagreement flag.
"""

from __future__ import annotations

from abc import ABC, abstractmethod
from typing import ClassVar

from eval.types import RubricScores


class Judge(ABC):
    """Async judge protocol.

    Implementations: judges/gpt5.py, judges/gemini.py.
    """

    name: ClassVar[str]

    @abstractmethod
    async def judge_pair(
        self, *, prompt: str, response_a: str, response_b: str
    ) -> tuple[RubricScores, RubricScores, str, str]:
        """Returns (scores_a, scores_b, rationale, raw_response).

        Order is randomized by the caller (ensemble.py); judges
        themselves don't shuffle.
        """
