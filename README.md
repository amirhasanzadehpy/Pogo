# Pogo

Pogo is an LSP 3.16 language server for Django ORM code. It starts Django only
on schema-loading cold paths, stores an immutable runtime schema in Go, and
serves completion, hover, signature help, diagnostics, definitions, and
document links without invoking Python on editor hot paths.

## Requirements

- Python 3.10 through 3.13 in the target project environment.
- Django 5.2, or Django 4.2 as an EOL compatibility target, with a
  Django-supported Python version.
- Go 1.22 or newer only when building from source.

Pogo adds no worker-specific Python package. The embedded worker uses the
Python standard library and the Django installation already present in the
selected project environment. Django's transitive dependencies and packages
imported by the target project are still required.

## Build

```sh
make fixture-env
make build
build/django-orm-lsp -version
```

`make build` embeds only the Python Tree-sitter grammar and writes
`build/django-orm-lsp` and `build/testclient`.

## Run

```sh
build/django-orm-lsp \
  -project /absolute/path/to/project \
  -python /absolute/path/to/project/.venv/bin/python \
  -settings config.settings \
  -log-file /absolute/path/to/django-orm-lsp.log
```

The server speaks LSP over stdio. Stdout is protocol-only. Server logs and
target-project output go to stderr or `-log-file`.

LSP clients can provide the same configuration during initialization:

```json
{
  "initializationOptions": {
    "djangoOrm": {
      "projectRoot": "/absolute/path/to/project",
      "pythonPath": "/absolute/path/to/project/.venv/bin/python",
      "settingsModule": "config.settings"
    }
  }
}
```

CLI values take precedence over initialization options. Without an explicit
project root, Pogo uses a sole workspace folder, `rootUri`, then `rootPath`.
Multiple workspace folders require an explicit project root.

Python selection is CLI, initialization option, `VIRTUAL_ENV`, project
`.venv`, then `python3`. Settings selection is CLI, initialization option,
`DJANGO_SETTINGS_MODULE`, static extraction from `manage.py`, then one
unambiguous immediate `*/settings.py` candidate.

## Feature Matrix

| Django ORM context | Supported behavior |
|---|---|
| Model fields, `pk`, relation attnames, reverse accessors | Completion, hover, definition |
| Managers and custom manager methods | Completion, hover, definition, signature help |
| Custom QuerySet chains | Completion, hover, definition, signature help |
| `filter`, `exclude`, `get` keyword paths | Relations, transforms, lookups, diagnostics |
| `values`, `values_list` | Static projection paths and transforms |
| `only`, `defer` | Static field paths |
| `select_related` | Single-valued relation paths |
| `prefetch_related` | Forward and reverse relation paths |
| Relation and query-path strings | Document links |
| General Python typing, rename, references, formatting | Not provided |

Static inference supports direct and aliased imports, qualified model
references, constructors, model and `QuerySet[Model]` annotations, annotated
parameters, simple assignments, managers, QuerySets, and the known
model-returning methods `get`, `first`, `last`, and `create`.

Dynamic expressions, interprocedural inference, runtime argument-value typing,
f-strings, `**filters`, unresolved receivers, ambiguous or guarded assignments,
and parser-recovered calls intentionally produce no speculative result.

## Compatibility

| Python | Django 4.2 | Django 5.2 |
|---|---:|---:|
| 3.10 | Tested | Tested |
| 3.11 | Tested | Tested |
| 3.12 | Tested | Tested |
| 3.13 | Unsupported by Django 4.2 | Tested |

Django 4.2 is an EOL compatibility target. Linux and macOS use private Unix
sockets. Windows uses authenticated IPv4 loopback TCP because the Go standard
library does not provide named pipes.

## Refresh And Failure Behavior

The worker starts asynchronously after `initialized`. A Python save under an
installed application root uses a 300 ms trailing-edge debounce and starts a
fresh Django process. A complete snapshot is strictly validated before one
atomic cache-generation swap.

Startup or refresh failures produce a client warning and are written to the
server log. The last valid immutable cache remains available. Before the first
valid snapshot, schema-backed requests return no result and diagnostics remain
empty rather than returning guessed data. Worker crashes use bounded
exponential restart backoff; shutdown terminates and reaps the child and removes
its private endpoint and runtime directory.

## Trust Boundary

Django startup imports and executes target-project code with the selected
Python environment and inherited process environment. Project imports may
access files, databases, services, or the network. Pogo is not a sandbox; use
it only with trusted workspaces.

Unix IPC uses a mode-0600 socket in a mode-0700 temporary directory. Windows
uses loopback TCP. Every session uses a fresh random one-time token and seals
the listener after authentication. These controls protect the endpoint from
unrelated local clients but do not restrict project code.

## Performance

`make bench` warms the fixture, writes human and machine-readable results under
`benchmark-results/`, captures CPU and heap profiles, and enforces:

- Loaded-cache completion p95 below 10 ms.
- Go RSS at or below 50 MiB.
- Combined Go and Python worker RSS at or below 150 MiB on Linux/macOS.

The checked release baseline and profiling recipe are documented in `DEV.md`.
