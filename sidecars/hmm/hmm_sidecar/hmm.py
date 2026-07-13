from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

import numpy as np


def _logsumexp(values: np.ndarray, axis: int) -> np.ndarray:
    maximum = np.max(values, axis=axis, keepdims=True)
    result = maximum + np.log(
        np.sum(np.exp(values - maximum), axis=axis, keepdims=True)
    )
    return np.squeeze(result, axis=axis)


@dataclass(frozen=True)
class HMMReadout:
    gamma: np.ndarray
    state_path: tuple[int, ...]
    state: int
    previous_state: int
    confidence: float
    margin: float


class FrozenHMM:
    def __init__(self, path: Path) -> None:
        with np.load(path, allow_pickle=False) as payload:
            self.pca_mean = np.asarray(payload["pca_mean"], dtype=np.float64)
            self.pca_components = np.asarray(
                payload["pca_components"], dtype=np.float64
            )
            self.pca_whiten = bool(np.asarray(payload["pca_whiten"]).item())
            self.pca_explained_variance = np.asarray(
                payload["pca_explained_variance"], dtype=np.float64
            )
            self.startprob = np.asarray(payload["startprob"], dtype=np.float64)
            self.transmat = np.asarray(payload["transmat"], dtype=np.float64)
            self.means = np.asarray(payload["means"], dtype=np.float64)
            self.variances = np.asarray(payload["variances"], dtype=np.float64)
        states = self.startprob.shape[0]
        if self.transmat.shape != (states, states):
            raise ValueError("invalid HMM transition matrix shape")
        if self.means.shape[0] != states or self.variances.shape != self.means.shape:
            raise ValueError("invalid HMM emission parameter shape")

    def project(self, raw: np.ndarray) -> np.ndarray:
        projected = (raw - self.pca_mean) @ self.pca_components.T
        if self.pca_whiten:
            projected = projected / np.sqrt(self.pca_explained_variance)
        return projected

    def posterior(self, raw: np.ndarray) -> HMMReadout:
        observations = self.project(np.asarray(raw, dtype=np.float64))
        gamma = self._forward_backward(observations)
        path = tuple(int(value) for value in np.argmax(gamma, axis=1))
        latest = gamma[-1]
        order = np.argsort(latest)[::-1]
        state = int(order[0])
        confidence = float(latest[state])
        margin = (
            float(latest[order[0]] - latest[order[1]]) if len(order) > 1 else confidence
        )
        previous = path[-2] if len(path) > 1 else state
        return HMMReadout(gamma, path, state, previous, confidence, margin)

    def _forward_backward(self, observations: np.ndarray) -> np.ndarray:
        log_emit = self._log_emissions(observations)
        log_start = np.log(np.maximum(self.startprob, 1e-300))
        log_trans = np.log(np.maximum(self.transmat, 1e-300))
        timesteps, states = log_emit.shape
        alpha = np.empty((timesteps, states), dtype=np.float64)
        alpha[0] = log_start + log_emit[0]
        for index in range(1, timesteps):
            alpha[index] = log_emit[index] + _logsumexp(
                alpha[index - 1][:, None] + log_trans,
                axis=0,
            )
        beta = np.zeros((timesteps, states), dtype=np.float64)
        for index in range(timesteps - 2, -1, -1):
            beta[index] = _logsumexp(
                log_trans + log_emit[index + 1][None, :] + beta[index + 1][None, :],
                axis=1,
            )
        log_gamma = alpha + beta
        log_gamma -= _logsumexp(log_gamma, axis=1)[:, None]
        return np.exp(log_gamma)

    def _log_emissions(self, observations: np.ndarray) -> np.ndarray:
        variances = np.maximum(self.variances, 1e-12)
        delta = observations[:, None, :] - self.means[None, :, :]
        return -0.5 * (
            np.sum(np.log(2.0 * np.pi * variances), axis=1)[None, :]
            + np.sum((delta * delta) / variances[None, :, :], axis=2)
        )
