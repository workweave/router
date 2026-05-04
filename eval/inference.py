"""Direct provider inference for the always-X routers.

Anthropic, OpenAI, and Google (Gemini via the OpenAI-compat endpoint)
each have their own helper below; routes that go through the staging
deployment live in routing.py and don't use this module.
"""

from __future__ import annotations

import asyncio
import os
import time
from dataclasses import dataclass

from tenacity import (
    AsyncRetrying,
    retry_if_exception_type,
    stop_after_attempt,
    wait_exponential,
)


@dataclass
class InferenceResult:
    output_text: str
    input_tokens: int
    output_tokens: int
    latency_ms: int
    error: str | None = None


# Model IDs — keep in sync with manifest.py and pricing.py.
ANTHROPIC_MODELS = {
    "claude-opus-4-7",
    "claude-sonnet-4-5",
    "claude-haiku-4-5",
}

# OpenAI model IDs reachable via api.openai.com.
OPENAI_MODELS = {
    # GPT-5.5 family (April 2026)
    "gpt-5.5",
    "gpt-5.5-pro",
    "gpt-5.5-mini",
    "gpt-5.5-nano",
    # GPT-5.4 family
    "gpt-5.4",
    "gpt-5.4-pro",
    "gpt-5.4-mini",
    "gpt-5.4-nano",
    # GPT-5 line (legacy sticker)
    "gpt-5",
    "gpt-5-chat",
    "gpt-5-mini",
    "gpt-5-nano",
    # GPT-4.x (legacy)
    "gpt-4.1",
    "gpt-4.1-mini",
    "gpt-4o",
    "gpt-4o-mini",
}

# Google Gemini model IDs reachable via the OpenAI-compatible endpoint
# at generativelanguage.googleapis.com/v1beta/openai. Same wire format
# as OpenAI; only the base URL and auth key differ.
#
# Frontier IDs (April 2026) all carry the `-preview` suffix Google
# requires while the family is in preview. Drop it once the models
# go GA so the eval reflects production routing.
GOOGLE_MODELS = {
    # Gemini 3.x (April 2026 frontier; preview)
    "gemini-3-pro-preview",
    "gemini-3.1-pro-preview",
    "gemini-3-flash-preview",
    "gemini-3.1-flash-lite-preview",
    # Gemini 2.x (legacy stable)
    "gemini-2.5-pro",
    "gemini-2.5-flash",
    "gemini-2.5-flash-lite",
    "gemini-2.0-flash",
    "gemini-2.0-flash-lite",
}

GOOGLE_OPENAI_BASE_URL = "https://generativelanguage.googleapis.com/v1beta/openai"

# Backward-compat alias used by callers that pre-date the multi-provider split.
MODELS = ANTHROPIC_MODELS

DEFAULT_MAX_OUTPUT_TOKENS = 2048
DEFAULT_TIMEOUT_S = 120.0


async def call_anthropic(
    *,
    model: str,
    prompt: str,
    max_output_tokens: int = DEFAULT_MAX_OUTPUT_TOKENS,
    timeout_s: float = DEFAULT_TIMEOUT_S,
) -> InferenceResult:
    """One-shot call to the Anthropic Messages API. Used for the
    always-X routers; staging-routed calls go through routing.py."""
    if model not in ANTHROPIC_MODELS:
        raise ValueError(f"unknown anthropic model {model!r}; expected one of {ANTHROPIC_MODELS}")
    from anthropic import (
        APIConnectionError,
        APITimeoutError,
        AsyncAnthropic,
        InternalServerError,
        RateLimitError,
    )

    # Only retry classes of failure that have a real chance of succeeding
    # on a second attempt: transport-level (connection / timeout), 429s,
    # and 5xx. Permanent 4xx (auth, bad request, not-found) should
    # surface immediately so the eval doesn't waste minutes hammering a
    # broken request.
    retryable = (
        APIConnectionError,
        APITimeoutError,
        InternalServerError,
        RateLimitError,
        asyncio.TimeoutError,
    )

    client = AsyncAnthropic(api_key=os.environ["ANTHROPIC_API_KEY"])
    started = time.monotonic()
    try:
        async for attempt in AsyncRetrying(
            wait=wait_exponential(multiplier=1, min=1, max=30),
            stop=stop_after_attempt(4),
            retry=retry_if_exception_type(retryable),
            reraise=True,
        ):
            with attempt:
                msg = await asyncio.wait_for(
                    client.messages.create(
                        model=model,
                        max_tokens=max_output_tokens,
                        messages=[{"role": "user", "content": prompt}],
                    ),
                    timeout=timeout_s,
                )
    except Exception as e:
        latency_ms = int((time.monotonic() - started) * 1000)
        return InferenceResult(output_text="", input_tokens=0, output_tokens=0, latency_ms=latency_ms, error=str(e))

    latency_ms = int((time.monotonic() - started) * 1000)
    parts: list[str] = []
    for block in msg.content:
        text = getattr(block, "text", None)
        if text:
            parts.append(text)
    return InferenceResult(
        output_text="".join(parts),
        input_tokens=getattr(msg.usage, "input_tokens", 0) or 0,
        output_tokens=getattr(msg.usage, "output_tokens", 0) or 0,
        latency_ms=latency_ms,
    )


async def call_openai_chat(
    *,
    model: str,
    prompt: str,
    max_output_tokens: int = DEFAULT_MAX_OUTPUT_TOKENS,
    timeout_s: float = DEFAULT_TIMEOUT_S,
) -> InferenceResult:
    """One-shot call to a Chat-Completions-shaped surface. Routes to
    api.openai.com when `model` is in OPENAI_MODELS; routes to Google's
    OpenAI-compat endpoint when `model` is in GOOGLE_MODELS. Same SDK
    (openai.AsyncOpenAI) for both — only base_url and the API key
    differ.
    """
    if model in OPENAI_MODELS:
        api_key = os.environ["OPENAI_API_KEY"]
        base_url: str | None = None
    elif model in GOOGLE_MODELS:
        api_key = os.environ["GOOGLE_API_KEY"]
        base_url = GOOGLE_OPENAI_BASE_URL
    else:
        raise ValueError(
            f"unknown chat model {model!r}; expected one of "
            f"{sorted(OPENAI_MODELS | GOOGLE_MODELS)}"
        )

    from openai import (
        APIConnectionError,
        APITimeoutError,
        AsyncOpenAI,
        InternalServerError,
        RateLimitError,
    )

    retryable = (
        APIConnectionError,
        APITimeoutError,
        InternalServerError,
        RateLimitError,
        asyncio.TimeoutError,
    )

    client = AsyncOpenAI(api_key=api_key, base_url=base_url)

    # GPT-5.x and later (and the o-series) reject `max_tokens` and
    # require `max_completion_tokens` — OpenAI renamed it on the
    # reasoning-capable line. Older 4.x models still take `max_tokens`.
    # Detect by model-name prefix; cheaper than introspecting the
    # error after a 400.
    use_max_completion = (
        model.startswith("gpt-5")
        or model.startswith("o1")
        or model.startswith("o3")
        or model.startswith("o4")
    )
    create_kwargs: dict = {
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
    }
    if use_max_completion:
        create_kwargs["max_completion_tokens"] = max_output_tokens
    else:
        create_kwargs["max_tokens"] = max_output_tokens

    started = time.monotonic()
    try:
        async for attempt in AsyncRetrying(
            wait=wait_exponential(multiplier=1, min=1, max=30),
            stop=stop_after_attempt(4),
            retry=retry_if_exception_type(retryable),
            reraise=True,
        ):
            with attempt:
                resp = await asyncio.wait_for(
                    client.chat.completions.create(**create_kwargs),
                    timeout=timeout_s,
                )
    except Exception as e:
        latency_ms = int((time.monotonic() - started) * 1000)
        return InferenceResult(output_text="", input_tokens=0, output_tokens=0, latency_ms=latency_ms, error=str(e))

    latency_ms = int((time.monotonic() - started) * 1000)
    text = ""
    if resp.choices:
        first = resp.choices[0]
        if getattr(first, "message", None) is not None and first.message.content:
            text = first.message.content
    usage = getattr(resp, "usage", None)
    return InferenceResult(
        output_text=text,
        input_tokens=getattr(usage, "prompt_tokens", 0) or 0,
        output_tokens=getattr(usage, "completion_tokens", 0) or 0,
        latency_ms=latency_ms,
    )
