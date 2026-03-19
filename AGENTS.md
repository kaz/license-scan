# AGENTS.md

This file is a handoff note for future LLM agents working on `license-scan`.

## Project Summary

`license-scan` is a Go CLI that:

- scans one or more local directories and/or Git repositories
- finds `package.json` and `go.mod`
- extracts direct dependency declarations
- resolves licenses via `deps.dev` gRPC API v3
- outputs results as `table` or `csv`

Main implementation lives in `main.go`.

## Current CLI Behavior

Current flags:

- `--format table|csv`
- `--insecure-ignore-host-key`
- `--fallback-to-default`

Positional arguments:

- one or more `TARGET`s
- a target can be a local directory or a Git repository URL

SSH behavior:

- SSH Git URLs use `ssh-agent` by default
- `--insecure-ignore-host-key` disables host key verification for SSH URLs only

## Current Dependency Extraction Rules

### `package.json`

Extracts only:

- `dependencies`
- `devDependencies`

Does not read:

- `peerDependencies`
- `optionalDependencies`
- lockfiles

### `go.mod`

Extracts only:

- direct `require` entries

Ignores:

- indirect dependencies
- `go.sum`

## Current deps.dev Resolution Rules

deps.dev client:

- package: `deps.dev/api/v3`
- gRPC connection: `grpc.NewClient(...)`
- endpoint: `dns:///api.deps.dev:443`

Lookup flow:

1. Try version-specific lookup first if a concrete version is available.
2. If `--fallback-to-default` is enabled:
   - fallback when requested version is `NotFound`
   - fallback when dependency spec cannot be normalized into a single version
3. Fallback logic:
   - call `GetPackage`
   - pick package default version (`isDefault`)
   - call `GetVersion` for that version

## npm Version Normalization

Implemented in `normalizeNPMVersion()`.

Current intent:

- normalize common range-like specs to one version for deps.dev lookup
- examples:
  - `^1.2.3` -> `1.2.3`
  - `~1.2.3` -> `1.2.3`
  - `>=1.2.3 <2.0.0` -> `1.2.3`
  - `workspace:^1.2.3` -> `1.2.3`

Current unresolved cases include:

- `file:`
- `link:`
- `git+`
- `github:`
- raw URLs
- wildcard specs such as `*`, `x`, `X`

If unresolved and `--fallback-to-default` is off:

- output license becomes `unresolved-version`

If unresolved and `--fallback-to-default` is on:

- tool still tries package default version

## Current Output Schema

Both `csv` and `table` currently output:

- `source`
- `manifest`
- `dependency_type`
- `library`
- `version`
- `license`

Meaning of `version`:

- this is the version actually queried against deps.dev
- if fallback-to-default was used, it is suffixed with ` *`

Examples:

- normal lookup: `1.2.3`
- fallback lookup: `1.2.3 *`

`source` format:

- local directory: directory name
- Git repository: host-aware identifier such as `github.com/owner/repo`

## Current Error Handling

### Manifest read/parse errors

- warning to `stderr`
- continue processing

### deps.dev lookup errors

- per-dependency best-effort behavior
- `NotFound` -> `not-found`
- other lookup failures -> `lookup-error`
- continue processing

### Git clone failure

- this was intentionally changed to warning-and-continue
- only clone failures are skipped this way
- other fatal input errors still abort the run

### Fatal errors that still abort

Examples:

- target path does not exist
- local target is not a directory
- deps.dev client initialization fails
- invalid argument combinations

## Progress / stderr Behavior

There is a single-line status renderer in `main.go`.

Behavior:

- writes progress to `stderr`
- rewrites one line in TTY mode
- warnings are printed as normal lines
- progress currently includes:
  - scanning target
  - cloning repository
  - scanning manifests count
  - resolving licenses count
  - rendering output

When changing warning or exit behavior, check interaction with `progress.Clear()` and `progress.Warnf()`.

## Important Historical Decisions

These are intentional and should not be “cleaned up” without checking the expected behavior.

1. Remote repositories are cloned in memory using `go-git` + `memfs`.
2. `git` shell command is not used.
3. SSH auth is default for SSH URLs; there is no `--use-ssh-agent` flag anymore.
4. `deps.dev/api/v3alpha` was replaced by `deps.dev/api/v3`.
5. `grpc.Dial` was replaced by `grpc.NewClient`.
6. deps.dev lookup failures no longer abort the whole run.
7. Git clone failures no longer abort the whole run.
8. `version` output column was added to show the actually queried version.
9. Fallback marker changed from `(*)` to ` *`.

## Files Worth Checking Before Changes

- `main.go`
- `README.md`
- `go.mod`

## Known Documentation Drift

Before changing behavior, check whether README still matches implementation.

At the time of writing, one likely area to verify is clone failure behavior:

- implementation skips failed Git clones with a warning
- if README says clone failure is fatal, README needs correction

Also verify README whenever changing:

- output columns
- fallback behavior
- error semantics
- SSH behavior

## Suggested Editing Strategy

If you modify dependency resolution or output:

1. Update `main.go`
2. Re-run formatting/tests
3. Verify both `csv` and `table` output
4. Update `README.md`
5. Re-check special values:
   - `unknown`
   - `not-found`
   - `unresolved-version`
   - `lookup-error`

## Quick Mental Model

Think of the tool as four stages:

1. Collect targets
2. Scan manifests
3. Resolve licenses
4. Render rows

Most feature work falls into one of these buckets.
