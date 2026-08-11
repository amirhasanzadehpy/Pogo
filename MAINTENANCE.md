# Pogo Maintenance Playbook

This playbook turns Pogo's engineering contract into repeatable operational
procedures. It is written for maintainers and coding agents handling CI,
cross-platform failures, releases, benchmarks, documentation, or GitHub access.

`AGENTS.md` is the normative contract. This file explains how to gather evidence
and complete maintenance work without weakening that contract. When executable
configuration and prose differ, determine which side violates the intended
contract. Fix the implementation when it violates architecture, security, or
supported behavior; fix the documentation when it is stale. If either repair is
outside the requested scope, report the drift explicitly.

## Authoritative Sources

Authority depends on the question:

1. `AGENTS.md` for non-negotiable architecture, security, quality, and
   verification requirements.
2. Source code and tests for current behavior.
3. `.github/workflows/*.yml` for the jobs GitHub actually runs.
4. `Makefile` and `scripts/` for local build, test, benchmark, compatibility,
   and release gates.
5. `DEV.md` for development workflows and measured reference profiles.
6. `README.md` for supported public behavior and installation.
7. This playbook for operational sequencing and incident evidence.

Executable files describe what currently happens; they do not override the
contract merely because CI runs them. Public documentation describes supported
behavior; an accidental source regression does not silently redefine support.

Files under `.codera/plans/` are historical plans, not current architecture or
maintenance instructions. Verify every useful statement from a plan against
current source, tests, and `AGENTS.md` before acting on it.

## First Ten Minutes

Before editing or rerunning anything:

```sh
git status --short --branch
git diff --check
git diff --cached --check
git log --oneline -10
git remote -v
```

Record:

- Current commit and branch.
- Staged, unstaged, untracked, and task-relevant ignored state. Use
  `git check-ignore -v PATH` when an expected path is absent; do not enumerate
  unrelated ignored files that may contain credentials.
- The exact user request and files that are intentionally in scope.
- The failing run, release, tag, or benchmark revision.
- Which checks are required by every applicable row in `AGENTS.md` under
  **Verification By Change Type**.

Untracked does not mean disposable. Local `.codera/`, editor settings,
generated environments, benchmark output, credentials, and user tools may be
valuable even when they are not committed. Stage exact paths and never use a
cleanup command to make the tree look clean.

## CI Triage

### Inspect Before Rerunning

Start with the exact run:

```sh
gh run view RUN_ID --repo amirhasanzadehpy/Pogo
gh run view RUN_ID --repo amirhasanzadehpy/Pogo --log-failed
```

Capture:

- Workflow name and commit SHA.
- Job and matrix values: OS, Python, Django, architecture, and transport.
- Failed step and first actionable error.
- Whether later jobs failed independently or were skipped because of `needs`.
- Whether the same commit passed another platform or compatibility cell.

Open the matching job in `.github/workflows/ci.yml` or
`.github/workflows/stability.yml`. Reproduce its command, build tags,
constraints, environment, and platform as closely as possible.

### Classify The Failure

Use one primary classification:

| Class | Evidence |
| --- | --- |
| Product regression | Supported input or lifecycle behavior is wrong in source |
| Test defect | Assertion, fixture, synchronization, or cleanup does not express the contract |
| Platform assumption | Paths, URIs, executable suffixes, sockets, process errors, or permissions assume another OS |
| Dependency/environment drift | Toolchain, action runtime, Python/Django version, or hosted image changed behavior |
| Performance regression | Measured gate input regressed, not merely the summary command |
| GitHub infrastructure | Service/network/runner failure with no repository-controlled failing behavior |

Do not classify a failure as flaky merely because a rerun passes. Stress the
focused behavior, inspect race output, and determine which ordering or platform
state made the original result possible.

### Validate A Fix

Run focused verification first, then the aggregate gates required by
`AGENTS.md`. Useful patterns include:

```sh
go test -race -tags=grammar_subset,grammar_subset_python \
  ./internal/harness -run '^TestName$' -count=100

make test
make test-race
```

On macOS, direct Go commands that include the Tree-Sitter graph need
`-ldflags=-linkmode=external`. Prefer Make targets when they already encode the
platform flags.

A Windows cross-compile proves syntax, imports, and build constraints only. It
does not prove process termination, authenticated loopback transport, file URI
round trips, permissions, or filesystem semantics. Require native Windows CI
for those behaviors.

After pushing, monitor the replacement run through its final conclusion, not
only until the originally failing job turns green:

```sh
gh run list --repo amirhasanzadehpy/Pogo --branch main --limit 5
gh run watch RUN_ID --repo amirhasanzadehpy/Pogo --exit-status
```

## Cross-Platform Lessons

### Native Paths And File URIs

Native paths and file URIs are separate representations:

- Use `path/filepath` for native path joining, cleaning, absoluteness, and
  executable names.
- Use `net/url` for URI parsing and construction.
- Do not build `file://` values through string concatenation.
- Windows test binaries require `.exe`.
- A Windows drive path is not a Unix absolute path.
- Cover drive letters, UNC paths, `file://localhost`, spaces, percent-encoded
  separators, non-ASCII names, malformed escapes, and invalid schemes.
- Convert at protocol boundaries and keep internal ownership explicit.

When a fixture stores paths, derive them from a temporary/project root instead
of hardcoding `/tmp`, `/home`, or drive letters. Compare normalized native paths
only after the protocol conversion has been tested independently.

### Process Ownership And EOF

`exec.CommandContext` already arranges termination when its context is
cancelled. Adding an independent cleanup `Process.Kill` can race that watcher;
on Windows the duplicate termination may surface as
`TerminateProcess: Access is denied` even though the process is exiting.

Maintain these invariants:

- One goroutine/path owns `Wait` and reaping.
- Cancellation requests shutdown once.
- Cleanup closes owned pipes and waits for the one reaper with a bound.
- An explicit `expect_eof` may normalize a wrapped `os.ErrClosed` when the child
  exit code is still verified.
- A message expectation must not normalize closed-pipe errors into success.
- Timeout tests must prove the process is reaped and cleanup adds no secondary
  failure.

Use repeated race-enabled lifecycle tests. Avoid sleeps as synchronization;
publish state or close channels only after the event a test is meant to observe
has actually happened.

### Warnings Versus Failures

GitHub annotations about an action's deprecated Node runtime may be important
maintenance debt without being the cause of a failed job. Identify the step's
exit code and first actionable error before changing action versions. Upgrade
actions in a dedicated, validated change rather than mixing them into an
unrelated portability fix.

## Release Operations

### Preflight

Publishing is tag-driven. Before creating `vX.Y.Z`, fetch the authoritative
remote refs, then verify:

```sh
git fetch --prune origin main
git fetch --tags origin
git merge-base --is-ancestor HEAD origin/main
git ls-remote --exit-code --tags origin \
  "refs/tags/vX.Y.Z" "refs/tags/vX.Y.Z^{}"
gh release view vX.Y.Z --repo amirhasanzadehpy/Pogo
```

For a new version, the last two commands should show that neither the exact tag
nor release exists. Their nonzero status is expected only after the output is
inspected and absence is established. Then verify:

1. The working tree contains no unintended staged or tracked changes.
2. `internal/lsp.ServerVersion` is `X.Y.Z`.
3. `client/vscode/package.json` and the root entries in
   `client/vscode/package-lock.json` are `X.Y.Z`; the workflow checks all three
   package-version fields.
4. User-facing installation text names the intended version and assets.
5. The release commit is pushed to `main` and is an ancestor of `origin/main`.
6. `make build`, `make test`, `make test-race`, `make bench`, and
   `make release-check` pass as required by the change.

Inspect surrounding tags and releases before creating anything:

```sh
git tag --list --sort=-version:refname
gh release list --repo amirhasanzadehpy/Pogo --limit 20
gh api --paginate repos/amirhasanzadehpy/Pogo/tags --jq '.[].name'
```

Do not move, force-update, delete, or recreate a tag or release without explicit
approval. `gh release create --verify-tag` proves that a remote tag exists; it
does not prove that the tag is cryptographically signed.

### Workflow Dependency Chain

The release job must wait for all required compatibility, race, native
transport, cross-build, and performance jobs. It must:

- Reject tags that do not match `vX.Y.Z`.
- Reject a tag mismatch with the extension package version and reject a server
  mismatch by executing the built binary.
- Reject a tag commit that is not already on `main`.
- Cross-build with `CGO_ENABLED=0` and the required grammar tags.
- Inspect every built release binary with `scripts/check_release.py`.
- Execute the Linux binary and verify its reported version.
- Package the VS Code extension with the same six staged native binaries.
- Inspect the VSIX for target completeness, executable modes, version agreement,
  and byte identity with the standalone binaries.
- Generate checksums after temporary staging directories are removed.
- Publish only after every prior step succeeds.

### Published Manifest

Derive the expected manifest from `.github/workflows/ci.yml`; do not trust a
copied list when the target matrix changes. The current shape is:

```text
checksums.txt
pogo-X.Y.Z.vsix
pogo-vX.Y.Z-darwin-amd64.tar.gz
pogo-vX.Y.Z-darwin-arm64.tar.gz
pogo-vX.Y.Z-linux-amd64.tar.gz
pogo-vX.Y.Z-linux-arm64.tar.gz
pogo-vX.Y.Z-windows-amd64.zip
pogo-vX.Y.Z-windows-arm64.zip
```

After publication:

```sh
gh release view vX.Y.Z --repo amirhasanzadehpy/Pogo \
  --json tagName,url,isDraft,isPrerelease,assets

gh release download vX.Y.Z --repo amirhasanzadehpy/Pogo \
  --dir /path/to/empty/verification-directory
```

Verify the exact filenames and count, SHA-256 checksums, one-file archive
layout, executable name, all six VSIX binaries, VSIX version, and `pogo -version`
from outside the source checkout. Also confirm the repository visibility did
not change.

A successful build with an empty GitHub Releases page is not a release. A
release record without downloadable, checksum-valid consumer artifacts is also
not complete.

### Failed Tagged Runs

Distinguish a release-job defect from a failed prerequisite. Inspect the exact
job before changing tags. A rerun of an unchanged tagged commit may recover a
proven infrastructure or nondeterministic failure, but it will not include a
later fix from `main`. State that distinction explicitly when deciding whether
the already-tagged product is safe to publish.

## Benchmark Evidence

Published profiles must come from a clean checkout or an export of one known
tracked revision. Untracked Go, Python, fixture, or script files can affect a
run even when `git diff` is empty. For exploratory working-tree measurements,
label the profile as uncommitted and retain the exact staged and unstaged diff
plus the contents or hashes of relevant untracked inputs instead of attributing
it to `HEAD`.

For a publishable run:

```sh
git status --short --branch
git rev-parse HEAD
make fixture-env
make bench
```

Treat `benchmark-results/profile.json` as the machine-readable evidence for that
run. Its `python` field identifies the interpreter running `scripts/bench.py`,
not necessarily the fixture worker interpreter, and it does not currently
record the fixture Django version. Record the fixture Python and Django versions
alongside the profile, plus the commit, OS, architecture, Go, logical CPU count,
sample count, and exact commands. Report operation scope with every number:

- Handler-only is not parse-plus-handler.
- In-process is not editor-observed JSON-RPC latency.
- A graph lookup benchmark may perform several indexed lookups.
- Parallel `ns/op` is aggregate throughput, not single-request latency.
- A three-sample p95 is the maximum of three observations, not a stable tail
  distribution.
- Sampled idle RSS is not startup peak or process-lifetime peak memory.

Compare optimization results only on the same host, toolchain, power state, and
representative fixture. Retain `ns/op`, `B/op`, `allocs/op`, p95, and RSS where
applicable. Never compare Pogo against another tool without an equivalent
workload and retained raw evidence.

## README And Rendered Assets

Rendered images are factual documentation. Review the PNG itself, not only the
Markdown that references it. Whenever capabilities, benchmark names, values,
budgets, versions, commits, dates, or release assets change:

1. Search README prose, tables, badges, alt text, diagrams, and images.
2. Regenerate charts from one retained profile.
3. Ensure chart labels describe the measured scope.
4. Remove deleted features from every capability list and visual.
5. Validate image dimensions and format.
6. Render/lint Markdown and verify local anchors and remote release links.
7. Preserve or add the chart source/generator so another maintainer can update
   the asset without reverse-engineering a binary PNG.

Do not label an old profile `current source`. Prefer `reference profile` plus an
explicit revision when documentation-only or later feature commits exist.

The README benchmark source is `assets/pogo-benchmarks.svg`. Regenerate its PNG
with:

```sh
rsvg-convert --version
rsvg-convert -w 1600 -h 900 \
  assets/pogo-benchmarks.svg -o assets/pogo-benchmarks.png
```

Record the librsvg and font-library environment with published image changes;
raster output can vary even when the SVG source is unchanged.

## Git And GitHub Access

An authentication error does not by itself mean repository access was removed.
Diagnose three layers separately:

1. Network reachability.
2. Git transport authentication: SSH key or HTTPS credential helper.
3. GitHub repository authorization and visibility.

Useful checks:

```sh
git remote -v
git status --short --branch
git ls-remote origin HEAD
ssh-add -l
ssh -T git@github.com
gh auth status
gh api user --jq '{login,id}'
gh repo view amirhasanzadehpy/Pogo \
  --json visibility,viewerPermission,url
```

`Permission denied (publickey)` is an SSH transport failure. Confirm which key
is registered and which key SSH actually offers. Non-default key filenames need
an explicit host rule or an agent identity. If port 22 is blocked, GitHub
supports `ssh.github.com` on port 443. Verify the server fingerprint against
GitHub's published fingerprints before adding it to `known_hosts`.

After an exact push is approved, HTTPS can be used for that operation without
changing repository or global Git configuration. Replace the refspec with the
requested branch or tag:

```sh
git -c credential.helper= \
  -c credential.helper='!gh auth git-credential' \
  push https://github.com/amirhasanzadehpy/Pogo.git LOCAL_REF:REMOTE_REF
```

Do not print tokens, embed credentials in remotes, change repository visibility,
or rewrite user SSH/Git configuration as a speculative fix. If a persistent
configuration change is necessary, explain its host scope, key selection, and
network reason, then validate the user's original failing command.

Network/API calls may time out intermittently. Retry a read-only check before
concluding that credentials or access changed. Keep transport failure evidence
separate from repository permission evidence.

## Clean-Room And Consumer Verification

Source-tree success can hide dependencies on local files, caches, fixtures, or
working directories. For release-sensitive changes:

1. Export a tracked commit into a temporary directory.
2. Use fresh Go, Python, and pip caches.
3. Build and run tests from the export.
4. Copy the sample project outside the export.
5. Run worker lifecycle and shutdown from an unrelated working directory.
6. Inspect production binaries for test or source dependencies.
7. Confirm shutdown leaves no child, socket, token, or runtime directory.
8. Download and execute the published artifact outside the repository.

See `DEV.md#release-inspection` for the existing build, test, fresh-cache, and
external-project checks. This playbook additionally requires published-download,
checksum, archive, and VSIX verification.

## Handoff Template

Finish maintenance work with evidence another agent can continue from:

```text
Classification:
Root cause:
Affected workflow/job/matrix:
Authoritative files consulted:
Files changed:
Focused reproduction:
Focused verification:
Aggregate verification:
Native-platform evidence:
Benchmark evidence and revision:
Release/tag/assets verified:
Checks not run and why:
Residual risk:
Commit/run/release URLs:
Unrelated local state preserved:
```

Do not report intent as completion. A check is complete only after its output is
observed, and a remote action is complete only after GitHub reports the expected
commit, run, release, or asset state.
