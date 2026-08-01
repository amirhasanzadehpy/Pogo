#!/usr/bin/env python3
"""Run Pogo's repeatable benchmark matrix and enforce release gates."""

from __future__ import annotations

import argparse
import json
import os
import platform
import re
import subprocess
import sys
from pathlib import Path


BENCHMARK_LINE = re.compile(r"^(Benchmark\S+)\s+(\d+)\s+(.+)$")
METRIC = re.compile(r"([0-9]+(?:\.[0-9]+)?)\s+(\S+)")
MIB = 1024 * 1024


def run(command: list[str], root: Path, output: Path, *, stdout_json: bool = False) -> str:
    completed = subprocess.run(command, cwd=root, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    output.write_text(completed.stdout, encoding="utf-8")
    if completed.returncode != 0:
        raise RuntimeError(f"command failed ({completed.returncode}): {' '.join(command)}\n{completed.stdout}")
    if stdout_json:
        json.loads(completed.stdout)
    return completed.stdout


def go_test_flags() -> list[str]:
    flags = ["-tags=grammar_subset,grammar_subset_python"]
    if platform.system() == "Darwin":
        flags.append("-ldflags=-linkmode=external")
    return flags


def parse_benchmarks(output: str) -> list[dict[str, object]]:
    benchmarks: list[dict[str, object]] = []
    for line in output.splitlines():
        match = BENCHMARK_LINE.match(line)
        if not match:
            continue
        metrics = {unit: float(value) for value, unit in METRIC.findall(match.group(3))}
        benchmarks.append({"name": match.group(1), "iterations": int(match.group(2)), "metrics": metrics})
    return benchmarks


def maximum(values: list[int]) -> int:
    return max(values, default=0)


def percentile(values: list[int], percent: int) -> int:
    if not values:
        return 0
    ordered = sorted(values)
    index = (len(ordered) * percent + 99) // 100 - 1
    return ordered[max(index, 0)]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", default="benchmark-results", help="profile output directory")
    parser.add_argument("--skip-profiles", action="store_true", help="skip CPU and heap pprof capture")
    args = parser.parse_args()

    root = Path(__file__).resolve().parent.parent
    output = (root / args.output).resolve()
    raw = output / "raw"
    pprof = output / "pprof"
    raw.mkdir(parents=True, exist_ok=True)
    pprof.mkdir(parents=True, exist_ok=True)

    flags = go_test_flags()
    commands = [
        ("lsp", ["go", "test", *flags, "-run", "^$", "-bench", "Benchmark(ParseUpdate|CompletionHandler|CompletionLatency|CompletionScale|HoverHandler|HoverScale|DiagnosticHandler|DiagnosticLatency|DiagnosticScale|DefinitionHandler)$", "-benchmem", "-benchtime=200x", "./internal/lsp"]),
        ("analysis", ["go", "test", *flags, "-run", "^$", "-bench", "Benchmark(ParseUpdateMatrix|DocumentSnapshots)$", "-benchmem", "-benchtime=10x", "./internal/analysis"]),
        ("schema-build", ["go", "test", *flags, "-run", "^$", "-bench", "BenchmarkGraphBuild$", "-benchmem", "-benchtime=1x", "./internal/schema"]),
        ("schema-lookup", ["go", "test", *flags, "-run", "^$", "-bench", "BenchmarkGraphLookup$", "-benchmem", "-benchtime=1000x", "./internal/schema"]),
        ("schema-cache", ["go", "test", *flags, "-run", "^$", "-bench", "BenchmarkCacheSnapshots$", "-benchmem", "-benchtime=1000x", "./internal/schema"]),
        ("refresh", ["go", "test", *flags, "-run", "^$", "-bench", "BenchmarkManagerRefresh$", "-benchmem", "-benchtime=3x", "./internal/python"]),
    ]

    all_benchmarks: list[dict[str, object]] = []
    command_records: list[dict[str, object]] = []
    for name, command in commands:
        text = run(command, root, raw / f"{name}.txt")
        parsed = parse_benchmarks(text)
        if not parsed:
            raise RuntimeError(f"{name} emitted no benchmark records")
        all_benchmarks.extend(parsed)
        command_records.append({"name": name, "command": command})

    fixture_python = root / ".venv-fixture" / ("Scripts/python.exe" if os.name == "nt" else "bin/python")
    if not fixture_python.exists():
        raise RuntimeError("fixture environment missing; run `make fixture-env` first")
    subprocess.run(
        [str(fixture_python), "src/daemon/introspect.py", "--project", "testdata/sample_django_project", "--settings", "sample_project.settings"],
        cwd=root,
        stdout=subprocess.DEVNULL,
        check=True,
    )
    rss_command = [
        "build/testclient", "-format", "json",
        "-scenario", "testdata/requests/worker-lifecycle.json", "--", "build/pogo",
        "-project", "testdata/sample_django_project", "-settings", "sample_project.settings", "-python", str(fixture_python),
    ]
    rss_text = run(rss_command, root, raw / "rss.json", stdout_json=True)
    rss = json.loads(rss_text)["result"]

    completion = next((entry for entry in all_benchmarks if str(entry["name"]).startswith("BenchmarkCompletionLatency-")), None)
    if completion is None or "p95-us" not in completion["metrics"]:
        raise RuntimeError("completion p95 metric is missing")
    completion_p95_us = float(completion["metrics"]["p95-us"])
    go_max = int(rss.get("go_rss_bytes", 0))
    worker_max = int(rss.get("worker_rss_bytes", 0))
    combined_samples = [int(value) for value in rss.get("combined_rss_samples", [])]
    combined_max = maximum(combined_samples)
    enforce_rss = platform.system() in {"Darwin", "Linux"}
    gates = [
        {"name": "completion_p95", "actual_us": completion_p95_us, "limit_us": 10_000, "passed": completion_p95_us < 10_000},
        {"name": "go_rss", "actual_bytes": go_max, "limit_bytes": 50 * MIB, "enforced": enforce_rss, "passed": not enforce_rss or 0 < go_max <= 50 * MIB},
        {"name": "combined_rss", "actual_bytes": combined_max, "limit_bytes": 150 * MIB, "enforced": enforce_rss, "passed": not enforce_rss or 0 < combined_max <= 150 * MIB},
    ]

    profiles: list[dict[str, object]] = []
    if not args.skip_profiles:
        profile_command = [
            "go", "test", *flags, "-run", "^$", "-bench", "BenchmarkCompletionLatency$", "-benchtime=1000x",
            f"-cpuprofile={pprof / 'completion.cpu.prof'}", f"-memprofile={pprof / 'completion.heap.prof'}", "./internal/lsp",
        ]
        run(profile_command, root, raw / "profile-run.txt")
        for kind, profile in (("cpu", pprof / "completion.cpu.prof"), ("heap", pprof / "completion.heap.prof")):
            text_path = pprof / f"completion.{kind}.txt"
            run(["go", "tool", "pprof", "-top", str(profile)], root, text_path)
            profiles.append({"kind": kind, "path": str(profile.relative_to(output)), "text": str(text_path.relative_to(output))})

    report = {
        "schema_version": 1,
        "environment": {
            "commit": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=root, text=True).strip(),
            "go_version": subprocess.check_output(["go", "version"], text=True).strip(),
            "platform": platform.platform(),
            "machine": platform.machine(),
            "python": platform.python_version(),
            "logical_cpus": os.cpu_count(),
        },
        "commands": command_records,
        "benchmarks": all_benchmarks,
        "rss": {
            "go_max_bytes": go_max,
            "worker_max_bytes": worker_max,
            "combined_p50_bytes": percentile(combined_samples, 50),
            "combined_p95_bytes": percentile(combined_samples, 95),
            "combined_p99_bytes": percentile(combined_samples, 99),
            "combined_max_bytes": combined_max,
            "sample_count": len(combined_samples),
        },
        "profiles": profiles,
        "gates": gates,
        "passed": all(bool(gate["passed"]) for gate in gates),
    }
    (output / "profile.json").write_text(json.dumps(report, indent=2, sort_keys=True) + "\n", encoding="utf-8")

    def display_percentile(metrics: dict[str, float], percent: int) -> str:
        if f"p{percent}-us" in metrics:
            return f"{metrics[f'p{percent}-us']:.2f} us"
        if f"p{percent}-ms" in metrics:
            return f"{metrics[f'p{percent}-ms']:.2f} ms"
        return "N/A"

    lines = [
        "Pogo Performance Profile",
        f"Platform: {report['environment']['platform']} ({report['environment']['machine']})",
        f"Go: {report['environment']['go_version']}",
        "",
        f"{'Benchmark':64} {'ns/op':>12} {'B/op':>12} {'allocs/op':>10} {'p50':>12} {'p95':>12} {'p99':>12}",
    ]
    for entry in all_benchmarks:
        metrics = entry["metrics"]
        lines.append(
            f"{str(entry['name'])[:64]:64} {metrics.get('ns/op', 0):12.1f} {metrics.get('B/op', 0):12.1f} "
            f"{metrics.get('allocs/op', 0):10.1f} {display_percentile(metrics, 50):>12} {display_percentile(metrics, 95):>12} {display_percentile(metrics, 99):>12}"
        )
    lines.extend([
        "",
        f"Go RSS max: {go_max / MIB:.2f} MiB",
        f"Worker RSS max: {worker_max / MIB:.2f} MiB",
        f"Combined RSS p50/p95/p99/max: {percentile(combined_samples, 50) / MIB:.2f}/{percentile(combined_samples, 95) / MIB:.2f}/{percentile(combined_samples, 99) / MIB:.2f}/{combined_max / MIB:.2f} MiB",
        "",
    ])
    for gate in gates:
        lines.append(f"Gate {gate['name']}: {'PASS' if gate['passed'] else 'FAIL'}")
    (output / "profile.txt").write_text("\n".join(lines) + "\n", encoding="utf-8")
    print("\n".join(lines))
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, RuntimeError, subprocess.CalledProcessError) as error:
        print(f"benchmark failed: {error}", file=sys.stderr)
        raise SystemExit(1)
