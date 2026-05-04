"""Pytest config: add the package root to sys.path so `import eval` works.

The harness lives as a flat package under router/eval/; it isn't pip-
installed (package-mode = false). Tests need to import the modules
under test, so we prepend router/eval/'s parent to sys.path.
"""

import sys
from pathlib import Path

PACKAGE_ROOT = Path(__file__).resolve().parents[2]  # router/
if str(PACKAGE_ROOT) not in sys.path:
    sys.path.insert(0, str(PACKAGE_ROOT))
