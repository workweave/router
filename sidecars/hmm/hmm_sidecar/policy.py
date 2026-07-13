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
        ranked_labels = sorted(
            classification.probabilities,
            key=lambda label: (
                -classification.probabilities[label],
                tuple(self.classifier.classes).index(label),
            ),
        )
        selected_label = ""
        selected_roster = ""
        for label in ranked_labels:
            cluster = self.clusters.get(label) or {}
            for roster_id in cluster.get("arms") or []:
                if roster_id in by_roster:
                    selected_label = label
                    selected_roster = roster_id
                    break
            if selected_roster:
                break
        if not selected_roster:
            raise ValueError("no candidate is present in the frozen HMM roster")
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
        reason = (
            f"classifier {classification.label!r} "
            f"(p={classification.confidence:.3f}, margin={classification.margin:.3f}); "
            f"frozen roster arm {selected_roster!r}"
        )
        return RouteResult(
            route_id=route_id,
            selected_roster_id=selected.roster_id,
            selected_provider=selected.provider,
            score=float(classification.probabilities[selected_label]),
            candidate_scores=candidate_scores,
            reason=reason,
            state_label=state_label,
            policy_group=selected_label,
            policy_label=selected_label,
            policy_route_key=f"hmm:{readout.state}:{selected_label}",
            confidence=float(classification.probabilities[selected_label]),
            margin=classification.margin,
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
