"""Gemini 2.5 Pro judge (google-genai SDK)."""

from __future__ import annotations

import asyncio
import os
from typing import ClassVar

from eval.judges import Judge
from eval.rubric import JUDGE_PROMPT_TEMPLATE, parse_judge_response
from eval.types import RubricScores

DEFAULT_MODEL = "gemini-2.5-pro"


class GeminiJudge(Judge):
    name: ClassVar[str] = "gemini"

    def __init__(self, model: str | None = None, timeout_s: float = 120.0):
        from google import genai

        # Toggle Vertex AI vs the AI Studio direct API. Vertex bills
        # against the GCP project (`workweave-prod-01`) and reuses the
        # GCP credentials Modal already mounts via `gcp-credentials`,
        # so the standalone `google-api-key` secret is not needed when
        # this is on. Set EVAL_GEMINI_VERTEX=true and supply
        # GOOGLE_CLOUD_PROJECT (+ optional GOOGLE_CLOUD_LOCATION,
        # default us-central1) to switch.
        if os.environ.get("EVAL_GEMINI_VERTEX", "").lower() == "true":
            self._client = genai.Client(
                vertexai=True,
                project=os.environ["GOOGLE_CLOUD_PROJECT"],
                location=os.environ.get("GOOGLE_CLOUD_LOCATION", "us-central1"),
            )
        else:
            self._client = genai.Client(api_key=os.environ["GOOGLE_API_KEY"])
        self._model = model or os.environ.get("EVAL_GEMINI_MODEL") or DEFAULT_MODEL
        self._timeout_s = timeout_s

    async def judge_pair(
        self, *, prompt: str, response_a: str, response_b: str
    ) -> tuple[RubricScores, RubricScores, str, str]:
        body = JUDGE_PROMPT_TEMPLATE.format(
            prompt=prompt, response_a=response_a, response_b=response_b
        )
        # google-genai's async surface returns a coroutine on
        # generate_content; wrap with asyncio.wait_for so a stuck
        # call doesn't block the whole batch.
        resp = await asyncio.wait_for(
            self._client.aio.models.generate_content(
                model=self._model,
                contents=body,
                config={"temperature": 0, "response_mime_type": "application/json"},
            ),
            timeout=self._timeout_s,
        )
        raw = resp.text or ""
        a, b, rationale = parse_judge_response(raw)
        return a, b, rationale, raw
