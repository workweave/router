from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path

import numpy as np
import xgboost


@dataclass(frozen=True)
class Classification:
    label: str
    probabilities: dict[str, float]
    confidence: float
    margin: float


class FrozenClassifier:
    def __init__(self, root: Path) -> None:
        self.metadata = json.loads((root / "metadata.json").read_text())
        self.classes = tuple(json.loads((root / "classes.json").read_text()))
        if tuple(self.metadata.get("classes") or ()) != self.classes:
            raise ValueError("classifier class order does not match metadata")
        self.feature_dim = int(self.metadata["feature_dim"])
        self.model = xgboost.Booster()
        self.model.load_model(root / "model.json")

    def predict(self, features: np.ndarray) -> Classification:
        values = np.asarray(features, dtype=np.float32).reshape(-1)
        if values.shape != (self.feature_dim,):
            raise ValueError(
                f"classifier expects {self.feature_dim} features; got {values.shape}"
            )
        probabilities = np.asarray(
            self.model.predict(xgboost.DMatrix(values[None, :]))[0],
            dtype=np.float64,
        )
        order = np.argsort(probabilities)[::-1]
        top = int(order[0])
        confidence = float(probabilities[top])
        margin = (
            float(probabilities[order[0]] - probabilities[order[1]])
            if len(order) > 1
            else confidence
        )
        return Classification(
            self.classes[top],
            {
                label: float(probabilities[index])
                for index, label in enumerate(self.classes)
            },
            confidence,
            margin,
        )
