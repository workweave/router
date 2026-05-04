"""RouterArena's "Acc-Cost Arena" composite score.

Copied verbatim from RouterArena's
``router_evaluation/compute_scores.py`` so our number is exactly
what their pipeline would emit. The leaderboard reports this on a
0–100 scale, so we multiply by 100 at the end.

Single source of truth — ``routerarena.py``, ``regrade.py``, and
``grade_lcb.py`` all import from here.
"""

from __future__ import annotations

import math


def arena_score(
    accuracy: float,
    cost_per_1k: float,
    *,
    beta: float = 0.1,
    c_max: float = 200.0,
    c_min: float = 0.0044,
) -> float:
    if cost_per_1k <= 0 or accuracy <= 0:
        return 0.0
    c_clamped = min(max(cost_per_1k, c_min), c_max)
    C_i = (math.log2(c_max) - math.log2(c_clamped)) / (
        math.log2(c_max) - math.log2(c_min)
    )
    return ((1 + beta) * accuracy * C_i) / (beta * accuracy + C_i) * 100
