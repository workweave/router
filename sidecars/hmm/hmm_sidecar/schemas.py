from __future__ import annotations

from pathlib import Path
from typing import Any

from pydantic import BaseModel, ConfigDict, Field, model_validator

from . import PACKAGE_SCHEMA_VERSION


class FrozenModel(BaseModel):
    model_config = ConfigDict(extra="forbid", frozen=True)


class EmbeddingContract(FrozenModel):
    model: str = Field(min_length=1)
    dimensions: int = Field(gt=0)
    task_type: str | None = None
    probe_text: str = Field(min_length=1)
    probe_vector_file: str = Field(min_length=1)
    minimum_cosine_similarity: float = Field(gt=0.0, le=1.0)


class ComponentFiles(FrozenModel):
    path: str = Field(min_length=1)


class ClassifierFiles(ComponentFiles):
    classes: tuple[str, ...] = Field(min_length=2)


class HMMFiles(ComponentFiles):
    states: int = Field(gt=1)


class FrozenPackageManifest(FrozenModel):
    schema_version: str
    model_id: str = Field(min_length=1)
    package_sha256: str | None = None
    embedding_contract: EmbeddingContract
    classifier: ClassifierFiles
    hmm: HMMFiles
    roster: ComponentFiles
    state_cards: ComponentFiles
    training_privacy: dict[str, Any]
    files: dict[str, str]

    @model_validator(mode="after")
    def validate_contract(self) -> "FrozenPackageManifest":
        if self.schema_version != PACKAGE_SCHEMA_VERSION:
            raise ValueError(
                f"unsupported package schema {self.schema_version!r}; "
                f"expected {PACKAGE_SCHEMA_VERSION!r}"
            )
        for relative, digest in self.files.items():
            path = Path(relative)
            if path.is_absolute() or ".." in path.parts:
                raise ValueError(f"unsafe manifest file path: {relative!r}")
            if len(digest) != 64 or any(
                char not in "0123456789abcdef" for char in digest
            ):
                raise ValueError(f"invalid sha256 for {relative!r}")
        return self


class Candidate(FrozenModel):
    roster_id: str = Field(min_length=1)
    catalog_id: str = Field(min_length=1)
    provider: str = Field(min_length=1)
    upstream_id: str = ""
    preference_rank: int | None = None
    input_usd_per_1m: float = 0.0
    output_usd_per_1m: float = 0.0
    estimated_cost_usd: float = 0.0
    capabilities: dict[str, Any] = Field(default_factory=dict)


class RouteResult(FrozenModel):
    route_id: str
    selected_roster_id: str
    selected_provider: str
    score: float
    candidate_scores: dict[str, float]
    reason: str
    state_label: str
    policy_group: str
    policy_label: str
    policy_route_key: str
    confidence: float
    margin: float
    propensity: float
    policy_artifact_id: str
    policy_artifact_sha256: str
    roster_version: str
    debug: dict[str, Any]
