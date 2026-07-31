# Pogo

**Fast, runtime-accurate Django ORM intelligence for your editor.**

Pogo is an LSP 3.16 language server that understands the model schema Django
actually loads. It provides ORM completion, hover information, signature help,
diagnostics, go-to-definition, and document links while keeping Python off the
editor hot path.

## Quick Start

1. Download the archive for your operating system and CPU from
   [GitHub Releases](https://github.com/amirhasanzadehpy/Pogo/releases).
2. Put the executable on `PATH` and confirm it runs:

   ```sh
   pogo -version
   ```

3. Add the setup for [VS Code](#vs-code), [Neovim](#neovim), or
   [Zed](#zed).
4. Open the directory containing `manage.py` and edit a Python file. Pogo finds
   the project environment and Django settings automatically.

Pogo supports Python 3.10 through 3.13 with Django 5.2. Django 4.2 remains an
EOL compatibility target on the Python versions Django 4.2 supports.

## Installation

Pogo is one native executable. It does not install a Python package into your
project; the embedded worker uses the standard library and the Django already
installed in the selected project environment.

### Linux And macOS

Release archives are named `pogo-vX.Y.Z-<os>-<arch>.tar.gz`. Download the
`linux` or `darwin` archive matching `amd64` or `arm64`, verify it against
`checksums.txt`, and extract it. The archive contains one `pogo` binary. Install
it for your user:

```sh
mkdir -p "$HOME/.local/bin"
install -m 0755 "$HOME/Downloads/pogo" "$HOME/.local/bin/pogo"
export PATH="$HOME/.local/bin:$PATH"
pogo -version
```

Make the `PATH` change permanent by adding this line to `~/.profile`,
`~/.zprofile`, or your shell's equivalent:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

For a machine-wide installation instead:

```sh
sudo install -m 0755 "$HOME/Downloads/pogo" /usr/local/bin/pogo
pogo -version
```

If macOS reports that the downloaded binary cannot be opened, approve it in
**System Settings > Privacy & Security**, or remove the quarantine attribute
after verifying the release checksum:

```sh
xattr -d com.apple.quarantine "$HOME/.local/bin/pogo"
```

### Windows

Download the matching `pogo-vX.Y.Z-windows-<arch>.zip` and `checksums.txt`.
Verify the archive with `Get-FileHash -Algorithm SHA256`, extract it, then run
this in PowerShell:

```powershell
$PogoBin = Join-Path $HOME ".local\bin"
New-Item -ItemType Directory -Force $PogoBin | Out-Null
Copy-Item "$HOME\Downloads\pogo.exe" "$PogoBin\pogo.exe"

$UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (($UserPath -split ";") -notcontains $PogoBin) {
    [Environment]::SetEnvironmentVariable("Path", "$PogoBin;$UserPath", "User")
}
$env:Path = "$PogoBin;$env:Path"
pogo.exe -version
```

Restart the editor after changing the user `PATH`.

### Build From Source

Building requires Git, Go 1.22 or newer, and `make` on Linux/macOS:

```sh
git clone https://github.com/amirhasanzadehpy/Pogo.git
cd Pogo
make build
./build/pogo -version
```

The development binary remains at `build/pogo` on Linux/macOS and
can be used directly without installing it globally.

On Windows, build the server from PowerShell with:

```powershell
git clone https://github.com/amirhasanzadehpy/Pogo.git
Set-Location Pogo
New-Item -ItemType Directory -Force build | Out-Null
go build -tags="grammar_subset,grammar_subset_python" -o build/pogo.exe ./cmd/pogo
.\build\pogo.exe -version
```

## Editor Setup

### VS Code

Pogo's VS Code extension starts one server for each Python workspace folder,
finds `pogo` on the extension host's `PATH`, and reports a direct
download action when the binary is missing.

Install the release VSIX from a terminal:

```sh
code --install-extension "$HOME/Downloads/pogo-0.1.0.vsix"
```

Alternatively, open **Extensions: Install from VSIX...** from the command
palette and select the downloaded file. Reload VS Code, open a trusted Django
project, and open any Python file.

For a source checkout, build and install the extension with:

```sh
cd client/vscode
npm ci --include=dev
npm run package
code --install-extension pogo-0.1.0.vsix --force
```

To use a server built in a separate Pogo checkout, add its absolute path to the
Django project's `.vscode/settings.json`:

```json
{
  "pogo.executablePath": "/absolute/path/to/Pogo/build/pogo"
}
```

Relative paths are resolved from each Django workspace folder, so
`"build/pogo"` is valid only when that binary is inside the project.
On Windows, use the `.exe` path. For projects whose environment is ambiguous,
all available overrides are:

```json
{
  "pogo.executablePath": "/absolute/path/to/pogo",
  "pogo.pythonPath": ".venv/bin/python",
  "pogo.settingsModule": "config.settings",
  "pogo.envFile": ".env",
  "pogo.environment": {
    "DEBUG": null
  }
}
```

The server environment starts with the VS Code extension host environment.
Values from `pogo.envFile` override inherited values, then
`pogo.environment` wins. Set a key to `null` to remove an inherited variable.
This is useful when editor tooling exports a generic variable such as `DEBUG`
that has different meaning in the Django project.
Do not put credentials directly in `pogo.environment`: workspace settings may
be committed and user settings may be synchronized. Keep secrets in an ignored,
permission-restricted dotenv file and reference it with `pogo.envFile`.

In Remote SSH, WSL, Dev Containers, and Codespaces, install the binary or set
the executable path on the remote extension host, not the local desktop.

### Neovim

Current `nvim-lspconfig` uses Neovim's built-in configuration API. With Neovim
0.11.3 or newer and `nvim-lspconfig` installed, put this in your Lua config:

```lua
vim.lsp.config("pogo", {
  cmd = { "pogo" },
  filetypes = { "python" },
  root_markers = { "manage.py", "pyproject.toml", ".git" },
})

vim.lsp.enable("pogo")
```

For `lazy.nvim`, save this as a plugin spec such as
`lua/plugins/pogo.lua`:

```lua
return {
  {
    "neovim/nvim-lspconfig",
    ft = { "python" },
    config = function()
      vim.lsp.config("pogo", {
        cmd = { "pogo" },
        filetypes = { "python" },
        root_markers = { "manage.py", "pyproject.toml", ".git" },
      })

      vim.lsp.enable("pogo")
    end,
  },
}
```

For a local build, replace the command with an absolute path:

```lua
cmd = { "/absolute/path/to/Pogo/build/pogo" }
```

Only add initialization overrides when automatic discovery is insufficient:

```lua
vim.lsp.config("pogo", {
  cmd = { "pogo" },
  filetypes = { "python" },
  root_markers = { "manage.py", "pyproject.toml", ".git" },
  init_options = {
    djangoOrm = {
      pythonPath = "/absolute/path/to/project/.venv/bin/python",
      settingsModule = "config.settings",
    },
  },
})

vim.lsp.enable("pogo")
```

Run `:checkhealth vim.lsp` to inspect the active command, root, and server log.

### Zed

Zed can override the command behind a registered language-server adapter, but
its `settings.json` cannot register a new adapter name. Until Pogo has a native
Zed adapter, use Zed's registered `pyright` slot as the external-command bridge.
This starts Pogo, not Pyright, for that slot.

Open **Zed: Open Settings**, then add:

```jsonc
{
  "languages": {
    "Python": {
      "language_servers": ["pyright", "..."]
    }
  },
  "lsp": {
    "pyright": {
      "binary": {
        "path": "/home/you/.local/bin/pogo",
        "arguments": []
      }
    }
  }
}
```

Change `binary.path` to the absolute installation path. On macOS that is often
`/Users/you/.local/bin/pogo`; on Windows, JSON paths use escaped
backslashes such as `C:\\Users\\you\\.local\\bin\\pogo.exe`.

To keep separate Python type checking, install another registered Python server
such as BasedPyright and list it after the bridge:

```jsonc
{
  "languages": {
    "Python": {
      "language_servers": ["pyright", "basedpyright", "..."]
    }
  },
  "lsp": {
    "pyright": {
      "binary": {
        "path": "/home/you/.local/bin/pogo",
        "arguments": [
          "-python",
          "/absolute/path/to/project/.venv/bin/python",
          "-settings",
          "config.settings"
        ]
      }
    }
  }
}
```

The Pyright adapter does not forward arbitrary initialization options, so this
bridge uses Pogo's equivalent CLI flags. Put project-specific settings in
`.zed/settings.json` and restart language servers after changing arguments.

## Environment Resolution

Pogo resolves the project, Python interpreter, and Django settings separately.
The first non-empty match in each list wins.

### Project Root

| Priority | Source |
|---:|---|
| 1 | Server flag: `-project /absolute/path` |
| 2 | LSP option: `djangoOrm.projectRoot` |
| 3 | The sole LSP workspace folder |
| 4 | LSP `rootUri` |
| 5 | Legacy LSP `rootPath` |

Multiple workspace folders require an explicit project root. The VS Code
extension handles this automatically by starting one Pogo client per folder.

### Python Interpreter

| Priority | Source |
|---:|---|
| 1 | Server flag: `-python /absolute/path/to/python` |
| 2 | LSP option: `djangoOrm.pythonPath` |
| 3 | `$VIRTUAL_ENV/bin/python` or `%VIRTUAL_ENV%\\Scripts\\python.exe` |
| 4 | `<project>/.venv/bin/python` or `<project>\\.venv\\Scripts\\python.exe`, when present |
| 5 | `python3` from `PATH` |

An activated virtual environment therefore wins over the project's `.venv`.
Use the editor override when the editor process inherited the wrong
`VIRTUAL_ENV`. On Windows, set an explicit Python path when the installation
provides `python.exe` or `py.exe` but not `python3.exe`.

### Django Settings Module

| Priority | Source |
|---:|---|
| 1 | Server flag: `-settings config.settings` |
| 2 | LSP option: `djangoOrm.settingsModule` |
| 3 | `DJANGO_SETTINGS_MODULE` inherited by Pogo |
| 4 | A literal `DJANGO_SETTINGS_MODULE` value in `manage.py`'s `os.environ.setdefault(...)` call |
| 5 | One unambiguous immediate `*/settings.py` under the project root |

If no settings file is found, or several immediate settings files are possible,
Pogo reports the ambiguity instead of guessing. Set `pogo.settingsModule` in
VS Code, `djangoOrm.settingsModule` in Neovim's initialization options, or
`-settings` in Zed's bridge arguments.

## Verify The Setup

Open a model-aware query and request completion inside the string:

```python
from shop.models import Order

Order.objects.filter(customer__address__city__icontains="York")
```

Pogo should provide field, relation, transform, and lookup completion; hover
information; diagnostics for invalid path segments; and go-to-definition for
model-backed symbols. It also understands managers, custom QuerySets,
`values`, `values_list`, `only`, `defer`, `select_related`, and
`prefetch_related`.

Pogo intentionally does not replace a general Python language server. Keep
Pyright, BasedPyright, Ruff, or another Python tool enabled for general typing,
rename, references, imports, and formatting.

## Troubleshooting

### The Editor Cannot Find The Binary

Confirm the editor can see the executable:

```sh
command -v pogo
pogo -version
```

On Windows:

```powershell
Get-Command pogo.exe
pogo.exe -version
```

Desktop applications, especially on macOS, may not inherit your interactive
shell's `PATH`. Use the editor's absolute executable-path setting or start the
editor from a terminal after exporting `PATH`.

### ORM Results Are Empty Or Stale

Pogo starts Django asynchronously after LSP initialization. Until the first
schema load completes, schema-backed requests return no result rather than
guessed data. Check that:

- The selected Python interpreter has Django and project dependencies installed.
- The workspace root contains the Django project.
- The settings module resolves using the precedence table above.
- Any environment setup normally performed by `manage.py` is represented by
  `pogo.envFile` and `pogo.environment` in VS Code.
- Importing the project does not raise an exception.

In VS Code, open **View > Output** and select **Pogo**. In Neovim, run
`:LspLog`. In Zed, run **Zed: Open Log**. Saving Python under an installed Django
application triggers a debounced schema refresh; the last valid schema remains
available if refresh fails.

### Configuration Changed But Nothing Happened

The VS Code extension restarts its clients when a `pogo.*` setting changes.
Neovim initialization options and Zed bridge arguments are startup-only, so
restart the language server after changing them.

### Multiple Projects In One Window

VS Code starts a separate Pogo process for every local workspace folder. In
Neovim or Zed, configure each Django project as its own LSP root. A single Pogo
process deliberately rejects multiple workspace folders unless
`djangoOrm.projectRoot` is explicit.

## Security

Pogo starts Django, and Django imports and executes code from the project. That
code can access files, databases, services, credentials, and the network with
the editor process's permissions. Pogo is not a sandbox: use it only with
trusted workspaces. The VS Code extension is disabled in untrusted workspaces.

Runtime schema loading uses a private authenticated local endpoint. Editor hot
paths read an immutable in-process schema and do not invoke Python.
