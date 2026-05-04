"""Auto-load router/eval/.env into os.environ.

Real environment variables (e.g. set by Modal secrets) take precedence —
load_dotenv defaults to override=False. Mirrors router/scripts/_env.py.
"""

from pathlib import Path

from dotenv import load_dotenv

load_dotenv(dotenv_path=Path(__file__).resolve().parent / ".env")
