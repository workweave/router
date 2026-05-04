"""Pareto plot + ranking table.

Pure module: input is a list of RouterResult, output is (a) a
matplotlib figure ready to save and (b) a markdown-formatted table
ready to embed in EVAL_RESULTS.md.

`pareto_frontier(points)` returns the subset of points that no other
point dominates on both axes (low cost AND high quality). Used by the
plotter to highlight the frontier and by the gate read to identify the
operating-point candidates.
"""

from __future__ import annotations

from dataclasses import dataclass

from eval.types import RouterResult


@dataclass(frozen=True)
class ParetoPoint:
    label: str
    cost: float
    quality: float


def pareto_frontier(points: list[ParetoPoint]) -> list[ParetoPoint]:
    """Return the subset of `points` that lie on the cost↓ / quality↑
    Pareto frontier. Ties broken by label so the result is stable.
    """
    if not points:
        return []
    # Sort by cost ascending, then quality descending: a point is on
    # the frontier iff its quality strictly exceeds every prior
    # point's quality.
    ordered = sorted(points, key=lambda p: (p.cost, -p.quality, p.label))
    out: list[ParetoPoint] = []
    best_q = float("-inf")
    for p in ordered:
        if p.quality > best_q:
            out.append(p)
            best_q = p.quality
    return out


def render_plot(points: list[ParetoPoint], *, title: str = "Cost vs quality"):
    """Build a matplotlib Figure. Caller saves with fig.savefig."""
    import matplotlib.pyplot as plt  # lazy

    frontier = set(p.label for p in pareto_frontier(points))
    fig, ax = plt.subplots(figsize=(7.0, 5.0))
    for p in points:
        marker = "o" if p.label in frontier else "x"
        color = "#1f77b4" if p.label in frontier else "#888888"
        ax.scatter([p.cost], [p.quality], marker=marker, color=color, s=80)
        ax.annotate(p.label, (p.cost, p.quality), xytext=(6, 6), textcoords="offset points")
    # Connect the frontier with a dashed line for visual emphasis.
    front_sorted = sorted(pareto_frontier(points), key=lambda p: p.cost)
    if len(front_sorted) >= 2:
        ax.plot(
            [p.cost for p in front_sorted],
            [p.quality for p in front_sorted],
            linestyle="--",
            color="#1f77b4",
            alpha=0.5,
        )
    ax.set_xlabel("Total cost (USD)")
    ax.set_ylabel("Mean quality (judge ensemble, 0-1)")
    ax.set_title(title)
    ax.grid(True, alpha=0.3)
    fig.tight_layout()
    return fig


def render_table(results: list[RouterResult]) -> str:
    """Markdown table for EVAL_RESULTS.md."""
    header = (
        "| Router | n | Total cost (USD) | Mean quality | Reference pass-rate | P50 latency (ms) | P95 latency (ms) |\n"
        "|---|---:|---:|---:|---:|---:|---:|\n"
    )
    rows = []
    for r in results:
        ref = "—" if r.reference_pass_rate is None else f"{r.reference_pass_rate:.2%}"
        rows.append(
            f"| `{r.router}` | {r.n_prompts} | ${r.total_cost_usd:.2f} | "
            f"{r.mean_quality:.3f} | {ref} | {r.p50_latency_ms} | {r.p95_latency_ms} |"
        )
    return header + "\n".join(rows) + "\n"


def to_points(results: list[RouterResult]) -> list[ParetoPoint]:
    return [ParetoPoint(label=r.router, cost=r.total_cost_usd, quality=r.mean_quality) for r in results]
