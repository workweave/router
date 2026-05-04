#!/usr/bin/env bash
#
# One-shot fetch of the OpenRouterBench dataset (NPULH/OpenRouterBench
# on HuggingFace), the same evaluation data that produced the
# AvengersPro paper's per-(prompt, model) scoring matrix.
#
# Output: scripts/.bench-cache/  (gitignored)
#
# Usage:
#   bash scripts/download_bench.sh
#
# Requires `huggingface-cli` installed (pip install --user huggingface_hub).
# A logged-in HF account isn't required for public datasets — but the
# router team's machines typically have one configured for rate-limit
# headroom.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CACHE_DIR="$SCRIPT_DIR/.bench-cache"

REPO_ID="${REPO_ID:-NPULH/OpenRouterBench}"
REVISION="${REVISION:-main}"

mkdir -p "$CACHE_DIR"

echo "Fetching $REPO_ID@$REVISION into $CACHE_DIR ..."

# `--repo-type dataset` is load-bearing; without it huggingface-cli
# defaults to model-repo lookup and 404s.
#
# `poetry run` activates scripts/.venv (where huggingface-cli is
# installed) without needing an active poetry shell first. Run this
# script from any cwd; SCRIPT_DIR pins poetry to the right project.
poetry -C "$SCRIPT_DIR" run huggingface-cli download \
    "$REPO_ID" \
    --revision "$REVISION" \
    --repo-type dataset \
    --local-dir "$CACHE_DIR"

# OpenRouterBench ships as a single ~1.3 GB bench-release.tar.gz on
# the HF dataset. After extraction we get
#     bench-release/<benchmark>/<model>/<benchmark>-<model>-<ts>.json
# which is the layout the inspect / sweep / train walkers expect.
TARBALL="$CACHE_DIR/bench-release.tar.gz"
if [ -f "$TARBALL" ]; then
    # Always re-extract so a refreshed tarball overwrites stale rows;
    # otherwise rerunning this script after an upstream bump silently
    # keeps the previous bench-release/ contents.
    echo "Extracting $TARBALL ..."
    rm -rf "$CACHE_DIR/bench-release"
    tar -xzf "$TARBALL" -C "$CACHE_DIR"
fi

echo
echo "Done. Run 'poetry run python inspect_bench.py' to summarize."
