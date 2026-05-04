"""Auto-load router/scripts/.env into os.environ.

Real environment variables (e.g. HF_TOKEN injected by CI) take
precedence — load_dotenv defaults to override=False.
"""

from pathlib import Path

from dotenv import load_dotenv

load_dotenv(dotenv_path=Path(__file__).resolve().parent / ".env")
