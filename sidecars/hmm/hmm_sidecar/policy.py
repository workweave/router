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
from .schemas import Candidate, RouteResult


def select_roster_arm(
    *,
    probabilities: dict[str, float],
    classes: tuple[str, ...],
    clusters: dict[str, Any],
    available_roster_ids: set[str],
) -> tuple[str, str]:
    ranked_labels = sorted(
        probabilities,
        key=lambda label: (-probabilities[label], classes.index(label)),
    )
    for label in ranked_labels:
        cluster = clusters.get(label) or {}
        for roster_id in cluster.get("arms") or []:
            if roster_id in available_roster_ids:
                return label, roster_id
    raise ValueError("no candidate is present in the frozen HMM roster")


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

    async def route(self, payload: dict[str, Any]) -> RouteResult:
        candidates = [
            Candidate.model_validate(value) for value in payload.get("candidates") or []
        ]
        if not candidates:
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
        selected_label, selected_roster = select_roster_arm(
            probabilities=classification.probabilities,
            classes=tuple(self.classifier.classes),
            clusters=self.clusters,
            available_roster_ids=set(by_roster),
        )
        selected = by_roster[selected_roster]
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
            f"frozen roster arm {selected_roster!r}"
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
            debug={
                "hmm_state_id": readout.state,
                "hmm_state_path": list(readout.state_path),
                "hmm_posterior": readout.confidence,
                "hmm_posterior_margin": readout.margin,
                "classifier_probs": classification.probabilities,
                "frozen_policy": True,
            },
        )
