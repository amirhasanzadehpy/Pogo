#!/usr/bin/env python3
"""Inspect release binaries and the embedded worker dependency boundary."""

from __future__ import annotations

import ast
import subprocess
import sys
from pathlib import Path


FORBIDDEN_MARKERS = (b"testdata", b"sample_django_project", b".venv-fixture", b"Django==")


def check_worker(root: Path) -> None:
    source = root / "src" / "daemon" / "introspect.py"
    tree = ast.parse(source.read_bytes(), filename=str(source))
    imports: set[str] = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            imports.update(alias.name.split(".", 1)[0] for alias in node.names)
        elif isinstance(node, ast.ImportFrom) and node.module:
            imports.add(node.module.split(".", 1)[0])
    allowed = set(sys.stdlib_module_names) | {"__future__", "django"}
    unexpected = sorted(imports - allowed)
    if unexpected:
        raise RuntimeError(f"unexpected worker imports: {unexpected}")


def check_binary(path: Path) -> None:
    data = path.read_bytes()
    markers = [marker.decode() for marker in FORBIDDEN_MARKERS if marker in data]
    if markers:
        raise RuntimeError(f"{path}: forbidden fixture markers: {markers}")
    subprocess.run(["go", "version", "-m", str(path)], check=True)


def main() -> int:
    root = Path(__file__).resolve().parent.parent
    binaries = [Path(argument).resolve() for argument in sys.argv[1:]]
    if not binaries:
        raise RuntimeError("usage: check_release.py BINARY [BINARY...]")
    check_worker(root)
    dependencies = subprocess.check_output(
        ["go", "list", "-deps", "-tags=grammar_subset,grammar_subset_python", "./cmd/pogo"],
        cwd=root,
        text=True,
    ).splitlines()
    forbidden = [dependency for dependency in dependencies if "testdata" in dependency or dependency.endswith("/internal/harness")]
    if forbidden:
        raise RuntimeError(f"release dependency graph contains test packages: {forbidden}")
    for binary in binaries:
        check_binary(binary)
    print("PASS release dependency and artifact inspection")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, RuntimeError, subprocess.CalledProcessError) as error:
        print(f"release inspection failed: {error}", file=sys.stderr)
        raise SystemExit(1)
