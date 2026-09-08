# Pogo for Zed

Zed extension for [Pogo](https://github.com/amirhasanzadehpy/Pogo), the Django
ORM language server.

Zed registers Pogo as an additional Python language server, so it runs beside
Pyright, BasedPyright, or Ruff rather than replacing them. The extension only
resolves a server executable and forwards project configuration; every ORM
feature is implemented by the Go server.

## Install

Requires **Zed 1.17 or newer**. Earlier versions ignore the LSP `filterText`
that ORM path completion depends on, and silently drop every candidate after a
`__` separator.

Install **Pogo — Django ORM** from Zed's extension gallery
(`zed: extensions`). No settings are required: Zed's default `language_servers`
list ends with `"..."`, which picks up newly registered servers.

Name Pogo explicitly only if your settings pin a list without `"..."`:

```jsonc
{
  "languages": {
    "Python": {
      "language_servers": ["basedpyright", "pogo", "..."]
    }
  }
}
```

## Server binary

The extension resolves the server in this order:

1. `lsp.pogo.binary.path` in Zed settings.
2. A `pogo` executable on the project `$PATH`.
3. The newest [GitHub release](https://github.com/amirhasanzadehpy/Pogo/releases)
   for the current OS and CPU, downloaded into Zed's extension work directory.

Downloads are pinned to the release tag, and superseded downloads are removed
once a newer one is in place. The extension version is independent of the server
version: it always tracks the latest published server.

Step 2 wins silently, so a `pogo` installed by hand — as the pre-extension Zed
setup required — keeps running at whatever version it was installed at, and the
extension never downloads anything. Check with `pogo -version` against the
[latest release](https://github.com/amirhasanzadehpy/Pogo/releases). To hand the
choice back to the extension, delete that binary; to keep it deliberately, name
it in `lsp.pogo.binary.path`.

`lsp.pogo.binary.arguments` and `lsp.pogo.binary.env` are passed through as
given. Setting `env` replaces the project shell environment rather than
extending it; Pogo snapshots only `PATH` from it for the Django worker.

## Configuration

Pogo works without configuration when the project has a `.venv`, `venv`, or
`env` virtual environment, or a `manage.py` that sets `DJANGO_SETTINGS_MODULE`.
Add overrides only for layouts that need them:

```jsonc
{
  "lsp": {
    "pogo": {
      "initialization_options": {
        "djangoOrm": {
          "pythonPath": ".venv/bin/python",
          "settingsModule": "config.settings",
          "environmentFile": ".env.pogo",
          "environment": {
            "APP_MODE": "development",
            "DEBUG": null
          }
        }
      }
    }
  }
}
```

| Option | Meaning |
| --- | --- |
| `pythonPath` | Interpreter that runs the Django worker. Relative paths resolve from the project root. |
| `settingsModule` | Django settings module. Defaults to the value `manage.py` sets. |
| `environmentFile` | Environment file loaded by the worker. Pogo never sends its contents back. |
| `environment` | Literal overrides applied after the file; a string replaces a value and `null` removes it. |

Zed's toolchain selection is not exposed to extensions, so the extension
discovers the interpreter itself: an in-project `.venv`, `venv`, or `env`
first, then `VIRTUAL_ENV` from the project shell environment. An in-project
environment wins so a globally activated shell cannot shadow the one that has
the project's Django installed. `pythonPath` overrides all of it.

Literal `environment` values cross LSP initialization and can appear in LSP
traces; use them only for nonsecret configuration. Keep secrets in an ignored,
permission-restricted `environmentFile`.

## Develop

Requires a Rust toolchain with the `wasm32-wasip1` target:

```sh
rustup target add wasm32-wasip1
cargo fmt --check
cargo clippy --target wasm32-wasip1 -- -D warnings
cargo build --release --target wasm32-wasip1
```

The first build resolves dependencies and writes `Cargo.lock`; commit it so the
registry and CI build the same dependency versions.

To run the working tree in Zed, open `zed: install dev extension` and select
this directory.
