"""Per-1k token pricing used for cost math across all evaluated providers.

INPUT costs here must match
`router/scripts/train_cluster_router.py`'s `DEFAULT_COST_PER_1K_INPUT`
when prices change — bake the same numbers into the eval at run time so
the per-router cost comparison aligns with what the cluster scorer was
trained against. OUTPUT costs are eval-only (the cluster scorer does
not factor output cost into its α-blend, per the AvengersPro paper).
"""

from __future__ import annotations

# USD per 1000 tokens. Update when a vendor ships a new price list and
# in lockstep with router/scripts/train_cluster_router.py for input
# costs.
# Sourced from each vendor's public pricing on 2026-04-30. Keep input
# numbers in lockstep with `router/scripts/train_cluster_router.py`'s
# DEFAULT_COST_PER_1K_INPUT — the cluster scorer's α-blend was trained
# with those numbers, so the eval cost comparison must use them too.
COST_PER_1K_INPUT: dict[str, float] = {
    # Anthropic
    "claude-opus-4-7": 0.015,
    "claude-sonnet-4-5": 0.003,
    "claude-haiku-4-5": 0.0008,

    # OpenAI: GPT-5.5 family (April 2026; 2x prior generation)
    "gpt-5.5": 0.005,
    "gpt-5.5-pro": 0.030,
    "gpt-5.5-mini": 0.0005,
    "gpt-5.5-nano": 0.00015,

    # OpenAI: GPT-5.4 family
    "gpt-5.4": 0.003,
    "gpt-5.4-pro": 0.020,
    "gpt-5.4-mini": 0.0004,
    "gpt-5.4-nano": 0.0001,

    # OpenAI: GPT-5 (legacy sticker)
    "gpt-5": 0.0025,
    "gpt-5-chat": 0.0025,
    "gpt-5-mini": 0.0005,
    "gpt-5-nano": 0.0001,

    # OpenAI: GPT-4.x (legacy)
    "gpt-4.1": 0.002,
    "gpt-4.1-mini": 0.0004,
    "gpt-4o": 0.0025,
    "gpt-4o-mini": 0.00015,

    # Google: Gemini 3.x (April 2026; -preview while in preview)
    "gemini-3-pro-preview": 0.002,
    "gemini-3.1-pro-preview": 0.002,
    "gemini-3-flash-preview": 0.0005,
    "gemini-3.1-flash-lite-preview": 0.0001,

    # Google: Gemini 2.x (legacy)
    "gemini-2.5-pro": 0.00125,
    "gemini-2.5-flash": 0.0003,
    "gemini-2.5-flash-lite": 0.0001,
    "gemini-2.0-flash": 0.0001,
    "gemini-2.0-flash-lite": 0.000075,
}

COST_PER_1K_OUTPUT: dict[str, float] = {
    # Anthropic
    "claude-opus-4-7": 0.075,
    "claude-sonnet-4-5": 0.015,
    "claude-haiku-4-5": 0.004,

    # OpenAI: GPT-5.5 family
    "gpt-5.5": 0.030,
    "gpt-5.5-pro": 0.180,
    "gpt-5.5-mini": 0.002,
    "gpt-5.5-nano": 0.0007,

    # OpenAI: GPT-5.4 family
    "gpt-5.4": 0.020,
    "gpt-5.4-pro": 0.120,
    "gpt-5.4-mini": 0.0016,
    "gpt-5.4-nano": 0.0004,

    # OpenAI: GPT-5 (legacy sticker)
    "gpt-5": 0.015,
    "gpt-5-chat": 0.010,
    "gpt-5-mini": 0.0015,
    "gpt-5-nano": 0.0004,

    # OpenAI: GPT-4.x (legacy)
    "gpt-4.1": 0.008,
    "gpt-4.1-mini": 0.0016,
    "gpt-4o": 0.010,
    "gpt-4o-mini": 0.0006,

    # Google: Gemini 3.x (preview)
    "gemini-3-pro-preview": 0.012,
    "gemini-3.1-pro-preview": 0.012,
    "gemini-3-flash-preview": 0.003,
    "gemini-3.1-flash-lite-preview": 0.0004,

    # Google: Gemini 2.x (legacy)
    "gemini-2.5-pro": 0.005,
    "gemini-2.5-flash": 0.0025,
    "gemini-2.5-flash-lite": 0.0004,
    "gemini-2.0-flash": 0.0004,
    "gemini-2.0-flash-lite": 0.0003,
}


def estimate_cost(model: str, input_tokens: int, output_tokens: int) -> float:
    """USD cost for a single (model, input_tokens, output_tokens) call.

    Unknown models cost 0; aggregation flags this so the operator
    knows to extend the table.
    """
    in_rate = COST_PER_1K_INPUT.get(model, 0.0)
    out_rate = COST_PER_1K_OUTPUT.get(model, 0.0)
    return (input_tokens / 1000.0) * in_rate + (output_tokens / 1000.0) * out_rate
