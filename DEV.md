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
completed. It checks Go formatting, runs `go vet`, runs Go tests, and runs the
fixture's standard-library Python test suite. `make test-race` runs the Go suite
with race detection. `make bench` measures the lifecycle server's idle RSS;
completion latency remains scoped N/A until its handler is introduced.

`make build` writes `build/django-orm-lsp` and `build/testclient`. Run a traced
lifecycle scenario with logs isolated from protocol stdout:

```sh
build/testclient \
  -scenario testdata/requests/normal-shutdown.json \
  -trace-methods \
  -- build/django-orm-lsp -log-file build/protocol.log
```

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
