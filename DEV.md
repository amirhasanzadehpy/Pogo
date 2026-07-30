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

`make build` writes `build/django-orm-lsp` and `build/testclient`. Run a traced
lifecycle scenario with logs isolated from protocol stdout:

```sh
build/testclient \
  -scenario testdata/requests/normal-shutdown.json \
  -trace-methods \
  -- build/django-orm-lsp -log-file build/protocol.log
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
build/django-orm-lsp \
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
