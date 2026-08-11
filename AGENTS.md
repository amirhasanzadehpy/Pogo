# Pogo Engineering Guidelines

This file is the repository-wide operating contract for AI coding agents and
human contributors. It applies to every change unless a more specific
`AGENTS.md` exists below the directory being changed. Read the relevant source,
tests, `README.md`, and `DEV.md` before modifying behavior. Prefer the smallest
correct change that preserves Pogo's latency, memory, compatibility, security,
and failure-isolation properties.

Use `MAINTENANCE.md` for operational CI triage, release publication and
verification, benchmark evidence, rendered documentation, GitHub transport, and
handoff procedures. `AGENTS.md` remains authoritative when the playbook or an
older plan conflicts with this contract.

## Project Mission

Pogo is a Django ORM language server. It provides runtime-accurate completion,
hover, signature help, diagnostics, and definitions to LSP 3.16 clients while
keeping editor interactions independent of Python startup and project import
latency.

The system has two deliberately separated execution domains:

1. The Go coordinator owns LSP transport, incremental Tree-Sitter parsing,
   conservative Python/Django inference, immutable schema indexes, and every
   editor-facing hot-path lookup.
2. The embedded Python worker starts the target project's installed Django,
   inspects the initialized app registry in the background, and serializes a
   bounded schema snapshot over authenticated local IPC.

Runtime Django metadata is the source of schema truth. The validated Go graph is
the only source used while serving editor feature requests.

## Core Non-Negotiables

### Hot-Path Isolation

- Python MUST NEVER run synchronously or asynchronously as a consequence of a
  completion, hover, signature-help, definition, diagnostic, or
  keystroke/document-change lookup.
- Hot-path handlers MUST read only current Go document state and the currently
  published in-memory schema generation. They must continue to work when the
  worker has exited or is unavailable.
- Do not add worker RPC, process startup, filesystem scans, Django imports,
  network access, subprocess execution, or unbounded waits to editor request
  handlers.
- Schema extraction is permitted only during initialization and debounced
  background refreshes caused by schema-affecting saves. Publication occurs only
  after complete payload validation and graph construction.
- Preserve tests proving cached handlers make no worker requests, especially
  `TestCachedFeatureHandlersDoNotRequestWorker`-style coverage in
  `internal/lsp`.

### Zero External Python Dependencies

- Production code in `src/daemon` may import only Python's standard library and
  Django from the interpreter selected for the target project.
- Never install or require Pogo-specific PyPI packages in a user's project or
  selected interpreter. Do not introduce `requests`, `pydantic`, `msgpack`,
  `typing_extensions`, or similar convenience dependencies into the worker.
- The fixture environment may install Django for tests. That does not permit a
  production worker dependency beyond Django.
- Keep the worker self-contained and embeddable through `src/daemon/embed.go`.
  A release binary must not depend on repository source files or test fixtures
  at runtime.
- Run `make release-check` after changing worker imports or embedding. Its AST
  inspection is an architectural gate, not an optional lint.

### Fault Tolerance And Last-Valid State

- Syntax errors, import failures, missing settings, invalid Django projects,
  malformed or oversized frames, worker crashes, timeouts, and invalid schemas
  MUST NOT crash or wedge the Go LSP coordinator.
- Never replace a valid graph with `nil`, a partially built graph, an invalid
  snapshot, or a result from a superseded refresh. Failed refreshes retain the
  last valid generation.
- Build and validate a candidate graph completely before publication. Publish a
  graph and generation together in one atomic state replacement.
- User-visible failures must be concise and actionable. Detailed worker and
  project output belongs in the language-server log, never LSP stdout.
- Worker restarts must remain bounded, cancellable, backoff-controlled, and
  leak-free. Shutdown must reap child processes and remove runtime directories,
  sockets, and token files.

### Microsecond Performance Mindset

- Treat document updates, AST extraction, model inference, graph traversal, and
  editor handlers as allocation-sensitive code.
- The engineering target for ordinary warmed editor interactions is p95 below
  `100 us`; use the tracked profiles in `DEV.md` and generated
  `benchmark-results/profile.json` as the current baseline. The automated
  release gate is intentionally broader (`completion p95 < 10 ms`, Go RSS
  `<= 50 MiB`, combined Go and worker RSS `<= 150 MiB`) and is not permission to
  consume the margin.
- The reference Go coordinator footprint is approximately `19 MiB` RSS. Avoid
  persistent per-document, per-model, or per-field duplication without measured
  justification.
- Minimize conversions among `string`, `[]byte`, and UTF-16 positions; avoid
  regexes, reflection, interface boxing, temporary maps, and sorting in hot
  loops unless benchmarks prove the cost acceptable.
- Precompute indexes during schema build. Prefer bounded iterative traversal and
  reusable parser pools over recursive or request-local reconstruction.
- Do not claim an optimization without measuring latency, `B/op`, and
  `allocs/op` on the relevant benchmark before and after the change on the same
  host.

## Architecture And Ownership

| Path | Responsibility | Boundary rules |
| --- | --- | --- |
| `cmd/pogo` | CLI flags, project/interpreter discovery, logging, and LSP server wiring | Keep policy and domain logic in `internal/*`; keep protocol stdout clean. |
| `cmd/testclient` | Executable LSP scenario client used by integration, lifecycle, RSS, and release checks | Test tooling only; it must not enter the production dependency graph. |
| `internal/lsp` | LSP 3.16 lifecycle, capabilities, handlers, diagnostics publication, refresh notifications, and navigation | Translate LSP types at the edge. Handlers use `analysis` plus `schema.Cache`, never the Python worker. |
| `internal/analysis` | Tree-Sitter Python parser pool, incremental document state, UTF-16 conversion, model inference, ORM path extraction, and conservative diagnostics | Own syntax-derived facts. Keep analysis bounded by source size/segment count and safe under parser recovery. |
| `internal/schema` | Wire DTOs, strict snapshot validation, immutable graph indexes, relation traversal, source metadata, and atomic cache generations | Build off-path, publish once, and never mutate a published `Graph` or its maps/slices. |
| `internal/python` | Worker extraction and supervision, authenticated local IPC, framing, refresh debounce, cancellation, retries, and schema publication | This is the only Go subsystem that communicates with Python. It must not be called by feature handlers. |
| `src/daemon` | Embedded standard-library-plus-Django introspection worker | Bootstrap trusted Django projects, inspect metadata without invoking project APIs deliberately, emit deterministic bounded JSON, and report failures defensively. |
| `internal/harness` | LSP framing and scenario runner support | Test-only; production packages must not import it. |
| `client/vscode` | First-party TypeScript VS Code extension and one language client per workspace folder | Keep it a thin process/configuration adapter. Django semantics belong in the server. |
| `testdata/sample_django_project` | Controlled Django fixture for metadata, inference, and lifecycle tests | Add focused fixture cases; do not turn the fixture into production code or a runtime dependency. |
| `scripts` | Standard-library compatibility, benchmark, and release inspection harnesses | Keep runs deterministic and fail closed when required evidence is missing. |
| `.github/workflows` | Compatibility matrix, native transports, race, fuzz, performance, cross-build, and release gates | Local verification should mirror the relevant CI job. Do not weaken gates to make a change pass. |

### Data Flow And State Model

1. The editor sends LSP messages over stdio to the Go coordinator.
2. Go incrementally parses open Python documents using the embedded Python
   Tree-Sitter grammar and maintains syntax-derived snapshots.
3. After LSP initialization, `internal/python.Manager` extracts and starts the
   embedded worker in the configured trusted project environment.
4. The worker calls `django.setup()`, reads runtime metadata, and returns a
   protocol-v1 JSON snapshot over a private Unix socket or authenticated
   loopback TCP on Windows.
5. Go enforces frame and schema bounds, decodes the DTO, resolves and validates
   all cross-references, then builds immutable lookup indexes.
6. `schema.Cache` publishes `{graph, generation}` through typed
   `atomic.Pointer` load/store operations. The write side may serialize
   generation assignment; the read side must remain lock-free.
7. LSP features combine a document snapshot with one loaded graph generation.
   A later refresh creates a new graph rather than mutating the old one.

Do not pass mutable worker DTOs directly to handlers. Do not let transport,
process, or Django objects cross into the schema or analysis layers.

### Protocol And Position Contracts

- LSP positions are UTF-16 code-unit positions. Tree-Sitter and schema source
  ranges use UTF-8 byte coordinates. Convert explicitly at layer boundaries and
  test non-ASCII, malformed positions, and incremental edits.
- Keep file URIs and native filesystem paths distinct. Construct and parse URIs
  with `net/url`, convert native paths with `path/filepath`, and test Windows
  drive letters, UNC paths, `localhost`, spaces, non-ASCII, percent encoding,
  and `.exe` suffixes. A cross-build does not prove URI or native path behavior.
- Keep LSP framing and worker framing distinct. Both require bounded reads,
  cancellation/timeout behavior, malformed-input tests, and fuzz coverage.
- The worker schema is a versioned cross-language API. Any field or semantic
  change must update Python serialization, Go DTO/wire decoding, graph
  validation, fixtures, deterministic output tests, and compatibility tests in
  the same change. Increment `schema_version` when compatibility is broken.
- Preserve deterministic ordering in worker JSON and user-facing results. Never
  rely on Python or Go map iteration order.
- Unix endpoints must retain private permissions. Windows endpoints must remain
  loopback-only and token-authenticated. Tokens need cryptographically secure
  randomness and constant-time comparison.

## Engineering Standards

### Go

- Target Go 1.22 or newer as declared in `go.mod`. Production builds must retain
  `grammar_subset,grammar_subset_python`; Darwin native builds also require the
  existing external-linker flag.
- Run `gofmt` on every changed Go file. Keep `go vet` clean. Do not suppress
  diagnostics or add blanket exclusions.
- Use concrete types and narrow interfaces defined by consumers. Avoid `any`,
  reflection, global registries, and package-level mutable state unless the
  boundary genuinely requires them.
- Handle every meaningful error explicitly and wrap it with operation context
  using `%w`. Preserve cancellation and timeout causes. Do not panic for target
  project input, protocol input, parser recovery, or expected lifecycle errors.
- Make ownership and lifetime clear for Tree-Sitter trees, parser-pool values,
  child processes, sockets, files, timers, goroutines, and contexts. Every
  acquisition needs a deterministic release path.
- Give every subprocess exactly one `Wait` and reap owner. Context cancellation
  may request termination, but cleanup must not race duplicate `Kill` or `Wait`
  calls. Normalize platform-specific closed-pipe or process-done errors only
  when they satisfy the same verified terminal-state contract.
- Use `sync/atomic` only with a documented immutable-state invariant. Prefer
  typed `atomic.Pointer[T]` over `unsafe.Pointer` and raw
  `atomic.StorePointer`. Never copy atomics after first use.
- Published schema objects are immutable. If a change is required, construct a
  complete replacement. Do not expose internal mutable maps or slices.
- Keep locks away from schema read paths. When mutable document state requires a
  lock, minimize its scope and never hold it while doing process I/O, network
  I/O, logging callbacks, or potentially blocking work.
- Avoid goroutine-per-item designs, unbounded channels, unbounded recursion, and
  background work without cancellation. Concurrency tests must run under
  `make test-race`.
- Preallocate slices/maps when sizes are known. Avoid hidden heap escapes and
  closures inside hot loops. Reuse immutable results only when ownership remains
  safe and measured behavior improves.
- Keep packages cohesive and dependency direction clear:
  `lsp -> analysis/schema`, `python -> schema`, and command packages -> internal
  packages. Feature code must not depend on `internal/python`.

### Python Worker

- Support Python 3.10 through 3.13 and the tested Django combinations: Django
  4.2 on Python 3.10-3.12, and Django 5.2 on Python 3.10-3.13. Do not use syntax
  or standard-library APIs unavailable on Python 3.10.
- New and substantially modified functions, methods, and data structures must
  use precise PEP 484 type annotations. Use built-in generics supported by
  Python 3.10 and avoid annotations that require importing a third-party typing
  package.
- Keep `from __future__ import annotations` where applicable. Prefer explicit
  return types and narrow optional/union types over implicit `Any`.
- Catch exceptions at the smallest boundary where a safe fallback exists. Catch
  broad `Exception` only around intentionally untrusted Django extension hooks,
  attach context or return a conservative omission, and never catch
  `BaseException`.
- Introspection may execute target imports through `django.setup()`; this is why
  Pogo only supports trusted workspaces. Inspection code itself must not call
  custom manager/queryset methods, run queries, write databases, mutate model
  metadata, modify project files, or perform outbound network activity.
- Read metadata and signatures defensively. Django descriptors, custom fields,
  backends, and user-defined properties can raise. Prefer omission or explicit
  degraded metadata to worker-wide failure when schema consistency permits.
- Bound traversal depth, queue size, output size, frame size, and expensive
  metadata expansion. Handle cyclic model relations and hostile docstrings or
  metadata without recursion blowups.
- JSON stdout is machine protocol output. Route tracebacks, project stdout/stderr,
  and diagnostics through the established capture/log channel. Keep standalone
  dumps deterministic and newline-terminated.
- Use `pathlib`/`os.path` and socket abstractions portably. Account for Windows
  executable paths, case-insensitive comparisons, symlinks, and Unix socket path
  limits.

### TypeScript Extension

- Keep `strict` TypeScript behavior and use `unknown` plus explicit narrowing
  for failures. Avoid `any` and non-null assertions unless an invariant is
  immediately proven.
- Preserve one client per file-backed workspace folder, document ownership
  filtering, remote extension-host execution, trusted-workspace restrictions,
  and orderly asynchronous start/stop behavior.
- Resolve relative configuration against the owning workspace folder. Use Node
  path APIs instead of hardcoded separators. Never log secrets or environment
  values.
- Keep dependencies pinned in `package-lock.json`. Extension dependencies are
  separate from, and never justification for changing, the zero-dependency
  Python worker rule.

### Design Principles

- **Separation of Concerns:** Syntax analysis, runtime extraction, schema
  indexing, transport, and editor adaptation remain separate layers.
- **SOLID:** Keep components single-purpose, depend on narrow contracts, and add
  extension points only at real variation boundaries.
- **DRY:** Centralize protocol constants, path semantics, and ORM traversal rules
  when one authoritative implementation is possible. Do not force unrelated
  contexts through a misleading abstraction merely to remove repeated lines.
- **KISS:** Prefer explicit bounded code and direct data flow over frameworks,
  reflection, metaprogramming, or generalized plugin systems.
- **YAGNI:** Implement current supported Django/LSP behavior only. Do not add
  speculative protocol versions, compatibility shims, caches, or configuration.
- Favor false negatives over false positives. If receiver or path inference is
  unsafe, return no Pogo result rather than inventing a model or emitting a
  misleading diagnostic.

## Change Workflow

### Before Editing

1. Identify the owning layer and read its implementation and tests.
2. Trace cross-language or cross-process contracts before changing a DTO,
   protocol field, lifecycle event, path rule, or position conversion.
3. Establish the applicable correctness, race, compatibility, and performance
   baseline. For hot-path work, run the focused benchmark before editing.
4. Check whether the worktree contains unrelated changes and preserve them.
5. Choose the smallest design that satisfies the requirement without moving
   work into the editor hot path.

Treat staged, unstaged, untracked, and ignored files as user-owned unless they
are explicitly in scope. Never delete, stage, package, or add local editor,
agent, credential, benchmark, or tool state to ignore rules merely to obtain a
clean worktree.

When Git or GitHub operations fail, diagnose remote reachability, transport
authentication, and repository authorization separately before changing
remotes, credentials, repository visibility, tags, or access settings. Prefer
temporary command-scoped transport overrides to persistent user configuration;
make a persistent change only when the user requests it and its scope is clear.

### While Editing

- Keep changes scoped. Do not perform unrelated renames, formatting sweeps,
  dependency upgrades, or generated-artifact churn.
- Add or update tests with the behavior. A bug fix requires a regression test
  that fails for the original defect.
- Keep protocol limits and schema validation fail-closed. New payload data must
  be bounded and validated before indexing.
- Update `README.md` for user-visible behavior/configuration and `DEV.md` for
  development, architecture, benchmark, protocol, or release-workflow changes.
- Never edit benchmark reference values without a real warmed run and recorded
  environment/commit metadata.
- Documentation claims include images, charts, diagrams, badges, captions, and
  alt text. Feature or benchmark changes must update every affected rendered
  asset and capability list. Published measurements must identify one tracked
  source revision and retained machine-readable evidence; never combine values
  from different runs or an undocumented working tree.

### Verification By Change Type

| Change | Minimum focused verification |
| --- | --- |
| Go implementation | `gofmt` changed files, focused `go test` package, then `make test` |
| Concurrency, cache, worker lifecycle, timers, or cancellation | `make test`, `make test-race`, and relevant repeated tests |
| Tree-Sitter, UTF-16, framing, or parser recovery | Focused tests plus the relevant fuzz target; use `make fuzz` before merge when feasible |
| Python worker or schema serialization | Worker unittest suite, `make test`, `make compat`, and `make release-check` |
| Schema DTO or graph semantics | Python golden output, Go wire/graph tests, worker-backed lifecycle scenario, and compatibility matrix |
| LSP feature behavior | Focused `internal/lsp` tests, fixture-backed integration, and `make test` |
| Hot path, graph layout, parser state, or persistent memory | Before/after focused benchmark with `-benchmem`, then `make bench` |
| Unix/Windows endpoint or process handling | Platform-specific tests plus native transport CI expectations; cross-compilation alone is insufficient |
| VS Code extension | `npm ci --include=dev` and `npm run compile` in `client/vscode`; package when release behavior changes |
| Release build graph or embedded worker imports | `make build` and `make release-check` |
| Release workflow, packaging, tag, or distribution | Validate version synchronization and tag ancestry, complete every required CI job, then verify the published manifest, checksums, archive contents, VSIX version, and runnable native binary |
| Documentation, badges, diagrams, or benchmark charts | Markdown/link validation, rendered-asset inspection, claim-to-source review, and exact release/profile evidence where shown |

## Build And Verification Commands

Run commands from the repository root unless a different working directory is
shown.

### Environment And Build

```sh
make fixture-env       # Create .venv-fixture and install pinned fixture Django.
make test-env          # Print fixture versions and run the complete test target.
make build             # Build build/pogo and build/testclient with grammar tags.
```

Use `PYTHON=/path/to/python make fixture-env` to select Python. If the existing
fixture environment uses another Python minor version, run `make clean` before
recreating it. Do not install fixture dependencies into system Python.

### Formatting, Static Checks, And Tests

```sh
gofmt -w path/to/changed.go
make test
make test-race
```

`make test` verifies formatting, runs `go vet`, runs all Go tests with the
required grammar tags, and runs both standard-library Python unittest suites.
For a focused Go package on Linux:

```sh
go test -tags=grammar_subset,grammar_subset_python ./internal/schema
go test -tags=grammar_subset,grammar_subset_python ./internal/analysis
go test -tags=grammar_subset,grammar_subset_python ./internal/lsp
go test -tags=grammar_subset,grammar_subset_python ./internal/python
```

Native Darwin commands also need `-ldflags=-linkmode=external`. Prefer Make
targets when in doubt because they encode this platform requirement.

Run a focused worker suite without creating bytecode:

```sh
PYTHONDONTWRITEBYTECODE=1 .venv-fixture/bin/python -m unittest discover -s src/daemon -p 'test_*.py' -v
```

On Windows, use `.venv-fixture/Scripts/python.exe`.

### Compatibility, Fuzzing, And Performance

```sh
make compat            # Django 4.2 and 5.2 against the selected supported Python.
make fuzz              # Thirty-second campaigns for all registered fuzz targets.
make bench             # Full profile, p95/RSS gates, and CPU/heap profiles.
make release-check     # Production dependency and embedded-import inspection.
```

Use focused benchmarks during iteration. Retain `-benchmem`:

```sh
go test -tags=grammar_subset,grammar_subset_python -run '^$' \
  -bench 'BenchmarkCompletionLatency$' -benchmem ./internal/lsp
go test -tags=grammar_subset,grammar_subset_python -run '^$' \
  -bench 'Benchmark(GraphLookup|CacheSnapshots)$' -benchmem ./internal/schema
```

Inspect full-run profiles with:

```sh
go tool pprof -top benchmark-results/pprof/completion.cpu.prof
go tool pprof -top -alloc_space benchmark-results/pprof/completion.heap.prof
```

Compare benchmark results only on the same host, toolchain, power state, and
representative fixture. Report the command, old/new `ns/op`, `B/op`,
`allocs/op`, p95 where available, and any RSS change.

### Integration And Extension

```sh
build/testclient \
  -scenario testdata/requests/worker-lifecycle.json -- \
  build/pogo \
  -project testdata/sample_django_project \
  -settings sample_project.settings \
  -python "$PWD/.venv-fixture/bin/python"
```

```sh
cd client/vscode
npm ci --include=dev
npm run compile
```

Do not mix logs with either LSP stdout or worker JSON stdout. Use `-log-file`
when manually running the server.

## Testing Expectations

- Test public behavior and boundary invariants, not only helper internals.
- Cover empty, malformed, oversized, stale, cancelled, cyclic, non-ASCII,
  platform-specific, and unavailable-worker cases where relevant.
- Go tests should be deterministic, race-safe, and table-driven when cases share
  a contract. Avoid sleeps as synchronization; use channels, contexts, hooks, or
  bounded polling when testing real subprocess state.
- Triage CI from the exact workflow, matrix cell, failed step, and first
  actionable log before changing code or rerunning a job. A passing rerun alone
  does not prove infrastructure noise: reproduce or stress the focused test and
  retain native-platform evidence for process, endpoint, URI, and path failures.
- Schema/cache tests must prove readers see one complete generation, never a
  mixture. Include concurrent reads and swaps for publication changes.
- Worker tests must verify deterministic JSON, schema bounds, graceful errors,
  stdout/stderr isolation, child cleanup, and supported Django behavior.
- LSP tests must use UTF-16 positions and assert exact ranges, stable diagnostic
  codes, conservative omissions, lifecycle correctness, and operation with the
  worker stopped.
- Performance-sensitive code needs representative scale cases, including dense
  models, 10,000-model graphs, recursive relations, 100 KiB documents, and high
  open-document counts as applicable.
- A cross-build verifies compilation only. Endpoint, process, permissions, and
  cleanup changes require native Linux, macOS, and/or Windows evidence.

## Security And Trust Boundary

- Pogo executes `django.setup()` and therefore imports target project code. It
  is not a sandbox and must only run against trusted workspaces.
- Local IPC authentication prevents unrelated local clients from connecting; it
  does not make untrusted Django code safe.
- Never expose the worker endpoint beyond loopback, weaken token entropy or
  comparison, broaden Unix permissions, log authentication material, or persist
  tokens beyond the worker session.
- Treat worker frames, schema payloads, LSP messages, file URIs, dotenv values,
  project metadata, and docstrings as untrusted input. Validate sizes, versions,
  ranges, absolute paths, and references before use.
- Avoid shell interpretation. Pass subprocess arguments as arrays, preserve
  platform quoting semantics, and never derive executable code from project
  metadata.
- Do not place credentials in workspace settings, fixtures, logs, diagnostics,
  benchmark artifacts, or tests.

## Architectural Guardrails: DO NOT

- DO NOT invoke, message, wait for, or restart Python from any editor hot path.
- DO NOT add a read mutex, blocking channel, condition variable, or serialized
  queue in front of schema cache reads.
- DO NOT mutate a published `schema.Graph`, cache state, model index, map, slice,
  or object reachable from a published generation.
- DO NOT publish a schema before complete frame validation, DTO validation,
  cross-reference resolution, and index construction.
- DO NOT clear or downgrade the last valid generation because a refresh failed,
  crashed, timed out, was cancelled, or became stale.
- DO NOT allow a superseded worker session to publish after a newer save or
  refresh request.
- DO NOT import non-standard packages other than Django from `src/daemon` or add
  Pogo packages to the target project's Python environment.
- DO NOT call custom managers, querysets, descriptors, model methods, or database
  operations while introspecting metadata.
- DO NOT write logs, tracebacks, banners, or project output to LSP stdout or
  worker protocol stdout.
- DO NOT remove frame limits, document-size limits, traversal limits, restart
  limits, timeouts, authentication, or schema-version checks.
- DO NOT hardcode `/tmp`, `/home`, drive letters, slash direction, executable
  suffixes, Unix-only sockets, or case-sensitive path assumptions.
- DO NOT use unsafe pointer operations when typed atomics express the invariant.
- DO NOT introduce unbounded goroutines, recursion, queues, maps, retries,
  filesystem walks, result sets, or metadata expansion.
- DO NOT add broad speculative abstractions, plugin systems, compatibility
  shims, or dependencies without a current concrete requirement.
- DO NOT expand Pogo into a general Python type checker. Preserve coexistence
  with Pyright, BasedPyright, Ruff, and editor-native tooling.
- DO NOT report diagnostics when model/receiver inference is ambiguous or the
  parse is recovered in a way that makes the path unsafe.
- DO NOT weaken tests, compatibility coverage, release checks, performance
  gates, security controls, or supported-platform behavior to land a change.
- DO NOT update golden schema output, benchmark baselines, generated artifacts,
  or lockfiles unless the change intentionally requires it and the source change
  is included.
- DO NOT leave child processes, goroutines, timers, parsers, trees, sockets,
  temporary worker directories, token files, or open descriptors behind after
  shutdown or failed startup.

## Definition Of Done

A change is complete only when:

- The owning boundary remains clear and editor hot paths remain Python-free.
- Correctness and failure behavior are covered by focused regression tests.
- Relevant formatting, static checks, tests, race, compatibility, fuzz,
  integration, performance, and release checks have passed or any unavailable
  platform verification is explicitly identified.
- Performance-sensitive changes include before/after allocation and latency
  evidence and remain within both engineering targets and release gates.
- Cross-language contracts, limits, deterministic ordering, and versioning are
  synchronized.
- The last valid schema survives every newly introduced failure path.
- User-facing and developer-facing documentation is updated where behavior,
  configuration, architecture, support, or workflows changed.
- Tagged releases are complete only after the remote release and exact asset
  manifest are verified; a green build without published, downloadable,
  checksum-valid artifacts is not a completed release.
- No unrelated changes, secrets, local environments, build outputs, benchmark
  artifacts, or temporary runtime files are included.
