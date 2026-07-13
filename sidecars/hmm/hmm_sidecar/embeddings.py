from __future__ import annotations

import asyncio
import math
import os
from collections import OrderedDict
from typing import Protocol

import httpx
import numpy as np

from .schemas import EmbeddingContract

MAX_CACHE_ITEMS = 8192


class EmbeddingError(RuntimeError):
    pass


class Embedder(Protocol):
    async def embed(self, texts: list[str]) -> list[list[float]]: ...


def _google_model_name(model: str) -> str:
    configured = os.environ.get("HMM_EMBEDDING_MODEL", model)
    return configured.removeprefix("google/").removeprefix("models/")


class GoogleEmbedder:
    def __init__(self, contract: EmbeddingContract) -> None:
        self.contract = contract
        self.api_key = (
            os.environ.get("GOOGLE_API_KEY", "").strip()
            or os.environ.get("GEMINI_API_KEY", "").strip()
        )
        if not self.api_key:
            raise EmbeddingError("GOOGLE_API_KEY or GEMINI_API_KEY is required")
        self.base_url = os.environ.get(
            "HMM_GOOGLE_EMBEDDING_BASE_URL",
            "https://generativelanguage.googleapis.com/v1beta",
        ).rstrip("/")

    async def embed(self, texts: list[str]) -> list[list[float]]:
        model = _google_model_name(self.contract.model)
        requests = []
        for text in texts:
            request: dict[str, object] = {
                "model": f"models/{model}",
                "content": {"parts": [{"text": text}]},
                "outputDimensionality": self.contract.dimensions,
            }
            if self.contract.task_type:
                request["taskType"] = self.contract.task_type
            requests.append(request)
        url = f"{self.base_url}/models/{model}:batchEmbedContents"
        async with httpx.AsyncClient(timeout=30.0) as client:
            response = await client.post(
                url,
                params={"key": self.api_key},
                json={"requests": requests},
            )
            if response.is_error:
                raise EmbeddingError(
                    f"Google embedding request failed with status {response.status_code}"
                )
            payload = response.json()
        embeddings = payload.get("embeddings") if isinstance(payload, dict) else None
        if not isinstance(embeddings, list):
            raise EmbeddingError("Google embedding response lacks embeddings")
        return [
            list(item.get("values") or []) if isinstance(item, dict) else []
            for item in embeddings
        ]


class OpenAICompatibleEmbedder:
    def __init__(self, contract: EmbeddingContract) -> None:
        self.contract = contract
        self.base_url = os.environ.get("HMM_EMBEDDING_BASE_URL", "").rstrip("/")
        if not self.base_url:
            raise EmbeddingError("HMM_EMBEDDING_BASE_URL is required")
        self.api_key = os.environ.get("HMM_EMBEDDING_API_KEY", "").strip()
        self.model = os.environ.get("HMM_EMBEDDING_MODEL", contract.model).strip()

    async def embed(self, texts: list[str]) -> list[list[float]]:
        headers = {"content-type": "application/json"}
        if self.api_key:
            headers["authorization"] = f"Bearer {self.api_key}"
        async with httpx.AsyncClient(timeout=30.0) as client:
            response = await client.post(
                f"{self.base_url}/embeddings",
                headers=headers,
                json={"model": self.model, "input": texts},
            )
            if response.is_error:
                raise EmbeddingError(
                    "OpenAI-compatible embedding request failed with status "
                    f"{response.status_code}"
                )
            payload = response.json()
        data = payload.get("data") if isinstance(payload, dict) else None
        if not isinstance(data, list):
            raise EmbeddingError("OpenAI-compatible response lacks data")
        ordered = sorted(
            (item for item in data if isinstance(item, dict)),
            key=lambda item: int(item.get("index") or 0),
        )
        return [list(item.get("embedding") or []) for item in ordered]


def build_embedder(contract: EmbeddingContract) -> Embedder:
    provider = os.environ.get("HMM_EMBEDDING_PROVIDER", "google").strip().lower()
    if provider == "google":
        return GoogleEmbedder(contract)
    if provider == "openai-compatible":
        return OpenAICompatibleEmbedder(contract)
    raise EmbeddingError(
        "HMM_EMBEDDING_PROVIDER must be 'google' or 'openai-compatible'"
    )


def validate_vectors(
    vectors: list[list[float]],
    *,
    expected_count: int,
    dimensions: int,
) -> list[np.ndarray]:
    if len(vectors) != expected_count:
        raise EmbeddingError(
            f"embedding provider returned {len(vectors)} vectors; "
            f"expected {expected_count}"
        )
    result = []
    for index, vector in enumerate(vectors):
        values = np.asarray(vector, dtype=np.float64)
        if values.shape != (dimensions,):
            raise EmbeddingError(
                f"embedding {index} has shape {values.shape}; expected ({dimensions},)"
            )
        if not np.all(np.isfinite(values)):
            raise EmbeddingError(f"embedding {index} contains non-finite values")
        if math.isclose(float(np.linalg.norm(values)), 0.0):
            raise EmbeddingError(f"embedding {index} has zero norm")
        result.append(values)
    return result


async def verify_embedding_contract(
    embedder: Embedder,
    contract: EmbeddingContract,
    reference: np.ndarray,
) -> float:
    vectors = validate_vectors(
        await embedder.embed([contract.probe_text]),
        expected_count=1,
        dimensions=contract.dimensions,
    )
    candidate = vectors[0]
    similarity = float(
        np.dot(candidate, reference)
        / (np.linalg.norm(candidate) * np.linalg.norm(reference))
    )
    if similarity < contract.minimum_cosine_similarity:
        raise EmbeddingError(
            "embedding endpoint is incompatible with the frozen artifact: "
            f"probe cosine similarity {similarity:.8f} < "
            f"{contract.minimum_cosine_similarity:.8f}"
        )
    return similarity


class CachedEmbedder:
    def __init__(self, inner: Embedder, dimensions: int) -> None:
        self.inner = inner
        self.dimensions = dimensions
        self.cache: OrderedDict[str, np.ndarray] = OrderedDict()
        self.lock = asyncio.Lock()

    async def embed(self, texts: list[str]) -> list[np.ndarray]:
        unique = list(dict.fromkeys(texts))
        async with self.lock:
            cached = {text: self.cache[text] for text in unique if text in self.cache}
            for text in cached:
                self.cache.move_to_end(text)
        missing = [text for text in unique if text not in cached]
        fetched: dict[str, np.ndarray] = {}
        if missing:
            values = validate_vectors(
                await self.inner.embed(missing),
                expected_count=len(missing),
                dimensions=self.dimensions,
            )
            fetched = dict(zip(missing, values, strict=True))
        async with self.lock:
            for text, vector in fetched.items():
                self.cache[text] = vector
                self.cache.move_to_end(text)
            while len(self.cache) > MAX_CACHE_ITEMS:
                self.cache.popitem(last=False)
        resolved = {**cached, **fetched}
        return [resolved[text] for text in texts]
