import pytest

from redharness.scoring import pass_at_k, weighted_mean


def test_pass_at_k_estimator() -> None:
    assert pass_at_k(4, 0, 1) == 0.0
    assert pass_at_k(4, 4, 1) == 1.0
    assert pass_at_k(4, 1, 1) == pytest.approx(0.25)
    assert pass_at_k(4, 1, 2) == pytest.approx(0.5)
    assert pass_at_k(4, 1, 4) == 1.0


def test_weighted_mean() -> None:
    assert weighted_mean([(100.0, 1.0), (0.0, 3.0)]) == 25.0
