from __future__ import annotations

import numpy as np

from hmm_sidecar.features import (
    classifier_features,
    conversation_sequence,
    tool_context_features,
)


def test_conversation_sequence_pairs_only_completed_turns() -> None:
    turns = conversation_sequence(
        {
            "prompt_text": "fallback",
            "conversation_messages": [
                {"role": "system", "text": "Keep the answer short."},
                {"role": "user", "text": "Inspect the repository."},
                {
                    "role": "assistant",
                    "text": "I inspected it.",
                    "tool_calls": [{"name": "Read", "input_keys": ["path"]}],
                },
                {"role": "user", "text": "Now fix the bug."},
            ],
        }
    )

    assert [turn.kind for turn in turns] == ["user", "agent", "user"]
    assert "system context: Keep the answer short." in turns[0].text
    assert "[Read] input keys: path" in turns[1].text
    assert "Observed outcome:\nI inspected it." in turns[1].text
    assert "Request:\nNow fix the bug." in turns[2].text


def test_final_tool_result_closes_the_pending_agent_turn() -> None:
    turns = conversation_sequence(
        {
            "conversation_messages": [
                {"role": "user", "text": "Inspect the repository."},
                {
                    "role": "assistant",
                    "tool_calls": [{"name": "Read", "input_keys": ["path"]}],
                },
                {
                    "role": "user",
                    "tool_results": [{"name": "Read", "is_error": False}],
                },
            ]
        }
    )

    assert [turn.kind for turn in turns] == ["user", "agent"]
    assert "[Read] input keys: path" in turns[1].text
    assert "Observed outcome:\nTool result returned." in turns[1].text


def test_tool_intent_requires_tools_and_is_inferred_from_latest_user_text() -> None:
    without_tools = tool_context_features(
        {"has_tools": False, "latest_user_text": "Please inspect the repository"}
    )
    with_tools = tool_context_features(
        {"has_tools": True, "latest_user_text": "Please inspect the repository"}
    )

    assert without_tools[1] == 0.0
    assert with_tools[1] == 1.0


def test_classifier_row_does_not_claim_the_live_turn_is_conversation_final() -> None:
    row = classifier_features(
        embedding=np.zeros(2),
        gamma=np.asarray([0.7, 0.3]),
        state=0,
        previous_state=1,
        position=2,
        prefix_length=3,
        tool_context=np.zeros(23),
    )

    scalar_offset = 2 + 2 + 2 + 2
    assert row[scalar_offset + 5] == 0.0
