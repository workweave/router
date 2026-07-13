from __future__ import annotations

from pathlib import Path

import numpy as np

from hmm_sidecar.hmm import FrozenHMM


def write_hmm(path: Path) -> None:
    np.savez_compressed(
        path,
        pca_mean=np.asarray([0.0, 0.0]),
        pca_components=np.eye(2),
        pca_whiten=np.asarray(False),
        pca_explained_variance=np.asarray([1.0, 1.0]),
        startprob=np.asarray([0.9, 0.1]),
        transmat=np.asarray([[0.95, 0.05], [0.05, 0.95]]),
        means=np.asarray([[0.0, 0.0], [5.0, 5.0]]),
        variances=np.ones((2, 2)),
    )


def test_hmm_posterior_tracks_the_emission_state(tmp_path: Path) -> None:
    path = tmp_path / "hmm.npz"
    write_hmm(path)
    hmm = FrozenHMM(path)

    readout = hmm.posterior(np.asarray([[0.1, -0.1], [5.1, 4.9]]))

    np.testing.assert_allclose(readout.gamma.sum(axis=1), 1.0)
    assert readout.state_path == (0, 1)
    assert readout.previous_state == 0
    assert readout.state == 1
    assert readout.confidence > 0.99


def test_pca_whitening_matches_the_exported_contract(tmp_path: Path) -> None:
    path = tmp_path / "hmm.npz"
    np.savez_compressed(
        path,
        pca_mean=np.asarray([1.0, 2.0]),
        pca_components=np.eye(2),
        pca_whiten=np.asarray(True),
        pca_explained_variance=np.asarray([4.0, 9.0]),
        startprob=np.asarray([0.5, 0.5]),
        transmat=np.asarray([[0.5, 0.5], [0.5, 0.5]]),
        means=np.asarray([[0.0, 0.0], [1.0, 1.0]]),
        variances=np.ones((2, 2)),
    )

    projected = FrozenHMM(path).project(np.asarray([[3.0, 5.0]]))

    np.testing.assert_allclose(projected, [[1.0, 1.0]])
