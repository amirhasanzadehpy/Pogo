# Changelog

All notable changes to Pogo are documented in this file.

## [Unreleased]

### Added
- CI now publishes tagged releases to the VS Code Marketplace and Open VSX, in addition to GitHub Releases.
- MIT license.
- Marketplace-required extension metadata: icon and gallery banner.
- Completion, hover, and go-to-definition for model constructor keyword arguments and for field names inside `bulk_create`/`bulk_update` list-literal arguments (`unique_fields`, `update_fields`, `fields`).

### Fixed
- The worker now surfaces the actual exception when Django schema introspection fails, instead of a generic message, so misconfiguration (ambiguous settings module, unsupported Django version, etc.) is diagnosable from the log.
- The worker logs a warning when the Django app registry reports zero concrete models, to distinguish a misconfigured project from one that legitimately has none yet.
- Stock QuerySet method metadata is deduplicated in the wire protocol (schema v3) instead of being inlined per model and manager, fixing `response_too_large` worker failures on large schemas.
- QuerySet methods monkey-patched onto Django's `QuerySet` class by third-party libraries (e.g. `django-cacheops`) now fall back to their owning class's source range instead of failing schema validation.

### Changed
- Removed the `private` flag from the VS Code extension manifest to allow Marketplace publishing.

## [0.2.7] - 2026-08-12
### Added
- Nested ORM path support (chained relation traversal in `filter`/`select_related`/`prefetch_related` and similar paths).
### Fixed
- Enclosing model class inference for completion/hover contexts.

## [0.2.6] - 2026-08-12
### Fixed
- Source manifests are now completed for large projects instead of being silently truncated.

## [0.2.5] - 2026-08-12
### Fixed
- Test reliability: contended fixture startup and native cache behavior in the test suite.

## [0.2.4] - 2026-08-12
### Added
- Validated schemas are restored provisionally on startup, so a previously known-good schema is available immediately while the worker refreshes.

## [0.2.3] - 2026-08-12
### Changed
- Accelerated Django schema startup in the worker.

## [0.2.2] - 2026-08-11
### Fixed
- Quoted Windows interpreter paths are now handled correctly.

## [0.2.1] - 2026-08-11
### Fixed
- CI now passes an absolute fixture interpreter path.

## [0.2.0] - 2026-08-11
### Added
- Go-to-definition navigation (replacing document links) and `ModelAdmin.queryset` admin inference.
### Changed
- Simplified Django startup and the VS Code install flow.
- Revamped the project overview documentation and refreshed the benchmark baseline.
### Fixed
- Duplicate process kill on Windows in the test harness.
- Closed stdout is now accepted as process EOF in the test harness.

## [0.1.0] - 2026-07-31
Initial release.
### Added
- Embedded Python worker for Django schema introspection.
- Go coordinator: LSP server lifecycle, schema worker integration, and in-memory model cache.
- Test client and fixture test environment.
