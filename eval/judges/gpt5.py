"""GPT-5 judge (OpenAI Chat Completions)."""

from __future__ import annotations

import asyncio
import os
from typing import ClassVar

from eval.judges import Judge
from eval.rubric import JUDGE_PROMPT_TEMPLATE, parse_judge_response
from eval.types import RubricScores

# The eval harness pins to a fixed model id at the top of each run via
# manifest.json. Override per-run with EVAL_GPT5_MODEL if Anthropic ID
# changes ship before we re-run.
DEFAULT_MODEL = "gpt-5"


class GPT5Judge(Judge):
    name: ClassVar[str] = "gpt5"

    def __init__(self, model: str | None = None, timeout_s: float = 120.0):
        # Lazy import so test environments without the OpenAI SDK
        # installed can still import the module for registry checks.
        from openai import AsyncOpenAI

        self._client = AsyncOpenAI(api_key=os.environ["OPENAI_API_KEY"])
        self._model = model or os.environ.get("EVAL_GPT5_MODEL") or DEFAULT_MODEL
        self._timeout_s = timeout_s

    async def judge_pair(
        self, *, prompt: str, response_a: str, response_b: str
    ) -> tuple[RubricScores, RubricScores, str, str]:
        body = JUDGE_PROMPT_TEMPLATE.format(
            prompt=prompt, response_a=response_a, response_b=response_b
        )
        resp = await asyncio.wait_for(
            self._client.chat.completions.create(
                model=self._model,
                messages=[{"role": "user", "content": body}],
                # GPT-5 only accepts the default temperature (1); the API
                # rejects any other value with 400 unsupported_value.
                response_format={"type": "json_object"},
            ),
            timeout=self._timeout_s,
        )
        raw = resp.choices[0].message.content or ""
        a, b, rationale = parse_judge_response(raw)
        return a, b, rationale, raw
