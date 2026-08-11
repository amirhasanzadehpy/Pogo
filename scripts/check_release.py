#!/usr/bin/env python3
"""Inspect release binaries and the embedded worker dependency boundary."""

from __future__ import annotations

import argparse
import ast
import json
import subprocess
import sys
import zipfile
from pathlib import Path


FORBIDDEN_MARKERS = (b"testdata", b"sample_django_project", b".venv-fixture", b"Django==")
TARGETS = (
    ("linux-amd64", "pogo"),
    ("linux-arm64", "pogo"),
    ("darwin-amd64", "pogo"),
    ("darwin-arm64", "pogo"),
    ("windows-amd64", "pogo.exe"),
    ("windows-arm64", "pogo.exe"),
)


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
    subprocess.run(
        ["go", "version", "-m", str(path)],
        check=True,
        capture_output=True,
        text=True,
    )


def check_vsix(root: Path, path: Path, binaries: list[Path]) -> None:
    expected_entries = {
        f"extension/bin/{target}/{name}" for target, name in TARGETS
    }
    by_target = {binary.parent.name: binary for binary in binaries}
    missing_targets = sorted(target for target, _ in TARGETS if target not in by_target)
    if missing_targets:
        raise RuntimeError(f"VSIX inspection missing staged binaries: {missing_targets}")

    with zipfile.ZipFile(path) as archive:
        binary_entries = {
            name for name in archive.namelist() if name.startswith("extension/bin/")
        }
        if binary_entries != expected_entries:
            raise RuntimeError(
                f"{path}: bundled binary entries {sorted(binary_entries)}, "
                f"expected {sorted(expected_entries)}"
            )
        for target, name in TARGETS:
            entry = f"extension/bin/{target}/{name}"
            if archive.read(entry) != by_target[target].read_bytes():
                raise RuntimeError(f"{path}: {entry} differs from staged binary")
            mode = archive.getinfo(entry).external_attr >> 16
            if not target.startswith("windows-") and mode & 0o111 == 0:
                raise RuntimeError(f"{path}: {entry} is not executable")

        packaged = json.loads(archive.read("extension/package.json"))
        expected = json.loads((root / "client" / "vscode" / "package.json").read_bytes())
        if packaged.get("version") != expected.get("version"):
            raise RuntimeError(
                f"{path}: extension version {packaged.get('version')!r} does not "
                f"match source {expected.get('version')!r}"
            )


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--vsix", type=Path)
    parser.add_argument("binaries", nargs="+", type=Path)
    arguments = parser.parse_args()
    root = Path(__file__).resolve().parent.parent
    binaries = [path.resolve() for path in arguments.binaries]
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
    if arguments.vsix is not None:
        check_vsix(root, arguments.vsix.resolve(), binaries)
    print("PASS release dependency and artifact inspection")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, RuntimeError, subprocess.CalledProcessError) as error:
        print(f"release inspection failed: {error}", file=sys.stderr)
        raise SystemExit(1)
