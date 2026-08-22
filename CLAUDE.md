# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build and Test

All Go builds require the `grammar_subset,grammar_subset_python` build tags. Darwin native builds also require `-ldflags=-linkmode=external` (added automatically by the Makefile).

```sh
make fixture-env      # create/refresh .venv-fixture (required before tests)
make build            # produces build/pogo and build/testclient
make test             # gofmt check, go vet, Go tests, Python fixture and daemon tests
make test-race        # Go suite with race detection
make bench            # build + run benchmark harness with release gate checks
make compat           # pinned Django 4.2 and 5.2 compatibility matrix
make fuzz             # 30-second fuzz campaigns across IPC framing, parsing, and path extraction
make release-check    # inspect production dependencies and embedded imports
make clean            # remove .venv-fixture, build/, benchmark-results/, and db files
```

Run a single Go test package:
```sh
go test -tags=grammar_subset,grammar_subset_python ./internal/analysis/...
go test -tags=grammar_subset,grammar_subset_python -run TestName ./internal/lsp/...
```

Run the Python fixture suite directly:
```sh
.venv-fixture/bin/python -m unittest discover -s testdata/sample_django_project/tests -p 'test_*.py' -v
.venv-fixture/bin/python -m unittest discover -s src/daemon -p 'test_*.py' -v
```

Dump the fixture's runtime schema as JSON:
```sh
.venv-fixture/bin/python src/daemon/introspect.py --project testdata/sample_django_project
```

Run the language server manually with the fixture project:
```sh
build/pogo -project testdata/sample_django_project -settings sample_project.settings \
  -python "$PWD/.venv-fixture/bin/python" -log-file build/protocol.log
```

Run a lifecycle scenario through the test client:
```sh
build/testclient -scenario testdata/requests/normal-shutdown.json -trace-methods -- build/pogo -log-file build/protocol.log
```

## Architecture

Pogo is a Django ORM language server with two deliberately separated execution domains:

1. **Go coordinator** — owns LSP transport (stdio, LSP 3.16 via `tliron/glsp`), incremental Tree-Sitter parsing, model inference, immutable schema indexes, and all editor-facing hot-path lookups.
2. **Embedded Python worker** (`src/daemon`) — starts the target project's Django, inspects the initialized app registry, and serializes a bounded schema snapshot over authenticated local IPC (Unix socket on POSIX, loopback TCP on Windows).

### Package Responsibilities

| Package | Role |
| --- | --- |
| `cmd/pogo` | CLI flags, project/interpreter discovery, logging, LSP server wiring |
| `cmd/testclient` | Integration/lifecycle/scenario test client (test tooling only) |
| `internal/lsp` | LSP lifecycle, capability registration, feature handlers, diagnostics publication, navigation |
| `internal/analysis` | Tree-Sitter parser pool, incremental document state, UTF-16↔UTF-8 conversion, model inference, ORM path extraction |
| `internal/schema` | Wire DTO validation, immutable graph indexes, relation traversal, atomic cache generations via `schema.Cache` |
| `internal/python` | Worker extraction/supervision, authenticated IPC, framing, refresh debounce, schema publication |
| `internal/harness` | LSP framing and scenario runner (test-only; production code must not import it) |
| `src/daemon` | Embedded Python introspection worker |
| `client/vscode` | TypeScript VS Code language client extension |

### Data Flow

1. Editor → Go coordinator over stdio (LSP 3.16).
2. Go incrementally parses open Python documents with the embedded Tree-Sitter Python grammar.
3. After LSP initialization, `internal/python.Manager` extracts and starts the embedded worker.
4. Worker calls `django.setup()`, reads runtime metadata, returns a protocol-v1 JSON snapshot.
5. Go validates, builds immutable indexes, publishes `{graph, generation}` atomically via `schema.Cache` (`atomic.Pointer`).
6. LSP feature handlers read only the in-memory graph and parsed document state — never Python.

### Schema Cache and Provisional Graph

On POSIX, `cmd/pogo` managers opt into a persistent cache under `os.UserCacheDir()/pogo`. A matching cached snapshot can publish as a provisional graph immediately while the real Django worker starts. The runtime graph atomically replaces the provisional one. Cache identity is a SHA-256 digest over all normalized configuration inputs.

## Critical Constraints

These are non-negotiable and must be preserved in all changes:

**Hot-Path Isolation** — Completion, hover, signature help, definition, and diagnostic handlers must NEVER invoke Python, start processes, scan the filesystem, or block on I/O. They read only the current in-memory schema generation and parsed document state. Feature code must not import `internal/python`.

**Zero External Python Dependencies** — `src/daemon` may import only Python's standard library and Django. Never introduce PyPI packages into the worker. Run `make release-check` after any worker import change — it's an architectural gate.

**Immutable Published Graphs** — Never mutate a published `schema.Graph` or its maps/slices. A refresh builds a complete replacement and publishes atomically. Failed refreshes retain the last valid generation; nil or partial graphs are never published.

**Position Coordinates** — LSP positions are UTF-16 code-unit offsets. Tree-Sitter and schema source ranges are UTF-8 byte offsets. Convert explicitly at layer boundaries.

**Schema Version** — The worker JSON schema is a versioned cross-language API. Any field or semantic change must update Python serialization, Go DTO/wire decoding, graph validation, fixtures, and tests together. Increment `schema_version` when compatibility breaks.

**Build Tags** — All Go builds must use `-tags=grammar_subset,grammar_subset_python`. Release builds must not contain `internal/harness` or `testdata` in their production dependency graph.

**Performance** — Target p95 below 100 µs for warmed editor interactions. The automated release gate is completion p95 < 10 ms, Go RSS ≤ 50 MiB, combined RSS ≤ 150 MiB. Precompute indexes at schema build time. Avoid allocations, reflection, regexes, and sorting in hot loops.

**Dependency Direction** — `lsp → analysis/schema`, `python → schema`, command packages → internal packages. Feature handlers must not depend on `internal/python`.

## Python Worker Standards

- Support Python 3.10–3.13 and Django 4.2 (Python 3.10–3.12) and Django 5.2 (Python 3.10–3.13).
- Use PEP 484 type annotations with built-in generics (no `from __future__ import annotations` required unless already present).
- Catch broad `Exception` only around Django extension hooks; never catch `BaseException`.
- Introspection must not call manager/queryset methods, run queries, write databases, or perform network activity.
- JSON stdout is machine protocol output — route all diagnostic/debug output through the log channel.

## Fixture Environment

The fixture uses an isolated virtualenv at `.venv-fixture/` pinned to Django 5.2.16. Never install fixture dependencies into the system Python. The normal SQLite DB is at `testdata/sample_django_project/db.sqlite3`; automated tests set `POGO_FIXTURE_DB` to a temp path instead.
