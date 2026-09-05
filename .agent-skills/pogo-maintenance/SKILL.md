---
name: pogo-maintenance
description: Use for Pogo CI incidents, cross-platform process or path failures, release or tag operations, benchmark publication, rendered-asset verification, GitHub access incidents, and maintainer handoffs. Do not use for ordinary code changes, installation, or architecture explanation without one of these operational tasks.
---

# Pogo Maintenance

Apply Pogo's existing engineering contract to a concrete maintenance task. This
skill is a decision workflow, not a replacement for repository documentation.

## Source Precedence

1. Read `AGENTS.md` as the normative contract.
2. Read the affected source and tests.
3. Use `.github/workflows/*.yml`, `Makefile`, and `scripts/` as executable truth.
4. Use `DEV.md` for development, compatibility, performance, and release detail.
5. Use `README.md` for supported public behavior.
6. Use `MAINTENANCE.md` for operational procedures and handoff evidence.
7. Never treat an agent tool's local scratch or plan directory (for example `.codera/plans/`) as current architecture without source proof.

When sources conflict, determine which side violates the intended contract. Fix
the drift when it is in scope; otherwise report it explicitly. Do not silently
choose the most convenient instruction.

## Start Safely

1. Inspect branch, status, diff, recent commits, and remote.
2. Record unrelated staged, unstaged, untracked, and task-relevant ignored state.
3. Classify the task by behavior and owning layer.
4. Select the union of every applicable verification row in `AGENTS.md`.
5. Read the exact implementation and tests before editing.

Never clean, stash, stage, ignore, or package unrelated local state.

## CI Decision Procedure

1. Inspect the exact run with `gh run view RUN_ID` and `--log-failed`.
2. Record workflow, commit, matrix cell, failed step, and first actionable error.
3. Open the corresponding workflow job.
4. Reproduce the exact command and environment where the native platform is
   available.
5. Classify the failure as product, test, portability, dependency/environment,
   performance, or infrastructure.
6. Add or preserve a focused regression test when repository behavior is wrong.
7. Stress race, lifecycle, timer, or ordering failures repeatedly.
8. Run focused checks, then every required aggregate gate.
9. Stage and commit only intended files, inspect the refspec and commits to be
   pushed, then monitor the entire replacement run.

Do not use a passing rerun as the sole evidence of flakiness. Do not weaken a
test, platform matrix, release check, or performance gate to obtain green CI.

## Release Decision Procedure

1. Confirm `vX.Y.Z`, server version, extension version, lockfile version, and
   user-facing asset names agree.
2. Confirm the tag commit is already on `origin/main`.
3. Confirm all prerequisite CI jobs pass for the tagged commit.
4. Confirm release binaries use required grammar tags and pass release
   inspection.
5. Confirm the binary reports the expected version and the VSIX packages.
6. Inspect the remote release and exact asset manifest.
7. Download into an empty directory and verify checksums, archive layout, VSIX
   version, and a native executable outside the checkout.

Do not delete, move, force-update, or recreate tags or releases without explicit
approval. Distinguish tag existence from annotated or cryptographically signed
tag status.

## Benchmark And Documentation Procedure

1. Capture a fresh clean tracked revision or exported commit with `make bench`
   when claims change; label dirty-tree exploratory results as uncommitted.
2. Keep one profile's commit, environment, timing scope, allocations, and RSS
   together.
3. Update README prose, tables, badges, diagrams, alt text, and rendered images.
4. Inspect binary images directly and preserve their source or generator.
5. Scope claims precisely: handler, parse plus handler, in-process, fixture
   refresh, sampled idle RSS, or consumer-observed behavior.
6. Lint Markdown, verify anchors, and compare release/profile claims with remote
   or machine-readable evidence.

## GitHub Access Procedure

1. Separate network reachability, transport authentication, and repository
   authorization.
2. Inspect remote URLs and test `git ls-remote`.
3. For SSH, inspect offered identities and test the GitHub handshake.
4. For authorization, inspect `gh auth status`, API identity, visibility, and
   viewer permission.
5. Prefer command-scoped HTTPS credentials for temporary recovery.
6. Change persistent SSH/Git configuration only when requested and validate the
   original failing command afterward.

Never expose tokens, private keys, or credential-bearing URLs.

## Report

Use the handoff template in `MAINTENANCE.md`. Include root cause, exact affected
job or release stage, files changed, focused and aggregate checks, native
evidence, benchmark revision, remote URLs, unavailable checks, residual risk,
and preserved unrelated state.
