#!/usr/bin/env python3
"""Run worker and fixture tests against both supported Django lines."""

from __future__ import annotations

import os
import subprocess
import sys
import tempfile
import venv
from pathlib import Path


def run(command: list[str], root: Path, environment: dict[str, str]) -> None:
    print("$", " ".join(command), flush=True)
    subprocess.run(command, cwd=root, env=environment, check=True)


def main() -> int:
    root = Path(__file__).resolve().parent.parent
    fixture = root / "testdata" / "sample_django_project"
    environment = os.environ.copy()
    environment["PYTHONDONTWRITEBYTECODE"] = "1"
    for line in ("django42", "django52"):
        with tempfile.TemporaryDirectory(prefix=f"pogo-{line}-") as temporary:
            virtualenv = Path(temporary) / "venv"
            venv.EnvBuilder(with_pip=True).create(virtualenv)
            python = virtualenv / ("Scripts/python.exe" if os.name == "nt" else "bin/python")
            run([
                str(python), "-m", "pip", "install", "-r", str(fixture / "requirements.txt"),
                "-c", str(fixture / f"constraints-{line}.txt"),
            ], root, environment)
            run([str(python), "-c", "import platform, django; print('Python', platform.python_version(), 'Django', django.get_version())"], root, environment)
            run([str(python), "-m", "unittest", "discover", "-s", str(fixture / "tests"), "-p", "test_*.py", "-v"], root, environment)
            run([str(python), "-m", "unittest", "discover", "-s", "src/daemon", "-p", "test_*.py", "-v"], root, environment)
            run([str(python), "src/daemon/introspect.py", "--project", str(fixture), "--settings", "sample_project.settings"], root, environment)
            run([str(python), "-m", "pip", "check"], root, environment)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except subprocess.CalledProcessError as error:
        raise SystemExit(error.returncode)
