"""Pull the cluster-scorer embedder artifacts from HF Hub into assets/.

We point directly at Jina's official repo
(`jinaai/jina-embeddings-v2-base-code`) which already ships an
INT8-quantized ONNX at `onnx/model_quantized.onnx` (162 MB,
maintained by the model authors). Self-hosters and CI build with no
token — the repo is public.

The router cluster scorer loads model.onnx + tokenizer.json from
disk at boot. In production the Dockerfile fetches them during
build; locally, contributors run this script once after
`poetry install` to populate `internal/router/cluster/assets/`.
The integration test under `-tags=onnx_integration` expects the
files to be there.

Pulls REQUIRED_FILES (always) and OPTIONAL_FILES (best-effort) from
hf_files.py.

Usage:
    cd router/scripts
    poetry run python download_from_hf.py                    # default revision
    poetry run python download_from_hf.py --revision <sha>   # pin
    poetry run python download_from_hf.py --force            # re-download

Auth:
    Not required (public repo). Optional HF_TOKEN raises rate limits.
"""

from __future__ import annotations

import argparse
import os
import shutil
import sys
from pathlib import Path

import _env  # noqa: F401  # auto-load .env into os.environ
from hf_files import OPTIONAL_FILES, REQUIRED_FILES
from huggingface_hub import hf_hub_download
from huggingface_hub.utils import EntryNotFoundError

ROUTER_ROOT = Path(__file__).resolve().parents[1]
ASSETS_DIR = ROUTER_ROOT / "internal/router/cluster/assets"
DEFAULT_REPO = "jinaai/jina-embeddings-v2-base-code"
# Pin to the SHA where Jina last touched the onnx/ folder. The repo
# hasn't changed weights since Apr 2024; pinning eliminates silent
# upstream drift. Bump deliberately if Jina ever ships a new export.
DEFAULT_REVISION = "516f4baf13dec4ddddda8631e019b5737c8bc250"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", default=DEFAULT_REPO)
    parser.add_argument(
        "--revision",
        default=DEFAULT_REVISION,
        help="HF revision (branch/tag/commit). Default pins to a known-good SHA.",
    )
    parser.add_argument("--force", action="store_true")
    args = parser.parse_args()

    ASSETS_DIR.mkdir(parents=True, exist_ok=True)
    token = os.environ.get("HF_TOKEN")  # optional; only raises rate limits

    def fetch(local_name: str, hf_path: str, optional: bool) -> None:
        dst = ASSETS_DIR / local_name
        if dst.exists() and not args.force:
            size_mb = dst.stat().st_size / 1024 / 1024
            print(f"{dst} already present ({size_mb:.1f} MB). Use --force to refresh.")
            return
        print(f"Fetching {hf_path} from {args.repo}@{args.revision[:8]}")
        try:
            cached = hf_hub_download(
                repo_id=args.repo,
                filename=hf_path,
                revision=args.revision,
                token=token,
            )
        except EntryNotFoundError:
            if optional:
                print(f"  (not on HF, skipping: {hf_path})")
                return
            raise
        # hf_hub_download returns a path inside ~/.cache/huggingface/.
        # Copy (not symlink) so the Go embedder's stat-and-read sees a
        # stable path independent of the HF cache layout.
        shutil.copy2(cached, dst)
        size_mb = dst.stat().st_size / 1024 / 1024
        print(f"Wrote {dst} ({size_mb:.1f} MB)")

    for local_name, hf_path in REQUIRED_FILES:
        fetch(local_name, hf_path, optional=False)
    for local_name, hf_path in OPTIONAL_FILES:
        fetch(local_name, hf_path, optional=True)

    model_size = (ASSETS_DIR / "model.onnx").stat().st_size
    if model_size < 1024 * 1024:
        sys.exit(
            "model.onnx is suspiciously small (<1 MB) — HF download likely failed."
        )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
