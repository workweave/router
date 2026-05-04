"""Tier 2 — difficulty-judge proxy for routing-decision sanity.

For prompts that aren't in our bench cache (real Claude Code traffic,
custom slices, brand-new HF datasets), Tier 1's free held-out regret
doesn't apply. Instead, call one cheap LLM per prompt to score
"required reasoning depth" 1-5, then check whether the cluster
router's pick lines up with the difficulty band.

This is intentionally a single-judge weak signal — not a replacement
for the GPT-5 + Gemini ensemble in router/eval/. Use it to catch
*directional* misrouting between iterations:

  * hard prompt (≥4) routed to a cheap model → router under-powered
  * easy prompt (≤2) routed to a frontier model → router wasteful

Usage:

    poetry run python difficulty_judge.py \\
        --prompts ../eval/results/<run-id>/prompts.jsonl \\
        --version v0.5 \\
        --judge-model claude-haiku-4-5 \\
        --concurrency 8

Costs roughly $0.001-0.003 per prompt with haiku-4-5 (200 input
tokens of rubric + ~50-100 output tokens of JSON), so a 220-prompt
sweep is well under $1.

Authentication: reads ANTHROPIC_API_KEY from the environment (same as
the trainer / eval harness). Anthropic-only by design — adding
OpenAI/Google would require duplicating the wire-format call sites
without changing the signal.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import sys
import time
import urllib.error
import urllib.request
from collections import Counter, defaultdict
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field
from pathlib import Path
from typing import Dict, List, Optional

import numpy as np

from bench_walker import load_bench
from holdout_eval import (
    embed_batch,
    load_artifact_bundle,
    load_embedder,
    simulate_cluster_route,
    split_holdout,
)
from train_cluster_router import ARTIFACTS_DIR, ASSETS_DIR, EMBED_DIM, load_registry, read_latest


# Manual quality tiers — set against published bench position, not
# cost. Cost-aware routing (which we're trying to evaluate) deliberately
# decouples the two: gemini-3.1-pro is frontier-quality at mid-cost,
# while claude-sonnet-4-5 is mid-quality at mid-cost. If a vendor ships
# a new tier, bump this table; otherwise the unmapped fallback is "mid".
MODEL_TIER: Dict[str, int] = {
    # Frontier (3): top public-bench performers as of April 2026.
    "claude-opus-4-7":              3,
    "gpt-5.5":                      3,
    "gpt-5.5-pro":                  3,
    "gemini-3-pro-preview":         3,
    "gemini-3.1-pro-preview":       3,
    # Mid (2): strong general-purpose models, often the value pick.
    "claude-sonnet-4-5":            2,
    "gpt-4.1":                      2,
    "gpt-5.5-mini":                 2,
    "gemini-3-flash-preview":       2,
    # Cheap (1): fast/small models tuned for cost.
    "claude-haiku-4-5":             1,
    "gemini-3.1-flash-lite-preview": 1,
    "gpt-5.5-nano":                 1,
}


def model_tier(model: str) -> int:
    """Look up a model's quality tier; unmapped models default to 2
    (mid) so a freshly-added registry entry doesn't spuriously trip the
    fit-check until the table is updated."""
    return MODEL_TIER.get(model, 2)


def expected_tier(difficulty: int) -> int:
    """Map difficulty 1..5 to the *minimum* model tier that should
    handle it. The fit check is asymmetric: tier ≥ expected is fine
    (a frontier model on an easy prompt is wasteful but not wrong);
    tier < expected is under-powered routing.

    | difficulty | expected tier |
    |------------|---------------|
    | 1-2        | 1 (cheap OK)  |
    | 3          | 2 (mid+)      |
    | 4-5        | 3 (frontier)  |
    """
    if difficulty <= 2:
        return 1
    if difficulty == 3:
        return 2
    return 3


JUDGE_PROMPT_TEMPLATE = """\
You are evaluating a single prompt to estimate the minimum reasoning \
capability an LLM needs to handle it well. Score on a 1-5 scale:

  1 = trivial: lookup, simple completion, basic formatting. Any LLM solves it.
  2 = simple: short coding task, basic Q&A. Fast small models are fine.
  3 = medium: multi-step reasoning, schema-grounded SQL, mid-size code task.
              Mid-tier models recommended.
  4 = hard: complex algorithm, subtle math, multi-file code change.
              Frontier model strongly preferred.
  5 = expert: novel problem, deep reasoning, expert-only domain.
              Only the strongest model has a real shot.

Return ONLY a JSON object: {{"score": <int 1-5>, "why": "<one short sentence>"}}.
No prose outside the JSON.

Prompt to score:
---
{prompt}
---
"""


@dataclass
class JudgeRow:
    prompt_id: str
    prompt_text: str
    difficulty: Optional[int] = None
    why: str = ""
    error: Optional[str] = None
    picks: Dict[str, str] = field(default_factory=dict)


def load_bench_holdout(
    cache_dir: Path,
    holdout_frac: float,
    seed: int,
) -> List[JudgeRow]:
    """Build a JudgeRow list from the bench cache's held-out split — the
    same 3023 prompts holdout_eval.py evaluates regret on (when
    holdout_frac=0.2, seed=42). Prompt IDs are content-addressed so
    cross-referencing with a holdout_eval JSONL dump is trivial.

    Pulls the bench-column-to-deployed-model mapping from the latest
    artifact registry, same as holdout_eval. Drops prompts with no
    score rows (a held-out prompt that produced no records under any
    column we care about isn't useful for Tier 1 OR Tier 2)."""
    latest = read_latest()
    _, bench_to_deployed = load_registry(ARTIFACTS_DIR / latest)
    prompts, scores = load_bench(cache_dir, bench_to_deployed)
    _, held_idx = split_holdout(prompts, holdout_frac, seed)
    rows: List[JudgeRow] = []
    for i in held_idx:
        prompt = prompts[i]
        if not scores.get(prompt):
            continue
        # Stable ID: bench-<index>-<sha8(prompt)>. The index pins the
        # ordering across runs at the same (holdout_frac, seed); the
        # hash makes IDs survive a future change to the bench cache
        # ordering.
        h = hashlib.sha256(prompt.encode("utf-8")).hexdigest()[:8]
        rows.append(JudgeRow(prompt_id=f"bench-{i:05d}-{h}", prompt_text=prompt))
    return rows


def load_prompts(path: Path) -> List[JudgeRow]:
    """Load a JSONL of prompts. Tolerates both the eval harness shape
    (``prompt_text`` + ``prompt_id``) and a minimal ``{"prompt": "..."}``
    so this script also works with hand-curated lists."""
    rows: List[JudgeRow] = []
    for i, line in enumerate(path.read_text().splitlines()):
        line = line.strip()
        if not line:
            continue
        try:
            obj = json.loads(line)
        except json.JSONDecodeError as e:
            sys.exit(f"{path}:{i+1}: {e}")
        text = obj.get("prompt_text") or obj.get("prompt") or ""
        if not text:
            continue
        pid = obj.get("prompt_id") or f"prompt-{i:04d}"
        rows.append(JudgeRow(prompt_id=pid, prompt_text=text))
    if not rows:
        sys.exit(f"{path}: no prompts loaded")
    return rows


def _extract_first_json_object(text: str) -> Optional[str]:
    """Return the first balanced {...} substring in ``text``, or None.
    Tracks brace depth and ignores braces inside double-quoted strings
    (with backslash escapes), so it survives stray prose before/after
    and a second {...} block later in the response."""
    depth = 0
    start = -1
    in_str = False
    escape = False
    for i, ch in enumerate(text):
        if in_str:
            if escape:
                escape = False
            elif ch == "\\":
                escape = True
            elif ch == '"':
                in_str = False
            continue
        if ch == '"':
            in_str = True
        elif ch == "{":
            if depth == 0:
                start = i
            depth += 1
        elif ch == "}":
            if depth == 0:
                continue
            depth -= 1
            if depth == 0 and start >= 0:
                return text[start : i + 1]
    return None


def call_judge(api_key: str, model: str, prompt_text: str, max_chars: int = 8000) -> tuple[Optional[int], str, Optional[str]]:
    """One synchronous Anthropic Messages call. Returns (difficulty,
    why, error). Truncates the prompt input at max_chars to keep judge
    cost bounded for SWE-bench-shaped (multi-KB) prompts; the judge
    only needs enough text to estimate complexity, not the full
    prompt verbatim.
    """
    truncated = prompt_text[:max_chars]
    if len(prompt_text) > max_chars:
        truncated += "\n\n[...truncated for difficulty judging...]"
    body = json.dumps({
        "model": model,
        "max_tokens": 200,
        "messages": [
            {"role": "user", "content": JUDGE_PROMPT_TEMPLATE.format(prompt=truncated)},
        ],
    }).encode("utf-8")
    req = urllib.request.Request(
        "https://api.anthropic.com/v1/messages",
        data=body,
        method="POST",
        headers={
            "x-api-key": api_key,
            "anthropic-version": "2023-06-01",
            "content-type": "application/json",
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            payload = json.loads(resp.read())
    except urllib.error.HTTPError as e:
        return None, "", f"HTTP {e.code}: {e.read()[:200].decode('utf-8', 'replace')}"
    except (urllib.error.URLError, TimeoutError) as e:
        return None, "", str(e)
    text = ""
    for block in payload.get("content", []):
        if isinstance(block, dict) and block.get("type") == "text":
            text += block.get("text", "")
    # The model is told to emit only JSON, but we tolerate stray prose
    # by extracting the first balanced {...} object. Greedy regex would
    # span "{a}...{b}" across multiple blocks and fail to parse.
    blob = _extract_first_json_object(text)
    if blob is None:
        return None, text.strip()[:200], "no JSON in judge response"
    try:
        parsed = json.loads(blob)
    except json.JSONDecodeError as e:
        return None, blob[:200], f"bad JSON: {e}"
    score = parsed.get("score")
    why = (parsed.get("why") or "").strip()
    if not isinstance(score, (int, float)) or isinstance(score, bool) or not 1 <= score <= 5:
        return None, why[:200], f"score out of range: {score!r}"
    return int(score), why[:200], None


def judge_all(
    rows: List[JudgeRow],
    api_key: str,
    model: str,
    concurrency: int,
) -> None:
    """Score every row in-place. Threaded because the bottleneck is
    network, not compute, and stdlib's urllib is sync. We don't need
    asyncio for the volumes Tier 2 is designed for (≲ a few hundred
    prompts per run)."""
    def _one(idx: int) -> None:
        score, why, err = call_judge(api_key, model, rows[idx].prompt_text)
        rows[idx].difficulty = score
        rows[idx].why = why
        rows[idx].error = err

    with ThreadPoolExecutor(max_workers=concurrency) as pool:
        futs = [pool.submit(_one, i) for i in range(len(rows))]
        done = 0
        for fut in as_completed(futs):
            fut.result()  # propagate unexpected errors
            done += 1
            if done % 25 == 0 or done == len(rows):
                print(f"  judged {done}/{len(rows)}", file=sys.stderr)


def attach_picks(
    rows: List[JudgeRow],
    versions: List[str],
    assets_dir: Path,
    batch_size: int,
) -> None:
    """For each requested artifact version, embed every prompt once
    and record the picked model on each ``rows[i].picks[version]``.
    Embeddings are computed once and reused across versions because
    the embedder is shared; only centroids/rankings differ per
    version."""
    # Disk cache keyed by (prompts, model.onnx mtime). Re-running the
    # same JSONL against a new artifact version skips embedding entirely.
    # See holdout_eval.py for the same pattern.
    cache_dir = Path(__file__).resolve().parent / ".embedding-cache"
    cache_dir.mkdir(exist_ok=True)
    cache_key = hashlib.sha256()
    for r in rows:
        cache_key.update(r.prompt_text.encode("utf-8"))
        cache_key.update(b"\0")
    for asset_name in ("model.onnx", "tokenizer.json"):
        asset_path = assets_dir / asset_name
        if asset_path.exists():
            stat = asset_path.stat()
            cache_key.update(
                f"{asset_name}:{stat.st_size}-{int(stat.st_mtime)}".encode()
            )
    cache_path = cache_dir / f"{cache_key.hexdigest()[:16]}.npy"

    if cache_path.exists():
        embed_start = time.monotonic()
        embeddings = np.load(cache_path)
        embed_ms = (time.monotonic() - embed_start) * 1000
        print(f"Embedding cache hit: {cache_path.name} "
              f"({embeddings.shape[0]} × {embeddings.shape[1]} in {embed_ms:.0f}ms)",
              file=sys.stderr)
    else:
        print(f"Loading embedder from {assets_dir} (cache miss; will write {cache_path.name}) ...",
              file=sys.stderr)
        # CPU-only — see holdout_eval.py for the rationale.
        sess, tok, input_names, output_name = load_embedder(
            assets_dir, use_coreml=False, dynamic_padding=True
        )
        print(f"Embedding {len(rows)} prompts (batch_size={batch_size}) ...", file=sys.stderr)
        chunks = []
        n_batches = (len(rows) + batch_size - 1) // batch_size
        embed_start = time.monotonic()
        for i in range(0, len(rows), batch_size):
            batch = [r.prompt_text for r in rows[i : i + batch_size]]
            chunks.append(embed_batch(sess, tok, input_names, output_name, batch))
            b = i // batch_size + 1
            if b == 1 or b % 10 == 0 or b == n_batches:
                print(f"  batch {b}/{n_batches} done", file=sys.stderr, flush=True)
        embed_ms = (time.monotonic() - embed_start) * 1000
        embeddings = np.vstack(chunks) if chunks else np.empty((0, EMBED_DIM), dtype=np.float32)
        per_prompt = embed_ms / max(1, len(rows))
        np.save(cache_path, embeddings)
        print(f"  embedded {len(rows)} prompts in {embed_ms:.0f}ms "
              f"({per_prompt:.1f}ms/prompt, {1000/per_prompt:.0f}/s); cached to "
              f"{cache_path.name}", file=sys.stderr)
    for v in versions:
        centroids, rankings, entries, _ = load_artifact_bundle(v)
        candidate_models = sorted({e["model"] for e in entries})
        picks = simulate_cluster_route(embeddings, centroids, rankings, candidate_models, top_p=4)
        for r, m in zip(rows, picks):
            r.picks[v] = m


def render_summary(rows: List[JudgeRow], versions: List[str]) -> str:
    """Per-version under-powered / wasted breakdown plus difficulty
    distribution. Under-powered (tier<expected) is the failure mode
    we care about most for a routing regression — a frontier model on
    a trivial prompt is annoying but not broken.
    """
    valid = [r for r in rows if r.difficulty is not None]
    if not valid:
        return "(no rows survived judging — see errors above)"

    out: List[str] = []
    out.append(f"Difficulty distribution (n={len(valid)} judged, {len(rows)-len(valid)} errors):")
    dist = Counter(r.difficulty for r in valid)
    for d in (1, 2, 3, 4, 5):
        bar = "█" * dist.get(d, 0) if len(valid) <= 80 else "█" * (dist.get(d, 0) * 50 // max(dist.values() or [1]))
        out.append(f"  {d}: {dist.get(d, 0):4d}  {bar}")

    if not versions:
        return "\n".join(out)

    out.append("")
    out.append("Tier-fit per cluster version:")
    out.append(f"  {'version':<14}  {'underpowered':>14}  {'wasted':>10}  {'on-tier':>9}  {'avg_picked_tier':>16}")
    out.append(f"  {'':<14}  {'(hard→cheap)':>14}  {'(easy→top)':>10}  {'':>9}  {'':>16}")
    for v in versions:
        under = wasted = on_tier = 0
        tier_sum = 0
        n_with_pick = 0
        for r in valid:
            m = r.picks.get(v)
            if not m:
                continue
            n_with_pick += 1
            picked_tier = model_tier(m)
            expected = expected_tier(r.difficulty)
            tier_sum += picked_tier
            if picked_tier < expected:
                under += 1
            elif r.difficulty <= 2 and picked_tier == 3:
                wasted += 1
            else:
                on_tier += 1
        if n_with_pick == 0:
            continue
        avg_tier = tier_sum / n_with_pick
        out.append(
            f"  {v+'-cluster':<14}  "
            f"{under:>4}/{n_with_pick:<4} ({100*under//n_with_pick:>2}%) "
            f"{wasted:>3}/{n_with_pick:<3} ({100*wasted//n_with_pick:>2}%) "
            f"{on_tier:>3}/{n_with_pick:<3} ({100*on_tier//n_with_pick:>2}%) "
            f"{avg_tier:>14.2f}"
        )
    return "\n".join(out)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--prompts", type=Path, default=None,
                        help="JSONL with one object per line; reads .prompt_text or .prompt. "
                             "Mutually exclusive with --bench-holdout.")
    parser.add_argument("--bench-holdout", action="store_true",
                        help="Pull prompts from the bench cache's deterministic 80/20 holdout — "
                             "the same set holdout_eval.py evaluates regret on. Lets you "
                             "cross-reference Tier 2 difficulty with Tier 1 per-prompt regret.")
    parser.add_argument("--bench-cache", type=Path,
                        default=Path(__file__).resolve().parent / ".bench-cache",
                        help="Path to bench cache (only used with --bench-holdout).")
    parser.add_argument("--holdout-frac", type=float, default=0.2)
    parser.add_argument("--seed", type=int, default=42)
    parser.add_argument("--version", action="append", dest="versions", default=None,
                        help="Cluster version to simulate against (repeatable; e.g. --version v0.4 --version v0.5). Omit to skip routing simulation and just report difficulty.")
    parser.add_argument("--judge-model", default="claude-haiku-4-5",
                        help="Anthropic model used for difficulty judging (cheap recommended).")
    parser.add_argument("--concurrency", type=int, default=8)
    parser.add_argument("--limit", type=int, default=0,
                        help="If >0, judge only the first N prompts (smoke).")
    parser.add_argument("--out", type=Path, default=None,
                        help="Optional JSONL output path. One row per prompt with difficulty + per-version picks.")
    parser.add_argument("--batch-size", type=int, default=32)
    parser.add_argument("--assets", type=Path, default=ASSETS_DIR)
    args = parser.parse_args()

    api_key = os.environ.get("ANTHROPIC_API_KEY")
    if not api_key:
        sys.exit("ANTHROPIC_API_KEY missing in environment")

    if args.bench_holdout and args.prompts:
        sys.exit("--bench-holdout and --prompts are mutually exclusive.")
    if not args.bench_holdout and not args.prompts:
        sys.exit("provide --prompts <jsonl> OR --bench-holdout")

    if args.bench_holdout:
        print(f"Loading bench from {args.bench_cache} ...", file=sys.stderr)
        rows = load_bench_holdout(args.bench_cache, args.holdout_frac, args.seed)
        source = f"bench-holdout(frac={args.holdout_frac}, seed={args.seed})"
    else:
        rows = load_prompts(args.prompts)
        source = str(args.prompts)
    if args.limit > 0:
        rows = rows[: args.limit]
    print(f"Loaded {len(rows)} prompts from {source}", file=sys.stderr)

    if args.versions:
        attach_picks(rows, args.versions, args.assets, args.batch_size)

    print(f"Judging difficulty (model={args.judge_model}, concurrency={args.concurrency}) ...", file=sys.stderr)
    judge_all(rows, api_key, args.judge_model, args.concurrency)

    n_err = sum(1 for r in rows if r.error)
    if n_err:
        # Surface the first few errors so a misconfigured key / model
        # name fails loud rather than producing an empty summary.
        print(f"WARN: {n_err}/{len(rows)} judging errors:", file=sys.stderr)
        for r in [row for row in rows if row.error][:5]:
            print(f"  {r.prompt_id}: {r.error}", file=sys.stderr)

    if args.out:
        with args.out.open("w") as f:
            for r in rows:
                f.write(json.dumps({
                    "prompt_id": r.prompt_id,
                    "prompt_preview": r.prompt_text[:200],
                    "difficulty": r.difficulty,
                    "why": r.why,
                    "error": r.error,
                    "picks": r.picks,
                }) + "\n")
        print(f"Wrote {args.out}", file=sys.stderr)

    print()
    print(render_summary(rows, args.versions or []))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
