from __future__ import annotations

import re
from dataclasses import dataclass
from typing import Any

import numpy as np

CATEGORICAL_DIM = 13
MAX_SEQUENCE_ELEMENTS = 48
NOT_VISIBLE = "None visible."
NO_TOOL_CALLS = "No tool calls recorded."

TOOL_FAMILIES = ("bash", "read", "search", "edit", "task")
SEARCH_RE = re.compile(
    r"\b(search|grep|rg|ripgrep|find|read|open|cat|ls|list|inspect|scan|look through|look at)\b",
    re.IGNORECASE,
)
EXPLORE_RE = re.compile(
    r"\b(explore|scour|inventory|survey|map out|read through|background agent|subagent)\b",
    re.IGNORECASE,
)
EDIT_RE = re.compile(
    r"\b(edit|write|create|implement|fix|patch|change|refactor|update)\b",
    re.IGNORECASE,
)
TEST_RE = re.compile(
    r"\b(run|execute|test|pytest|npm test|smoke test|validate|verify|check)\b",
    re.IGNORECASE,
)
TOOL_RE = re.compile(r"(^\s*/|\b(tool|tool call|bash|shell|command|script)\b)", re.I)


@dataclass(frozen=True)
class SequenceTurn:
    text: str
    kind: str


def _section(title: str, values: list[str]) -> str:
    clean = [value.strip() for value in values if value.strip()]
    if not clean:
        return f"{title}:\n- {NOT_VISIBLE}"
    return f"{title}:\n" + "\n".join(f"- {value}" for value in clean)


def user_document(index: int, text: str, context: list[str] | None = None) -> str:
    clean = text.strip() or "[missing prompt text]"
    return "\n\n".join(
        (
            f"User message (turn {index}):",
            f"Request:\n{clean}",
            f"Intent:\n{clean}",
            _section("Constraints and guidelines", []),
            _section("Context supplied", context or []),
            _section("Clarifications and corrections", []),
        )
    )


def _tool_groups(message: dict[str, Any]) -> list[str]:
    groups = []
    for call in message.get("tool_calls") or []:
        if not isinstance(call, dict):
            continue
        name = str(call.get("name") or "tool").strip() or "tool"
        keys = sorted(
            str(key).strip() for key in call.get("input_keys") or [] if str(key).strip()
        )
        operation = "tool call" if not keys else "input keys: " + ", ".join(keys)
        groups.append(f"[{name}] {operation}")
    return groups


def _tool_result_summary(message: dict[str, Any]) -> str:
    results = [
        result
        for result in message.get("tool_results") or []
        if isinstance(result, dict)
    ]
    if not results:
        return ""
    errors = sum(1 for result in results if bool(result.get("is_error")))
    if len(results) == 1:
        return "Tool result returned with error." if errors else "Tool result returned."
    if errors:
        return f"{len(results)} tool results returned, {errors} with errors."
    return f"{len(results)} tool results returned."


def agent_document(index: int, message: dict[str, Any]) -> str:
    groups = _tool_groups(message)
    sections = [f"Agent reply (turn {index}):"]
    sections.append(
        _section("Actions", groups) if groups else f"Actions:\n- {NO_TOOL_CALLS}"
    )
    text = str(message.get("text") or "").strip()
    if text:
        sections.append(f"Observed outcome:\n{text}")
    return "\n\n".join(sections)


def _completed_agent_document(
    index: int,
    *,
    goal: str,
    outcome: str,
    groups: list[str],
) -> str:
    sections = [f"Agent reply (turn {index}):"]
    if groups:
        if goal:
            sections.append(f"Goal:\n{goal}")
        sections.append(_section("Actions", groups))
    else:
        sections.append(f"Actions:\n- {NO_TOOL_CALLS}")
    sections.append(f"Observed outcome:\n{outcome or 'Conversation continued.'}")
    sections.append(
        "Open state after turn:\nLater turns may refine or supersede this request."
    )
    return "\n\n".join(sections)


def conversation_sequence(payload: dict[str, Any]) -> list[SequenceTurn]:
    messages = payload.get("conversation_messages")
    if not isinstance(messages, list):
        return [
            SequenceTurn(
                user_document(0, str(payload.get("prompt_text") or "")), "user"
            )
        ]
    turns: list[SequenceTurn] = []
    context_lines: list[str] = []
    user_index = -1
    pending_user_text = ""
    pending_user_context: list[str] = []
    agent_goal = ""
    agent_outcome = ""
    agent_groups: list[str] = []
    pending_tool_result = False

    def close_pending() -> None:
        nonlocal pending_user_text, pending_user_context
        nonlocal agent_goal, agent_outcome, agent_groups, pending_tool_result
        if not pending_user_text:
            agent_goal = ""
            agent_outcome = ""
            agent_groups = []
            return
        turns.append(
            SequenceTurn(
                user_document(user_index, pending_user_text, pending_user_context),
                "user",
            )
        )
        turns.append(
            SequenceTurn(
                _completed_agent_document(
                    user_index,
                    goal=agent_goal,
                    outcome=agent_outcome,
                    groups=agent_groups,
                ),
                "agent",
            )
        )
        pending_user_text = ""
        pending_user_context = []
        agent_goal = ""
        agent_outcome = ""
        agent_groups = []
        pending_tool_result = False

    for message in messages:
        if not isinstance(message, dict):
            continue
        role = str(message.get("role") or "").strip().lower()
        if role == "model":
            role = "assistant"
        text = str(message.get("text") or "").strip()
        tool_result_summary = _tool_result_summary(message)
        if role == "user" and text:
            close_pending()
            user_index += 1
            pending_user_text = text
            pending_user_context = context_lines
            context_lines = []
        elif role == "user" and tool_result_summary:
            if pending_user_text:
                agent_outcome = tool_result_summary
                pending_tool_result = True
            else:
                context_lines.append(f"tool context: {tool_result_summary}")
        elif role == "assistant":
            groups = _tool_groups(message)
            agent_groups.extend(groups)
            if text:
                if not agent_goal:
                    agent_goal = text
                agent_outcome = text
        elif role in {"system", "developer"} and text:
            context_lines.append(f"{role} context: {text}")
    if pending_user_text:
        if pending_tool_result:
            close_pending()
        else:
            turns.append(
                SequenceTurn(
                    user_document(user_index, pending_user_text, pending_user_context),
                    "user",
                )
            )
    if not turns:
        turns.append(
            SequenceTurn(
                user_document(0, str(payload.get("prompt_text") or "")), "user"
            )
        )
    if len(turns) <= MAX_SEQUENCE_ELEMENTS:
        return turns
    window = turns[-MAX_SEQUENCE_ELEMENTS:]
    for index, turn in enumerate(window):
        if turn.kind == "user":
            return window[index:]
    for index in range(len(turns) - MAX_SEQUENCE_ELEMENTS - 1, -1, -1):
        if turns[index].kind == "user":
            return turns[index:]
    return []


def raw_hmm_features(
    embeddings: list[np.ndarray], turns: list[SequenceTurn]
) -> np.ndarray:
    denominator = max(len(embeddings) - 1, 1)
    rows = []
    for index, (embedding, turn) in enumerate(zip(embeddings, turns, strict=True)):
        categorical = [0.0] * CATEGORICAL_DIM
        categorical[0 if turn.kind == "user" else 1] = 1.0
        rows.append(
            np.concatenate(
                (
                    np.asarray(embedding, dtype=np.float64),
                    np.asarray(categorical, dtype=np.float64),
                    np.asarray([float(index) / float(denominator)]),
                )
            )
        )
    return np.asarray(rows, dtype=np.float64)


def _tool_family_flags(names: list[str]) -> list[float]:
    normalized = " ".join(names).lower()
    tokens = {
        "bash": ("bash", "shell", "exec", "terminal"),
        "read": ("read", "open", "cat", "view"),
        "search": ("grep", "glob", "search", "find", "rg"),
        "edit": ("edit", "write", "patch", "apply"),
        "task": ("task", "agent", "explore", "spawn"),
    }
    return [
        1.0 if any(token in normalized for token in tokens[family]) else 0.0
        for family in TOOL_FAMILIES
    ]


def _intent_flags(text: str) -> list[float]:
    flags = [
        1.0 if TOOL_RE.search(text) else 0.0,
        1.0 if SEARCH_RE.search(text) else 0.0,
        1.0 if EXPLORE_RE.search(text) else 0.0,
        1.0 if EDIT_RE.search(text) else 0.0,
        1.0 if TEST_RE.search(text) else 0.0,
    ]
    return [1.0 if not any(flags) else 0.0, *flags]


def current_text_has_tool_intent(text: str) -> bool:
    return _intent_flags(text)[0] == 0.0


def tool_context_features(payload: dict[str, Any]) -> np.ndarray:
    available = [
        str(value).strip()
        for value in payload.get("available_tools") or []
        if str(value).strip()
    ]
    messages = payload.get("conversation_messages") or []
    prior_names: list[str] = []
    calls = turns = results = errors = 0
    if isinstance(messages, list):
        for message in messages:
            if not isinstance(message, dict):
                continue
            message_calls = 0
            for call in message.get("tool_calls") or []:
                if isinstance(call, dict):
                    name = str(call.get("name") or "").strip()
                    if name:
                        prior_names.append(name)
                    calls += 1
                    message_calls += 1
            turns += 1 if message_calls else 0
            for result in message.get("tool_results") or []:
                if isinstance(result, dict):
                    results += 1
                    errors += 1 if bool(result.get("is_error")) else 0
    available = list(dict.fromkeys([*available, *prior_names]))
    latest = ""
    if isinstance(messages, list):
        for message in reversed(messages):
            if not isinstance(message, dict):
                continue
            role = str(message.get("role") or "").strip().lower()
            if role != "user":
                continue
            latest = str(message.get("text") or "").strip()
            if latest:
                break
    latest = latest or str(
        payload.get("latest_user_text") or payload.get("prompt_text") or ""
    )
    has_tools = bool(payload.get("has_tools"))
    return np.asarray(
        [
            1.0 if has_tools else 0.0,
            1.0 if has_tools and current_text_has_tool_intent(latest) else 0.0,
            float(np.log1p(len(available))),
            *_tool_family_flags(available),
            *_intent_flags(latest),
            float(np.log1p(calls)),
            float(np.log1p(turns)),
            float(np.log1p(results)),
            float(np.log1p(errors)),
            *_tool_family_flags(prior_names),
        ],
        dtype=np.float64,
    )


def classifier_features(
    *,
    embedding: np.ndarray,
    gamma: np.ndarray,
    state: int,
    previous_state: int,
    position: int,
    prefix_length: int,
    tool_context: np.ndarray,
) -> np.ndarray:
    states = gamma.shape[0]
    eye = np.eye(states, dtype=np.float64)
    order = np.argsort(gamma)[::-1]
    top = float(gamma[order[0]])
    margin = float(gamma[order[0]] - gamma[order[1]]) if states > 1 else top
    denominator = max(prefix_length - 1, 1)
    scalars = np.asarray(
        [
            float(position) / float(denominator),
            float(np.log1p(prefix_length)),
            top,
            margin,
            1.0 if position == 0 else 0.0,
            0.0,
        ],
        dtype=np.float64,
    )
    return np.concatenate(
        (embedding, gamma, eye[state], eye[previous_state], scalars, tool_context)
    )
