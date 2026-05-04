"""Unit tests for pareto_frontier + render_table."""

from eval.pareto import ParetoPoint, pareto_frontier, render_table
from eval.types import RouterResult


def test_pareto_frontier_filters_dominated_points():
    pts = [
        ParetoPoint("opus", cost=10.0, quality=0.9),
        ParetoPoint("sonnet", cost=5.0, quality=0.85),
        ParetoPoint("haiku", cost=1.0, quality=0.7),
        ParetoPoint("dominated", cost=6.0, quality=0.6),
    ]
    front = {p.label for p in pareto_frontier(pts)}
    assert front == {"opus", "sonnet", "haiku"}


def test_pareto_frontier_breaks_cost_ties_by_quality():
    pts = [
        ParetoPoint("a", cost=5.0, quality=0.5),
        ParetoPoint("b", cost=5.0, quality=0.7),
    ]
    front = pareto_frontier(pts)
    # Cheaper-or-equal cost + higher quality → only `b` is on the frontier.
    assert [p.label for p in front] == ["b"]


def test_pareto_frontier_with_one_point():
    pts = [ParetoPoint("only", cost=1.0, quality=0.5)]
    assert pareto_frontier(pts) == pts


def test_pareto_frontier_with_zero_points():
    assert pareto_frontier([]) == []


def test_pareto_frontier_strictly_increasing_quality_required():
    """Two points with the same quality but different cost: only the cheaper survives."""
    pts = [
        ParetoPoint("cheap", cost=1.0, quality=0.5),
        ParetoPoint("expensive", cost=2.0, quality=0.5),
    ]
    front = pareto_frontier(pts)
    assert [p.label for p in front] == ["cheap"]


def test_render_table_includes_each_router_row():
    results = [
        RouterResult(
            router="always-opus",
            n_prompts=10,
            total_cost_usd=1.23,
            mean_quality=0.85,
            reference_pass_rate=0.7,
            p50_latency_ms=400,
            p95_latency_ms=900,
            model_picks={"claude-opus-4-7": 10},
        ),
        RouterResult(
            router="v0.2-cluster",
            n_prompts=10,
            total_cost_usd=0.50,
            mean_quality=0.80,
            reference_pass_rate=None,
            p50_latency_ms=350,
            p95_latency_ms=850,
            model_picks={"claude-opus-4-7": 4, "claude-haiku-4-5": 6},
        ),
    ]
    md = render_table(results)
    assert "`always-opus`" in md
    assert "`v0.2-cluster`" in md
    assert "$1.23" in md
    assert "$0.50" in md
    # `reference_pass_rate=None` renders as em-dash.
    assert "—" in md
