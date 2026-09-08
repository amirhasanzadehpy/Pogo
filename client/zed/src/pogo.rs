//! Zed extension for Pogo, the Django ORM language server.
//!
//! The extension is deliberately thin: it resolves a `pogo` executable and
//! forwards project configuration as LSP initialization options. Every ORM
//! feature, and all project, settings-module, and environment-file resolution,
//! belongs to the Go server, which reads the same `djangoOrm` options the
//! VS Code and Neovim clients send.

use std::fs;

use zed_extension_api::settings::LspSettings;
use zed_extension_api::{self as zed, serde_json, LanguageServerId, Result};

/// Repository publishing the release archives built by the `release` job in
/// `.github/workflows/ci.yml`.
const RELEASE_REPOSITORY: &str = "amirhasanzadehpy/Pogo";

/// Language server id declared in `extension.toml`. Users name it in
/// `languages.Python.language_servers` and configure it under `lsp` in Zed
/// settings.
const SERVER_NAME: &str = "pogo";

/// Worktree-relative directories probed for a project virtual environment.
/// Pogo already falls back to `.venv`, so this list only rescues the other
/// conventional layouts.
const VIRTUAL_ENVIRONMENT_DIRECTORIES: [&str; 3] = [".venv", "venv", "env"];

/// Marker file written by every virtual environment. It is the only readable
/// proof of one available here: the worktree API reads text files, not
/// directories or executables.
const VIRTUAL_ENVIRONMENT_MARKER: &str = "pyvenv.cfg";

struct PogoExtension {
    /// Path to the most recently resolved downloaded server, revalidated on
    /// every start so a cleared cache directory triggers a fresh download.
    downloaded_server_path: Option<String>,
}

impl PogoExtension {
    /// Resolves the server executable, preferring anything the user already
    /// controls over a download: an explicit `lsp.pogo.binary.path`, then a
    /// `pogo` on `$PATH`, then the newest published release.
    fn server_path(
        &mut self,
        language_server_id: &LanguageServerId,
        worktree: &zed::Worktree,
        configured_path: Option<String>,
    ) -> Result<String> {
        if let Some(path) = configured_path {
            return Ok(path);
        }
        if let Some(path) = worktree.which(SERVER_NAME) {
            return Ok(path);
        }
        if let Some(path) = self
            .downloaded_server_path
            .as_ref()
            .filter(|path| fs::metadata(path).is_ok_and(|entry| entry.is_file()))
        {
            return Ok(path.clone());
        }

        let path = download_server(language_server_id)?;
        self.downloaded_server_path = Some(path.clone());
        Ok(path)
    }
}

impl zed::Extension for PogoExtension {
    fn new() -> Self {
        Self {
            downloaded_server_path: None,
        }
    }

    fn language_server_command(
        &mut self,
        language_server_id: &LanguageServerId,
        worktree: &zed::Worktree,
    ) -> Result<zed::Command> {
        let binary = LspSettings::for_worktree(SERVER_NAME, worktree)
            .ok()
            .and_then(|settings| settings.binary);
        let (configured_path, arguments, environment) = match binary {
            Some(binary) => (binary.path, binary.arguments, binary.env),
            None => (None, None, None),
        };

        Ok(zed::Command {
            command: self.server_path(language_server_id, worktree, configured_path)?,
            args: arguments.unwrap_or_default(),
            // Pogo snapshots only `PATH` from this environment for the Django
            // worker, so the project shell environment lets ordinary project
            // imports locate tools such as Git without leaking the rest.
            env: match environment {
                Some(environment) => {
                    let mut variables: Vec<(String, String)> = environment.into_iter().collect();
                    variables.sort();
                    variables
                }
                None => worktree.shell_env(),
            },
        })
    }

    fn language_server_initialization_options(
        &mut self,
        _language_server_id: &LanguageServerId,
        worktree: &zed::Worktree,
    ) -> Result<Option<serde_json::Value>> {
        let mut options = LspSettings::for_worktree(SERVER_NAME, worktree)
            .ok()
            .and_then(|settings| settings.initialization_options)
            .unwrap_or_else(|| serde_json::json!({}));

        let root = options.as_object_mut().ok_or_else(|| {
            format!("lsp.{SERVER_NAME}.initialization_options must be a JSON object")
        })?;
        let django_orm = root
            .entry("djangoOrm")
            .or_insert_with(|| serde_json::json!({}))
            .as_object_mut()
            .ok_or_else(|| {
                format!("lsp.{SERVER_NAME}.initialization_options.djangoOrm must be a JSON object")
            })?;

        // A configured interpreter always wins; discovery only fills the gap
        // that Zed's lack of a Python environment API leaves behind.
        if !django_orm.contains_key("pythonPath") {
            if let Some(python_path) = project_python_path(worktree) {
                django_orm.insert("pythonPath".to_string(), python_path.into());
            }
        }

        Ok(Some(options))
    }
}

/// Finds the interpreter for the worktree's virtual environment.
///
/// In-project environments outrank `VIRTUAL_ENV` so a globally activated shell
/// cannot shadow the environment that actually has the project's Django
/// installed. Returns `None` when nothing is found, leaving Pogo's own `.venv`
/// fallback and its diagnostic message intact.
fn project_python_path(worktree: &zed::Worktree) -> Option<String> {
    let (os, _) = zed::current_platform();

    for directory in VIRTUAL_ENVIRONMENT_DIRECTORIES {
        let marker = format!("{directory}/{VIRTUAL_ENVIRONMENT_MARKER}");
        if worktree.read_text_file(&marker).is_ok() {
            // Pogo resolves relative interpreter paths against the project root.
            return Some(python_executable_path(os, directory));
        }
    }

    worktree
        .shell_env()
        .into_iter()
        .find(|(name, value)| name == "VIRTUAL_ENV" && !value.is_empty())
        .map(|(_, root)| python_executable_path(os, &root))
}

/// Builds the interpreter path for a virtual environment root, matching the
/// layout `cmd/pogo` expects on each platform.
fn python_executable_path(os: zed::Os, environment_root: &str) -> String {
    match os {
        zed::Os::Windows => format!("{environment_root}\\Scripts\\python.exe"),
        zed::Os::Mac | zed::Os::Linux => format!("{environment_root}/bin/python"),
    }
}

/// Downloads the newest published server for the current platform.
///
/// A failed download removes the partially extracted directory so the next
/// start retries instead of caching a broken tree.
fn download_server(language_server_id: &LanguageServerId) -> Result<String> {
    zed::set_language_server_installation_status(
        language_server_id,
        &zed::LanguageServerInstallationStatus::CheckingForUpdate,
    );

    let release = zed::latest_github_release(
        RELEASE_REPOSITORY,
        zed::GithubReleaseOptions {
            require_assets: true,
            pre_release: false,
        },
    )?;
    let (os, architecture) = zed::current_platform();
    let asset_name = release_asset_name(&release.version, os, architecture)?;
    let asset = release
        .assets
        .iter()
        .find(|asset| asset.name == asset_name)
        .ok_or_else(|| {
            format!(
                "release {} publishes no asset {asset_name}",
                release.version
            )
        })?;

    let release_directory = format!("pogo-{}", release.version);
    let executable_path = match os {
        zed::Os::Windows => format!("{release_directory}\\pogo.exe"),
        zed::Os::Mac | zed::Os::Linux => format!("{release_directory}/pogo"),
    };
    if fs::metadata(&executable_path).is_ok_and(|entry| entry.is_file()) {
        return Ok(executable_path);
    }

    zed::set_language_server_installation_status(
        language_server_id,
        &zed::LanguageServerInstallationStatus::Downloading,
    );
    let downloaded = zed::download_file(&asset.download_url, &release_directory, archive_type(os))
        .map_err(|error| format!("download {asset_name}: {error}"))
        .and_then(|()| zed::make_file_executable(&executable_path));
    if let Err(error) = downloaded {
        fs::remove_dir_all(&release_directory).ok();
        return Err(error);
    }

    remove_superseded_releases(&release_directory);
    Ok(executable_path)
}

/// Names the release archive for the current platform, matching the artifacts
/// the release workflow uploads. `release.version` is the tag name, so it
/// already carries the `v` prefix.
fn release_asset_name(
    version: &str,
    os: zed::Os,
    architecture: zed::Architecture,
) -> Result<String> {
    let goos = match os {
        zed::Os::Linux => "linux",
        zed::Os::Mac => "darwin",
        zed::Os::Windows => "windows",
    };
    let goarch = match architecture {
        zed::Architecture::X8664 => "amd64",
        zed::Architecture::Aarch64 => "arm64",
        zed::Architecture::X86 => {
            return Err("Pogo publishes no 32-bit x86 server build".to_string())
        }
    };
    let extension = match os {
        zed::Os::Windows => "zip",
        zed::Os::Mac | zed::Os::Linux => "tar.gz",
    };
    Ok(format!("pogo-{version}-{goos}-{goarch}.{extension}"))
}

fn archive_type(os: zed::Os) -> zed::DownloadedFileType {
    match os {
        zed::Os::Windows => zed::DownloadedFileType::Zip,
        zed::Os::Mac | zed::Os::Linux => zed::DownloadedFileType::GzipTar,
    }
}

/// Deletes every download except the one just resolved. Cleanup failures are
/// ignored: a stale directory wastes disk, but must never block a working
/// server from starting.
fn remove_superseded_releases(current_directory: &str) {
    let Ok(entries) = fs::read_dir(".") else {
        return;
    };
    for entry in entries.flatten() {
        if entry.file_name() != current_directory {
            fs::remove_dir_all(entry.path()).ok();
        }
    }
}

zed::register_extension!(PogoExtension);
