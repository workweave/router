from __future__ import annotations

import asyncio

import numpy as np
import pytest
import respx
from httpx import Response

import hmm_sidecar.embeddings as embedding_module
from hmm_sidecar.embeddings import (
    CachedEmbedder,
    EmbeddingError,
    GoogleEmbedder,
    OpenAICompatibleEmbedder,
    verify_embedding_contract,
)
from hmm_sidecar.schemas import EmbeddingContract


class FakeEmbedder:
    def __init__(self, vector: list[float]) -> None:
        self.vector = vector

    async def embed(self, texts: list[str]) -> list[list[float]]:
        return [self.vector for _ in texts]


class DelayedEmbedder:
    def __init__(self) -> None:
        self.started = asyncio.Event()
        self.release = asyncio.Event()

    async def embed(self, texts: list[str]) -> list[list[float]]:
        if texts == ["delayed"]:
            self.started.set()
            await self.release.wait()
        return [[1.0, 2.0, 3.0] for _ in texts]


def contract() -> EmbeddingContract:
    return EmbeddingContract(
        model="google/gemini-embedding-2",
        dimensions=3,
        probe_text="probe",
        probe_vector_file="probe.npy",
        minimum_cosine_similarity=0.99999,
    )


async def test_accepts_the_artifact_embedding_space() -> None:
    reference = np.asarray([1.0, 2.0, 3.0])

    similarity = await verify_embedding_contract(
        FakeEmbedder([1.0, 2.0, 3.0]), contract(), reference
    )

    assert similarity == pytest.approx(1.0)


async def test_rejects_an_incompatible_same_dimension_embedding() -> None:
    reference = np.asarray([1.0, 0.0, 0.0])

    with pytest.raises(EmbeddingError, match="incompatible"):
        await verify_embedding_contract(
            FakeEmbedder([0.0, 1.0, 0.0]), contract(), reference
        )


async def test_rejects_wrong_embedding_dimensions() -> None:
    with pytest.raises(EmbeddingError, match="shape"):
        await verify_embedding_contract(
            FakeEmbedder([1.0, 2.0]), contract(), np.asarray([1.0, 2.0, 3.0])
        )


@respx.mock
async def test_google_provider_uses_the_artifact_model_and_dimensions(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("GOOGLE_API_KEY", "test-key")
    monkeypatch.setenv("HMM_EMBEDDING_MODEL", "google/gemini-embedding-2")
    request = respx.post(
        "https://generativelanguage.googleapis.com/v1beta/models/"
        "gemini-embedding-2:batchEmbedContents?key=test-key"
    ).mock(return_value=Response(200, json={"embeddings": [{"values": [1, 2, 3]}]}))

    vectors = await GoogleEmbedder(contract()).embed(["hello"])

    assert vectors == [[1, 2, 3]]
    assert request.called
    body = request.calls.last.request.content.decode()
    assert '"model":"models/gemini-embedding-2"' in body
    assert '"outputDimensionality":3' in body


@respx.mock
async def test_openai_compatible_provider_restores_response_order(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("HMM_EMBEDDING_BASE_URL", "https://embed.example/v1")
    monkeypatch.setenv("HMM_EMBEDDING_API_KEY", "test-key")
    respx.post("https://embed.example/v1/embeddings").mock(
        return_value=Response(
            200,
            json={
                "data": [
                    {"index": 1, "embedding": [4, 5, 6]},
                    {"index": 0, "embedding": [1, 2, 3]},
                ]
            },
        )
    )

    vectors = await OpenAICompatibleEmbedder(contract()).embed(["a", "b"])

    assert vectors == [[1, 2, 3], [4, 5, 6]]


async def test_cache_returns_hits_that_are_evicted_during_a_concurrent_fetch(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(embedding_module, "MAX_CACHE_ITEMS", 1)
    inner = DelayedEmbedder()
    embedder = CachedEmbedder(inner, dimensions=3)
    await embedder.embed(["cached"])

    delayed = asyncio.create_task(embedder.embed(["cached", "delayed"]))
    await inner.started.wait()
    await embedder.embed(["evicting"])
    inner.release.set()

    vectors = await delayed

    assert len(vectors) == 2
    np.testing.assert_array_equal(vectors[0], [1.0, 2.0, 3.0])
    np.testing.assert_array_equal(vectors[1], [1.0, 2.0, 3.0])
