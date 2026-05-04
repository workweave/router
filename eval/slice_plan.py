"""The locked 500-prompt slice composition for Phase 1a.

Two design pressures:

  1. Claude Code traffic is coding-heavy — slice budget skews toward
     coding + tool-calling (LiveCodeBench, Aider Polyglot, BFCL v4,
     BIRD-SQL, tau-bench).
  2. Reviewers will ask "why didn't you use the standard router
     benchmarks?" — a meaningful chunk goes to the consensus set used
     by RouteLLM / RouterBench / GraphRouter (MMLU, GSM8K, MT-Bench,
     HumanEval, MBPP).

Long-context (>20k tokens) is intentionally dropped from PR 2; revisit
in Phase 1b with real long PRs.

The composition can shift during scaffolding but MUST be locked in
prompts.jsonl by the time the full run starts. The shape is:

    SLICES: dict[slice_name, SliceSpec]

with SliceSpec.loader naming the BenchmarkLoader to call (registry
lookup in benchmarks/__init__.py).
"""

from __future__ import annotations

from pydantic import BaseModel


class SliceSpec(BaseModel):
    """A single slice of the eval set."""

    slice: str
    loader: str  # BenchmarkLoader.name
    count: int
    rationale: str


# ---------------------------------------------------------------------------
# Locked composition. Counts sum to 500. Adjustments allowed during
# scaffolding (see SliceSpec.rationale for trade-off context); freeze
# before the full run.
# ---------------------------------------------------------------------------
SLICES: list[SliceSpec] = [
    SliceSpec(
        slice="coding-python",
        loader="livecodebench-python",
        count=45,
        rationale="AvengersPro precedent; contamination-resistant.",
    ),
    SliceSpec(
        slice="coding-ts",
        loader="livecodebench-ts-and-aider-polyglot-ts",
        count=40,
        rationale="Claude Code traffic skews JS/TS.",
    ),
    SliceSpec(
        slice="coding-go",
        loader="aider-polyglot-go",
        count=30,
        rationale="Workweave dogfood language.",
    ),
    SliceSpec(
        slice="coding-rust-cpp-java",
        loader="aider-polyglot-rust-cpp-java",
        count=35,
        rationale="Polyglot coverage.",
    ),
    SliceSpec(
        slice="coding-sql",
        loader="bird-sql",
        count=20,
        rationale="Realistic schema-grounded SQL.",
    ),
    SliceSpec(
        slice="coding-humaneval-mbpp",
        loader="humaneval-mbpp",
        count=25,
        rationale="Router-canon coding consensus (RouterBench + GraphRouter).",
    ),
    SliceSpec(
        slice="tool-calling-single",
        loader="bfcl-v4-simple",
        count=35,
        rationale="Best public tool-calling eval; single-call subset.",
    ),
    SliceSpec(
        slice="tool-calling-parallel-multi",
        loader="bfcl-v4-parallel-multi",
        count=35,
        rationale="Multi-step agentic tool-calling.",
    ),
    SliceSpec(
        slice="tool-calling-agentic",
        loader="tau-bench",
        count=30,
        rationale="AvengersPro precedent; long-horizon tool use.",
    ),
    SliceSpec(
        slice="math-gpqa",
        loader="gpqa-diamond",
        count=25,
        rationale="AvengersPro precedent; reasoning.",
    ),
    SliceSpec(
        slice="math-gsm8k",
        loader="gsm8k",
        count=25,
        rationale="Router-canon math consensus (RouteLLM + RouterBench + GraphRouter).",
    ),
    SliceSpec(
        slice="knowledge-mmlu",
        loader="mmlu",
        count=30,
        rationale="Router-canon knowledge consensus (used by every cited router).",
    ),
    SliceSpec(
        slice="summarization",
        loader="xsum",
        count=25,
        rationale="Default summarization source; CNN/DailyMail or GovReport are alternates.",
    ),
    SliceSpec(
        slice="chat-mt-bench",
        loader="mt-bench",
        count=25,
        rationale="Router-canon chat / LLM-judge consensus (RouteLLM + RouterBench).",
    ),
    SliceSpec(
        slice="multilingual",
        loader="multilingual-coding",
        count=20,
        rationale="Translated subset of coding; sanity coverage across ~10 languages.",
    ),
    SliceSpec(
        slice="edge-cases",
        loader="edge-cases-handcurated",
        count=55,
        rationale="What heuristic is most likely to mis-route on (refusals, ambiguity).",
    ),
]


TOTAL_PROMPTS = sum(s.count for s in SLICES)
if TOTAL_PROMPTS != 500:
    raise ValueError(f"Slice composition must sum to 500; got {TOTAL_PROMPTS}")


# ---------------------------------------------------------------------------
# Extra slices — opt-in only. Not part of the locked Phase 1a 500-prompt
# composition (so they don't perturb the gate numbers in EVAL_RESULTS.md),
# but reachable from compare.py via `--slices <name>`. Use these for
# targeted follow-up evaluations: SWE-bench Verified for "does the router
# pick a strong-coding model on real GitHub-issue tasks?", etc.
# ---------------------------------------------------------------------------
EXTRA_SLICES: list[SliceSpec] = [
    SliceSpec(
        slice="swebench-verified",
        loader="swebench-verified",
        count=25,
        rationale=(
            "Directional check on real GitHub-issue patch generation. "
            "LLM-judge graded — not real SWE-bench pass-rate (which "
            "requires per-instance docker test execution outside this "
            "harness)."
        ),
    ),
]
