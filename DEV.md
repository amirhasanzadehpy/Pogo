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
diagnostic, definition, and document-link handlers; end-to-end p50/p95; schema
refresh; graph lookup; and server/worker RSS.

`make fuzz` runs 30-second campaigns for worker and LSP framing, UTF-16
position/edit conversion, ORM path extraction, and parser recovery. `make
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

The worker starts after the LSP `initialized` notification, communicates over a
private local endpoint using bounded protocol-v1 JSON frames, and is stopped and
reaped on shutdown, exit, or stdio EOF. Worker and project output is forwarded
to the language-server log and never to LSP stdout.

Open Python documents are parsed incrementally in Go with only the Python
grammar embedded in release builds. Completion and hover resolve direct fields,
deep relations, type-specific lookups and transforms, projection strings,
related-loading paths, attached custom managers, and custom QuerySet chains.
Signature help and method hover use cached signatures and docstrings without
executing project methods. These hot handlers continue to use the last valid
cache when the worker is unavailable and never issue worker requests.

Definition navigation uses the same cache-only resolver for model imports,
fields, relations, managers, custom methods, and individual ORM path segments.
Inherited and reverse fields retain their originating declaration, while lookup
and transform suffixes target the underlying field. Document links cover static
relation-target and `related_name` strings in model declarations and resolvable
schema-backed string query paths. Targets are absolute percent-encoded file URIs;
missing or stale source files are omitted without failing the request.

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
sandbox for project code.

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
| LSP handlers | Completion, hover, diagnostics, definition, document links |

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
`ns/op`, `B/op`, `allocs/op`, p50/p95/p99 where sampled, aligned RSS samples,
and gate results. A missing required metric fails the harness. The fixed gates
are completion p95 `<10 ms`, Go RSS `<=50 MiB`, and combined RSS `<=150 MiB`.
Linux and macOS failures are not waived for platform variance; Windows RSS is
informational because the standard-library sampler is unavailable there.

Inspect a captured profile directly:

```sh
go tool pprof -top benchmark-results/pprof/completion.cpu.prof
go tool pprof -top -alloc_space benchmark-results/pprof/completion.heap.prof
```

### Reference Baseline

Measured on 2026-07-31 at commit `2fa5cb6` plus the Milestone 9 working tree,
Apple M1 arm64, macOS 26.5.2, Go 1.22.12, Python 3.11.10, and Django 5.2.16.
Values are real warmed runs from `make bench`; host load and toolchain changes
can affect informational values but not the fixed Linux/macOS gates.

| Profile | p50 | p95 | p99 | `ns/op` | `B/op` | allocations |
|---|---:|---:|---:|---:|---:|---:|
| Parse plus completion | 44.29 us | 51.46 us | 63.62 us | 45,558 | 36,978 | 111 |
| Completion handler | N/A | N/A | N/A | 4,621 | 7,956 | 49 |
| Hover handler | 2.88 us | 3.33 us | 6.21 us | 3,082 | 2,174 | 34 |
| Diagnostics end-to-end | 11.67 us | 15.62 us | 19.38 us | 14,005 | 33,395 | 85 |
| Definition | 23.62 us | 25.83 us | 30.04 us | 24,281 | 4,415 | 80 |
| Document links | 91.71 us | 99.79 us | 106.40 us | 93,242 | 15,087 | 345 |
| Dense 256-relation completion | 207.00 us | 506.40 us | 576.50 us | 232,797 | 311,858 | 4,381 |
| 10,000-model completion | 3.46 us | 5.54 us | 7.21 us | 4,074 | 6,414 | 40 |
| 100 KiB parse update | 11.68 ms | 14.98 ms | 14.98 ms | 12,318,338 | 8,610,690 | 1,124 |
| 1,000-document snapshot | 1.42 ms | 3.65 ms | 3.65 ms | 1,699,017 | 314,680 | 3,004 |
| Cache read | 0.04 us | 0.04 us | 0.04 us | 86.4 | 0 | 0 |
| Snapshot swap under readers | 0.29 us | 0.33 us | 0.33 us | 339.8 | 24 | 1 |

| Schema profile | Time | Bytes/op | Allocations/op |
|---|---:|---:|---:|
| 10 models | 166.50 us | 81,848 | 759 |
| 1,000 models | 9.41 ms | 8,158,672 | 74,092 |
| 10,000 models | 72.44 ms | 81,125,600 | 740,619 |
| Dense 256-field model | 371.83 us | 431,968 | 1,421 |

| Memory profile | Measured | Gate |
|---|---:|---:|
| Go RSS maximum | 19.56 MiB | `<=50 MiB` |
| Python worker RSS maximum | 47.62 MiB | Informational |
| Combined p50/p95/p99/maximum | 67.19 MiB | `<=150 MiB` |

Typed schema cloning reduced the 10,000-model build from the pre-optimization
profile's 340.47 ms and 201,471,736 B/op to 72.44 ms and 81,125,600 B/op.
Parser pooling reduced parse-plus-completion p95 from the measured pre-change
16.89-39.43 ms range to tens of microseconds on the reference fixture.

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

Production builds must retain `grammar_subset,grammar_subset_python`; Darwin
also requires `-ldflags=-linkmode=external`. Run:

```sh
make build
python3 scripts/check_release.py build/pogo build/testclient
go version -m build/pogo
```

The inspection rejects fixture markers in release binaries, rejects production
dependency paths containing `testdata` or `internal/harness`, and AST-checks the
embedded worker import roots against the Python standard library plus Django.

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
