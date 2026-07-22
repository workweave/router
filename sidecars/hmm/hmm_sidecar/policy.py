from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any

import numpy as np

from .artifacts import FrozenArtifacts
from .classifier import FrozenClassifier
from .embeddings import CachedEmbedder, Embedder
from .features import (
    classifier_features,
    conversation_sequence,
    raw_hmm_features,
    tool_context_features,
)
from .hmm import FrozenHMM
from .schemas import Candidate, RankedFallback, RoutePreviewResult, RouteResult


def select_roster_group(
    *,
    probabilities: dict[str, float],
    classes: tuple[str, ...],
    clusters: dict[str, Any],
    available_roster_ids: set[str],
) -> tuple[str | None, tuple[str, ...], tuple[RankedFallback, ...]]:
    """Select the first nonempty ranked group and retain every eligible arm."""
    ranked_labels = sorted(
        probabilities,
        key=lambda label: (-probabilities[label], classes.index(label)),
    )
    selected_group: str | None = None
    selected_arms: tuple[str, ...] = ()
    fallback: list[RankedFallback] = []
    for label in ranked_labels:
        cluster = clusters.get(label) or {}
        roster_arms = tuple(str(value) for value in cluster.get("arms") or [])
        eligible_arms = tuple(
            roster_id for roster_id in roster_arms if roster_id in available_roster_ids
        )
        fallback.append(
            RankedFallback(
                group=label,
                probability=float(probabilities[label]),
                roster_arms=roster_arms,
                eligible_arms=eligible_arms,
            )
        )
        if selected_group is None and eligible_arms:
            selected_group = label
            selected_arms = eligible_arms
    return selected_group, selected_arms, tuple(fallback)


def select_roster_arm(
    *,
    probabilities: dict[str, float],
    classes: tuple[str, ...],
    clusters: dict[str, Any],
    available_roster_ids: set[str],
) -> tuple[str, str]:
    label, arms, _ = select_roster_group(
        probabilities=probabilities,
        classes=classes,
        clusters=clusters,
        available_roster_ids=available_roster_ids,
    )
    if label is None or not arms:
        raise ValueError("no candidate is present in the frozen HMM roster")
    return label, arms[0]


def selected_margin(probabilities: dict[str, float], selected_label: str) -> float:
    selected = float(probabilities[selected_label])
    alternatives = [
        float(score)
        for label, score in probabilities.items()
        if label != selected_label
    ]
    return selected - max(alternatives) if alternatives else selected


class FrozenPolicy:
    def __init__(self, artifacts: FrozenArtifacts, embedder: Embedder) -> None:
        self.artifacts = artifacts
        manifest = artifacts.manifest
        self.embedder = CachedEmbedder(embedder, manifest.embedding_contract.dimensions)
        self.hmm = FrozenHMM(artifacts.root / manifest.hmm.path)
        self.classifier = FrozenClassifier(artifacts.root / manifest.classifier.path)
        roster_path = artifacts.root / manifest.roster.path
        roster_raw = roster_path.read_bytes()
        roster = json.loads(roster_raw)
        self.roster_version = hashlib.sha256(roster_raw).hexdigest()
        self.clusters = roster["clusters"]
        cards = json.loads((artifacts.root / manifest.state_cards.path).read_text())
        self.state_cards = {
            int(card["state_id"]): card
            for card in cards
            if isinstance(card, dict) and isinstance(card.get("state_id"), int)
        }

    async def _evaluate(
        self, payload: dict[str, Any], *, allow_empty_candidates: bool
    ) -> tuple[list[Candidate], Any, Any]:
        candidates = [
            Candidate.model_validate(value) for value in payload.get("candidates") or []
        ]
        if not candidates and not allow_empty_candidates:
            raise ValueError("route request has no candidates")
        by_roster = {candidate.roster_id: candidate for candidate in candidates}
        if len(by_roster) != len(candidates):
            raise ValueError("candidate roster_id values must be unique")
        turns = conversation_sequence(payload)
        embeddings = await self.embedder.embed([turn.text for turn in turns])
        readout = self.hmm.posterior(raw_hmm_features(embeddings, turns))
        feature_row = classifier_features(
            embedding=embeddings[-1],
            gamma=readout.gamma[-1],
            state=readout.state,
            previous_state=readout.previous_state,
            position=len(turns) - 1,
            prefix_length=len(turns),
            tool_context=tool_context_features(payload),
        )
        classification = self.classifier.predict(feature_row)
        return candidates, readout, classification

    async def preview(self, payload: dict[str, Any]) -> RoutePreviewResult:
        candidates, readout, classification = await self._evaluate(
            payload, allow_empty_candidates=True
        )
        selected_group, eligible_arms, ranked_fallback = select_roster_group(
            probabilities=classification.probabilities,
            classes=tuple(self.classifier.classes),
            clusters=self.clusters,
            available_roster_ids={candidate.roster_id for candidate in candidates},
        )
        return RoutePreviewResult(
            route_id=str(payload.get("route_id") or ""),
            policy_artifact_id=self.artifacts.manifest.model_id,
            policy_artifact_sha256=self.artifacts.package_sha256,
            roster_sha256=self.roster_version,
            hmm_state_id=readout.state,
            hmm_state_path=tuple(readout.state_path),
            hmm_state_probabilities=tuple(float(value) for value in readout.gamma[-1]),
            class_order=tuple(self.classifier.classes),
            class_probabilities=classification.probabilities,
            ranked_fallback=ranked_fallback,
            selected_group=selected_group,
            eligible_roster_ids=eligible_arms,
        )

    async def route(self, payload: dict[str, Any]) -> RouteResult:
        candidates, readout, classification = await self._evaluate(
            payload, allow_empty_candidates=False
        )
        by_roster = {candidate.roster_id: candidate for candidate in candidates}
        selected_label, selected_roster, ranked_fallback = select_roster_group(
            probabilities=classification.probabilities,
            classes=tuple(self.classifier.classes),
            clusters=self.clusters,
            available_roster_ids=set(by_roster),
        )
        if selected_label is None or not selected_roster:
            raise ValueError("no candidate is present in the frozen HMM roster")
        selected_roster_id = (
            selected_roster[0]
            if isinstance(selected_roster, tuple)
            else selected_roster
        )
        selected = by_roster[selected_roster_id]
        card = self.state_cards.get(readout.state, {})
        state_label = str(card.get("name") or f"state_{readout.state}")
        candidate_scores = {
            candidate.roster_id: max(
                (
                    classification.probabilities.get(label, 0.0)
                    for label, cluster in self.clusters.items()
                    if candidate.roster_id in (cluster.get("arms") or [])
                ),
                default=0.0,
            )
            for candidate in candidates
        }
        route_id = str(payload.get("route_id") or "")
        score = float(classification.probabilities[selected_label])
        margin = selected_margin(classification.probabilities, selected_label)
        reason = (
            f"classifier group {selected_label!r} "
            f"(p={score:.3f}, margin={margin:.3f}, "
            f"raw_top={classification.label!r}); "
            f"frozen roster arm {selected_roster_id!r}"
        )
        return RouteResult(
            route_id=route_id,
            selected_roster_id=selected.roster_id,
            selected_provider=selected.provider,
            score=score,
            candidate_scores=candidate_scores,
            reason=reason,
            state_label=state_label,
            policy_group=selected_label,
            policy_label=selected_label,
            policy_route_key=f"hmm:{readout.state}:{selected_label}",
            confidence=score,
            margin=margin,
            propensity=1.0,
            policy_artifact_id=self.artifacts.manifest.model_id,
            policy_artifact_sha256=self.artifacts.package_sha256,
            roster_version=self.roster_version,
            ranked_fallback=ranked_fallback,
            debug={
                "hmm_state_id": readout.state,
                "hmm_state_path": list(readout.state_path),
                "hmm_posterior": readout.confidence,
                "hmm_posterior_margin": readout.margin,
                "classifier_probs": classification.probabilities,
                "frozen_policy": True,
            },
        )
