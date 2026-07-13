from __future__ import annotations

import pytest

from hmm_sidecar.policy import select_roster_arm, selected_margin


def test_roster_fallback_reports_the_selected_classifier_group() -> None:
    probabilities = {"fast": 0.2, "maximum": 0.8}

    label, roster_id = select_roster_arm(
        probabilities=probabilities,
        classes=("fast", "maximum"),
        clusters={
            "fast": {"arms": ["provider/fast"]},
            "maximum": {"arms": ["provider/maximum"]},
        },
        available_roster_ids={"provider/fast"},
    )

    assert label == "fast"
    assert roster_id == "provider/fast"
    assert selected_margin(probabilities, label) == pytest.approx(-0.6)
