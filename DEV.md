# Development

## Requirements

- Go 1.22 or newer.
- Python 3.10 through 3.13.
- GNU Make 3.81 or newer.

The Django fixture uses an isolated virtual environment at `.venv-fixture`. Do
not install its dependencies into the system Python environment.

## Fixture Setup

Create or refresh the fixture environment and run the complete suite:

```sh
make test-env
```

The environment is reused until `requirements.txt` or `constraints.txt`
changes. Run `make clean` before selecting a different `PYTHON` interpreter.
To prepare the environment without running tests, use `make fixture-env`.

The fixture pins Django 5.2.16. Its normal SQLite database path is
`testdata/sample_django_project/db.sqlite3`, but automated migration tests set
`POGO_FIXTURE_DB` to a temporary database and never mutate that path.

## Commands

```sh
make build
make test
make test-race
make bench
make clean
```

`make test` is non-installing and requires `make fixture-env` to have already
completed. It checks Go formatting, runs `go vet`, runs Go tests, and runs both
the fixture and daemon standard-library Python suites. `make test-race` runs the
Go suite with race detection. `make bench` measures parse-update, completion,
diagnostic and definition handlers; end-to-end p50/p95; schema
refresh; graph lookup; and server/worker RSS.

`make fuzz` runs 30-second campaigns for worker environment parsing, worker and
LSP framing, UTF-16 position/edit conversion, ORM path extraction, and parser recovery. `make
compat` creates isolated temporary environments and runs the fixture and worker
suites against the pinned Django 4.2 and 5.2 profiles for the current Python.

`make build` writes `build/pogo` and `build/testclient`. Run a traced
lifecycle scenario with logs isolated from protocol stdout:

```sh
build/testclient \
  -scenario testdata/requests/normal-shutdown.json \
  -trace-methods \
  -- build/pogo -log-file build/protocol.log
```

Dump the fixture's runtime ORM schema as deterministic compact JSON:

```sh
.venv-fixture/bin/python src/daemon/introspect.py \
  --project testdata/sample_django_project
```

Pass `--pretty` for indented output or `--settings sample_project.settings` to
select the settings module explicitly. Introspection starts Django and executes
trusted project imports; its JSON stdout must not be mixed with diagnostic
output.

Run the language server with the authenticated schema worker enabled:

```sh
build/pogo \
  -project testdata/sample_django_project \
  -settings sample_project.settings \
  -python .venv-fixture/bin/python \
  -log-file build/protocol.log
```

`-worker-env-file PATH` is the CLI equivalent of
`djangoOrm.environmentFile`. Relative paths resolve against `-project`; an
explicit absolute path may be outside the project. For an environment-dependent
project, add `-worker-env-file .env.pogo` to the command after creating that
ignored file under the project root. There is deliberately no `KEY=value` CLI
flag because process listings can expose argument values.

Worker configuration sources are:

| Value | LSP initialization | CLI |
| --- | --- | --- |
| Project root | `djangoOrm.projectRoot` | `-project PATH` |
| Python interpreter | `djangoOrm.pythonPath` | `-python PATH` |
| Settings module | `djangoOrm.settingsModule` | `-settings MODULE` |
| Environment file | `djangoOrm.environmentFile` | `-worker-env-file PATH` |
| Environment literals | `djangoOrm.environment` as `Record<string, string \| null>` | Not supported |

Resolution order is CLI then initialization options. If Python is still empty,
Pogo requires the project-local `.venv`; it never consults `VIRTUAL_ENV` or a
global `python3` on `PATH`. If settings are still empty, Pogo checks an explicit
worker `DJANGO_SETTINGS_MODULE`, a literal `manage.py` setting, then one
unambiguous immediate `*/settings.py`. Ambient `DJANGO_SETTINGS_MODULE` is not a
configuration source. Explicit settings that conflict with the worker
environment fail configuration, as do ambiguous settings.

The worker starts after the LSP `initialized` notification, communicates over a
private local endpoint using bounded protocol-v1 JSON frames, and is stopped and
reaped on shutdown, exit, or stdio EOF. Worker and project output is forwarded
to the language-server log and never to LSP stdout.

### Worker Environment

The coordinator may inherit its parent environment normally, but manager
construction creates a separate worker snapshot containing only the
coordinator's snapshotted `PATH` as a baseline. Precedence is environment file,
nullable LSP literals, then Pogo-owned runtime values. A string literal replaces the file
value and `null` removes it. Reserved transport, Python, temporary-directory,
and platform keys fail on collision instead of being silently replaced. Pogo
never synthesizes project configuration such as `SECRET_KEY`, database URLs,
settings, or fallback databases.

Environment files are explicit only; `.env` names are never auto-discovered.
The bounded parser supports UTF-8 dotenv-style blank lines, comments, optional
`export`, portable keys, empty/unquoted/single-quoted/double-quoted values, and
multiline quoted values. It performs no variable expansion, command
substitution, shell execution, or ambient lookup. Malformed input, duplicate or
invalid keys, invalid UTF-8/NUL, unsupported escapes, and limit violations fail
manager configuration without logging values. Limits are 256 KiB per file, 256
variables, 256 bytes per key, 16 KiB per value, and 30,000 total portable
environment units. The variable and total-unit limits apply to the complete
process environment, including Pogo-owned entries, so explicit configuration
must leave capacity for those fixed/runtime values.

Single quotes are literal. Double quotes accept only `\\`, `\"`, `\n`, `\r`,
and `\t` escapes. In unquoted values, `#` starts a comment only after
whitespace, so URL fragments remain literal.

The manager opens and parses the file once. Retries and schema refreshes reuse
the immutable snapshot, so an edit requires a server/client restart. On POSIX,
Pogo logs a warning if the selected file is group- or world-readable. Use an
ignored, permission-restricted `.env.pogo` for real values and track a
value-free `.env.pogo.example` that lists required keys. Literal environment
values cross LSP initialization and may appear in protocol traces, so they are
only suitable for nonsecrets.

The worker receives fixed Python startup values, a `PATH` containing the
interpreter directory followed by the coordinator's snapshotted search path,
private session temp paths, the required Windows platform value,
and private authenticated transport metadata. Ambient `HOME`, `USERPROFILE`,
`VIRTUAL_ENV`, `PYTHONPATH`, `PYTHONHOME`, locale, proxy, cloud, CI, database,
certificate, and linker/native-library variables are not inherited. An explicit
worker `PATH` replaces the default. Projects must explicitly opt in to any
required locale, proxy, certificate path, or native-library search path. The worker runs with the
project root as its current directory for manage.py-compatible relative paths.

Open Python documents are parsed incrementally in Go with only the Python
grammar embedded in release builds. Completion and hover resolve direct fields,
deep relations, type-specific lookups and transforms, projection strings,
related-loading paths, attached custom managers, and custom QuerySet chains.
Signature help and method hover use cached signatures and docstrings without
executing project methods. These hot handlers continue to use the last valid
cache when the worker is unavailable and never issue worker requests.

Definition navigation uses the same cache-only resolver for model imports,
fields, relations, managers, custom methods, individual ORM path segments, and
static relation-target and `related_name` strings. Inherited and reverse fields
retain their originating declaration, while lookup and transform suffixes target
the underlying field. Targets are exact ranges in absolute percent-encoded file
URIs; missing or stale source files are omitted without failing the request.

Completed static ORM paths are validated locally on open, change, and save.
Diagnostics use stable `django-orm.*` codes, exact UTF-16 segment ranges, and the
same context-aware resolver as completion and hover. Dynamic paths, unresolved
receivers, and parser-recovered calls are left unreported. Closing a document
clears its diagnostics.

Saving a reported schema source or a Python file below an installed app root
starts a 300 ms trailing-edge debounce. A burst causes one fresh Django worker
session and one atomic cache-generation swap after strict schema validation.
Failed refreshes retain the previous immutable graph; successful generations
revalidate every still-open document. Ordinary Python saves outside those roots
do not restart the worker.

Deep paths use Django's context-specific names: query names in filters and
projections, reverse accessors in `prefetch_related`, and only single-valued
relations in `select_related`. The iterative resolver is bounded by source
length and segment count rather than unique models, so recursive self-relations
remain valid.

To run Django commands directly, invoke the virtual environment interpreter
without activating it:

```sh
.venv-fixture/bin/python testdata/sample_django_project/manage.py check
```

## Trust Boundary

Django startup imports and executes code from the target project. Only run the
fixture tools and future language server against trusted workspaces. Local IPC
authentication prevents unrelated clients from connecting, but it is not a
sandbox for project code. Environment isolation prevents accidental ambient
application-variable forwarding other than `PATH`; it does not restrict filesystem access,
user-site packages, databases, subprocesses, or the network. The coordinator's
inherited editor/shell environment remains separate from the explicit worker
snapshot.

## Performance Profiles

`make bench` runs deterministic synthetic dimensions independently rather than
forming an impractical Cartesian product:

| Dimension | Cases |
|---|---|
| Schema size | 10, 1,000, and 10,000 models |
| Dense model | 256 additional fields |
| Recursive paths | Ring/self-cycle schema paths |
| Document size | 1 KiB and 100 KiB incremental updates |
| Open documents | 1, 100, and 1,000 snapshots |
| Cache concurrency | Parallel reads, swaps, and swaps under active readers |
| LSP handlers | Completion, hover, diagnostics, definition |

Results are written to:

```text
benchmark-results/
  profile.json
  profile.txt
  raw/*.txt
  raw/rss.json
  pprof/completion.cpu.prof
  pprof/completion.cpu.txt
  pprof/completion.heap.prof
  pprof/completion.heap.txt
```

`profile.json` records command lines, environment metadata, iterations,
`ns/op`, `B/op`, `allocs/op`, p50/p95/p99 where sampled, RSS summaries, sample
count, and gate results. The aligned RSS samples remain in `raw/rss.json`. A
missing required metric fails the harness. The fixed gates are completion p95
`<10 ms`, Go RSS `<=50 MiB`, and combined RSS `<=150 MiB`. Linux and macOS
failures are not waived for platform variance; Windows RSS is informational
because the standard-library sampler is unavailable there.

Inspect a captured profile directly:

```sh
go tool pprof -top benchmark-results/pprof/completion.cpu.prof
go tool pprof -top -alloc_space benchmark-results/pprof/completion.heap.prof
```

### Reference Baseline

Measured on 2026-07-31 from tracked source at commit `ca86c19`, Apple M1 arm64,
macOS 26.5.2, Go 1.22.12, Python 3.11.10, and Django 5.2.16. Values are real
warmed runs from `make bench`; host load and toolchain changes can affect
informational values but not the fixed Linux/macOS gates.

| Profile | p50 | p95 | p99 | `ns/op` | `B/op` | allocations |
|---|---:|---:|---:|---:|---:|---:|
| Parse plus completion | 45.75 us | 58.67 us | 67.29 us | 48,558 | 37,123 | 112 |
| Completion handler | N/A | N/A | N/A | 4,446 | 8,287 | 50 |
| Hover handler | 3.08 us | 3.50 us | 3.96 us | 3,250 | 2,318 | 35 |
| Diagnostics end-to-end | 12.08 us | 15.75 us | 20.79 us | 14,358 | 33,395 | 85 |
| Definition | 24.67 us | 28.33 us | 40.67 us | 25,506 | 4,575 | 81 |
| Dense 256-relation completion | 209.80 us | 496.80 us | 622.70 us | 233,817 | 311,649 | 4,382 |
| 10,000-model completion | 3.75 us | 4.75 us | 10.71 us | 4,138 | 6,558 | 41 |
| 100 KiB parse update | 11.72 ms | 15.02 ms | 15.02 ms | 12,516,625 | 9,266,335 | 2,104 |
| 1,000-document snapshot | 0.53 ms | 2.37 ms | 2.37 ms | 879,179 | 331,080 | 3,004 |
| Cache read | 0.04 us | 0.04 us | 0.04 us | 67.8 | 0 | 0 |
| Snapshot swap under readers | 0.12 us | 0.33 us | 0.33 us | 222.3 | 24 | 1 |

| Schema profile | Time | Bytes/op | Allocations/op |
|---|---:|---:|---:|
| 10 models | 726.12 us | 221,304 | 2,582 |
| 1,000 models | 72.09 ms | 22,232,088 | 256,182 |
| 10,000 models | 568.17 ms | 221,309,144 | 2,561,190 |
| Dense 256-field model | 492.04 us | 445,840 | 1,604 |

| Memory profile | Measured | Gate |
|---|---:|---:|
| Go RSS maximum | 19.67 MiB | `<=50 MiB` |
| Python worker RSS maximum | 47.88 MiB | Informational |
| Combined p50/p95/p99/maximum | 67.55 MiB | `<=150 MiB` |

Graph-build cases use one timed operation and are scaling indicators, not
statistically stable latency distributions. LSP interaction cases use 200
samples. The three-run fixture refresh measures Python introspection, IPC, and
atomic graph replacement separately from editor hot-path requests.

## Compatibility Matrix

The supported CI intersections are:

| Python | Django 4.2 | Django 5.2 |
|---|---:|---:|
| 3.10 | required | required |
| 3.11 | required | required |
| 3.12 | required | required |
| 3.13 | N/A: unsupported upstream | required |

Each compatibility job runs Go tests, fixture tests, worker tests,
introspection, a worker-backed LSP lifecycle, and graceful shutdown. Native
Linux/macOS jobs exercise Unix sockets; native Windows CI exercises authenticated
loopback TCP. Cross-build checks verify both Linux and Windows release command
graphs but are not presented as native transport tests.

## Release Inspection

Production builds must retain `grammar_subset,grammar_subset_python`. Native
Darwin builds with CGO enabled also require `-ldflags=-linkmode=external`;
release archives are reproducible cross-builds with `CGO_ENABLED=0`. Run:

```sh
make build
python3 scripts/check_release.py build/pogo build/testclient
go version -m build/pogo
```

The inspection rejects fixture markers in release binaries, rejects production
dependency paths containing `testdata` or `internal/harness`, and AST-checks the
embedded worker import roots against the Python standard library plus Django.
When passed `--vsix`, it also requires all six bundled targets, executable mode
on POSIX entries, source/package version agreement, and byte identity with the
inspected standalone binaries.

Publishing is tag-driven. After the release version in the server and VS Code
extension matches `X.Y.Z`, commit and push those changes to `main`, then push a
`vX.Y.Z` tag for that commit. The CI release job rejects tags whose commit is
not already on `main`, waits for all compatibility, race, native transport,
cross-build, and performance jobs, then publishes Linux, macOS, and Windows
archives for amd64/arm64, a VSIX containing those binaries, and `checksums.txt`
to GitHub Releases.
Non-version tags are never published.

For a clean-room verification, export `HEAD` into a temporary directory, set
fresh `GOCACHE`, `GOMODCACHE`, and `PIP_CACHE_DIR` paths, then run:

```sh
make fixture-env
make build
make test
make test-race
make fuzz
make compat
make bench
python3 scripts/check_release.py build/pogo build/testclient
build/testclient -scenario testdata/requests/normal-shutdown.json -- build/pogo
```

Also copy the sample project outside the exported source tree and run the
worker-lifecycle scenario from an unrelated working directory. This confirms
that the worker is embedded and release execution has no source-fixture
dependency. A clean shutdown must leave no child process, socket, token, or
`pogo-worker-*` runtime directory.
