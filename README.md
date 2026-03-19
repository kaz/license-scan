# license-scan

`license-scan` is a Go CLI tool that scans one or more local directories or Git repositories, extracts dependencies from `package.json` and `go.mod`, and looks up dependency licenses using the [deps.dev API v3](https://docs.deps.dev/api/v3/).

It is designed for quick license inventory generation across mixed Go and Node.js codebases.

## Features

- Scan one or more local directories
- Scan one or more remote Git repositories
- Process multiple targets concurrently
- Clone Git repositories in memory without creating temporary working directories
- Detect:
  - `package.json`
  - `go.mod`
- Extract:
  - `dependencies` and `devDependencies` from `package.json`
  - direct `require` dependencies from `go.mod` only
- Resolve dependency licenses through `deps.dev`
- Output results as either:
  - pretty terminal table
  - CSV
- Show live progress on `stderr` with in-place status updates
- Continue even if some deps.dev lookups fail

## Supported Inputs

Each positional argument can be one of the following:

- A local directory path
- An HTTPS Git repository URL
- An SSH Git repository URL such as `git@github.com:owner/repo.git`
- An `ssh://` Git repository URL

You can mix multiple targets in the same command.

## What Gets Scanned

### `package.json`

The tool reads:

- `dependencies`
- `devDependencies`

It does not currently read:

- `peerDependencies`
- `optionalDependencies`
- `bundleDependencies`
- lockfiles such as `package-lock.json`, `pnpm-lock.yaml`, or `yarn.lock`

### `go.mod`

The tool reads only direct `require` entries.

It ignores:

- indirect dependencies
- other files such as `go.sum`

## Installation

Install with:

```bash
go install github.com/kaz/license-scan@latest
```

After installation, make sure your Go binary directory is in `PATH`.

For example:

```bash
export PATH="$(go env GOPATH)/bin:$PATH"
```

You can then run:

```bash
license-scan <target> [<target>...]
```

## Usage

```bash
license-scan [--format table|csv] [--parallelism N] [--lookup-parallelism N] [--fallback-to-default] [--insecure-ignore-host-key] <target> [<target>...]
```

### Flags

- `--format`
  - Output format.
  - Supported values: `table`, `csv`
  - Default: `table`

- `--parallelism`
  - Maximum number of targets processed concurrently.
  - Default: `4`

- `--lookup-parallelism`
  - Maximum number of concurrent deps.dev license lookups.
  - Default: `20`

- `--insecure-ignore-host-key`
  - Disables SSH host key verification for SSH Git repository access.
  - This also prevents reading `known_hosts`.
  - Only valid for SSH repository URLs.
  - This reduces security and should only be used when you understand the risk.

- `--fallback-to-default`
  - If the requested package version is not found in deps.dev, retry using the package default version.
  - This fallback is also used when the tool cannot normalize a dependency specification into a single version.

## Examples

### Scan the current directory

```bash
license-scan .
```

### Scan multiple local directories

```bash
license-scan ./service-a ./service-b
```

### Scan a public Git repository

```bash
license-scan https://github.com/golang/example.git
```

### Scan multiple targets together

```bash
license-scan . ./frontend https://github.com/golang/example.git
```

### Export CSV

```bash
license-scan --format csv . > out.csv
```

### Use SSH and skip host key verification

```bash
license-scan --insecure-ignore-host-key git@github.com:owner/repo.git
```

## Output Formats

## Table Output

Default output format.

Columns:

- `source`
- `manifest`
- `dependency_type`
- `library`
- `version`
- `license`

Example:

```text
┌──────────────────────────┬──────────────────────┬─────────────────┬──────────────────────────────┬───────────┬──────────────┐
│ SOURCE                   │ MANIFEST             │ DEPENDENCY TYPE │ LIBRARY                      │ VERSION   │ LICENSE      │
├──────────────────────────┼──────────────────────┼─────────────────┼──────────────────────────────┼───────────┼──────────────┤
│ github.com/golang/example│ ragserver/go.mod     │ require         │ google.golang.org/api        │ v0.194.0  │ BSD-3-Clause │
└──────────────────────────┴──────────────────────┴─────────────────┴──────────────────────────────┴───────────┴──────────────┘
```

## CSV Output

CSV includes the same information and is easier to consume from scripts or spreadsheets.

Header:

```csv
source,manifest,dependency_type,library,version,license
```

Example:

```csv
source,manifest,dependency_type,library,version,license
github.com/golang/example,go.mod,require,golang.org/x/tools,v0.33.0,BSD-3-Clause
```

## Column Definitions

- `source`
  - A normalized identifier for the target being scanned.
  - Local directories use the directory name.
  - Git repositories use a host-aware identifier such as `github.com/owner/repo`.

- `manifest`
  - Relative path to the dependency source file within the scanned target.

- `dependency_type`
  - One of:
    - `dependencies`
    - `devDependencies`
    - `require`

- `library`
  - Package or module name.

- `version`
  - The version actually queried against deps.dev.
  - If fallback-to-default was used, the version is suffixed with ` *`.

- `license`
  - License value returned or derived from deps.dev.

## License Resolution Behavior

The tool queries the [deps.dev API v3](https://docs.deps.dev/api/v3/) using gRPC.

### Go dependencies

For Go modules, the tool looks up the exact module version from `go.mod`.

### npm dependencies

For npm dependencies, deps.dev expects a concrete version. Many `package.json` files use version ranges, so the tool normalizes some common forms before lookup.

Examples:

- `^1.2.3` -> `1.2.3`
- `~1.2.3` -> `1.2.3`
- `>=1.2.3 <2.0.0` -> `1.2.3`
- `workspace:^1.2.3` -> `1.2.3`

### npm versions that are not resolved

Some npm dependency specs are intentionally not resolved because they do not clearly map to a single published version.

Examples:

- `file:../lib`
- `link:../lib`
- `git+https://...`
- `github:user/repo`
- URL-based dependencies
- wildcard versions such as `*`, `x`, or `X`

### Optional fallback to the default package version

When `--fallback-to-default` is enabled, the tool keeps its normal version-specific lookup behavior first whenever a concrete version is available.

It falls back to the package default version when either of the following is true:

1. deps.dev cannot find the requested version
2. the tool cannot normalize the dependency specification to a single concrete version

In those cases it will:

1. query package metadata with `GetPackage`
2. locate the package default version
3. query that default version with `GetVersion`

If that fallback succeeds, the returned license is used in the output.

## Special License Values

The `license` column may contain special values:

- `unknown`
  - deps.dev returned no license information for the package version

- `not-found`
  - the package/version could not be found in deps.dev

- `unresolved-version`
  - the tool could not normalize the dependency specification into a single version suitable for deps.dev lookup

- `lookup-error`
  - deps.dev lookup failed for that dependency, but the tool continued processing the rest

## Error Handling

### Manifest parsing

If the tool encounters a malformed or unreadable `package.json` or `go.mod`, it prints a warning to `stderr` and continues scanning other files.

### deps.dev lookup failures

If deps.dev lookup fails for a specific dependency:

- that dependency gets `lookup-error`
- a warning is written to `stderr`
- the overall scan continues

This means one failed remote lookup does not abort the full run.

### Hard failures

The tool exits with an error if:

- a target path does not exist
- a local target is not a directory
- a Git repository cannot be cloned
- the deps.dev client cannot be initialized
- invalid flag combinations are used

## Progress Output

While the tool is running, it writes status updates to `stderr`.

Typical messages include:

- processing targets
- completed target count
- discovered manifest count
- resolving licenses
- rendering output

When `stderr` is attached to a terminal, the status line is updated in place instead of printing a new line every time.

The progress display is intentionally aggregate-only so it remains readable while multiple targets are processed concurrently.

Warnings are printed as normal lines so they remain visible.

## Git and SSH Behavior

### Remote repositories

Git repositories are cloned:

- with `go-git`
- shallow (`Depth: 1`)
- entirely in memory

No temporary working directory is created for repository contents.

### SSH authentication

For SSH Git URLs, the tool uses `ssh-agent` by default.

### Host key verification

By default, SSH host keys are verified using normal SSH behavior. If you pass `--insecure-ignore-host-key`, host key verification is disabled.

## Notes and Limitations

- The tool scans repository contents, not lockfiles
- npm license resolution depends on whether the version string can be normalized to a single version
- License values come from deps.dev and should not be treated as legal advice
- The tool currently does not enrich output with transitive dependency trees
- Progress output is designed for interactive terminal use

## License

This project is licensed under the MIT License. See `LICENSE` for details.
