# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Before `v1.0.0`, a minor release may change the Go API. The rendered-output
parity guarantee described in [MIGRATING.md](MIGRATING.md) is separate from Go
API stability and is tied to the embedded go-jsonnet version.

## [Unreleased]

### Added

- ABI 7 is a public, versioned host contract with a machine-readable manifest,
  release `sonnetbox.wasm` and checksum artifacts, and GitHub build-provenance
  attestation.
- `guest.Bytes` and `guest.SHA256` expose defensive access to the canonical
  checked-in guest from Go.
- `rust/sonnetbox-wasmtime` is a Rust 1.94 reference host with default-deny
  WASI, fresh instances, Wasmtime fuel and epoch deadlines, memory and stack
  ceilings, virtual imports, pure native capabilities, bounded traces, and
  all three manifestation modes.
- `abi/v7/conformance.json` drives the same import, capability, trace, file,
  anonymous, multi-file, and stream cases through the Go and Rust hosts.

### Changed

- `EvaluationStats` reports the stable runtime-specific fuel model. The Go
  host uses `wazero-fuel-v1`; the Rust host uses `wasmtime-fuel-v1`.
- The local suite and GitHub Actions validate Rust formatting, Clippy, tests,
  and cross-runtime conformance with the exact `.rust-version` toolchain.

## [0.3.0] - 2026-07-26

This release is about adoption for existing go-jsonnet users. The headline is
that a `FileImporter` with scattered `JPaths` can finally be expressed without
widening the workspace root.

### Added

- `WithSearchRoot` grants a read-only host directory outside the workspace root,
  addressed in virtual paths by a mount name. Each search root is a separate
  `os.Root`, so a Jsonnet program still cannot reach a directory the host did
  not name. Library paths and search roots share one precedence list searched in
  reverse declaration order, matching go-jsonnet `FileImporter` search order.
- `Result.Imports` reports the canonical paths an evaluation resolved,
  deduplicated and in resolution order, bounded by `MaxImports`.
- `(*compat/gojsonnet.VM).FindDependencies` reports the imports an entry point
  pulls in. Unlike go-jsonnet it evaluates rather than parsing, so it reflects
  what the program actually read.
- The `sonnetbox` command accepts `--ext-str-file`, `--ext-code-file`,
  `--tla-str-file`, and `--tla-code-file`. The operator names the file, so the
  host reads it directly rather than routing it through the importer.
- `-v` is an alias for `--version`.
- `make bench` runs startup, snippet, import-tree, capability, and
  large-manifest benchmarks. Measured figures are published in
  [MIGRATING.md](MIGRATING.md).
- [docs/wazero-fork.md](docs/wazero-fork.md) records why the forked wazero
  dependency exists, what it costs, and what an optional upstream build would
  give up.
- Testable godoc examples for search roots, capabilities, both observer forms,
  and the compatibility VM.

### Changed

- The `sonnetbox` command no longer rejects a `-J` path outside `--root`. Such a
  path becomes a search root named after its last path element, numbered when
  names repeat and skipped when the workspace root already uses the name.
- `JSONNET_PATH` is still ignored, but setting it now prints a notice naming
  `-J` instead of silently changing nothing.
- `-V NAME` without a value, `-t`/`--max-trace`, and garbage-collector tuning
  flags are refused by name with the reason and the alternative, instead of a
  generic unrecognized-option error.
- `make conformance` fetches the pinned upstream `google/jsonnet` suite into
  `build/`, so it runs from a bare checkout without a sibling clone. The commit
  is pinned once, by `JSONNET_SUITE_REF` in the Makefile.

### Fixed

- The Jsonnet conformance comparison required only a nonzero exit status for the
  103 fixtures upstream rejects, so an exhausted fuel or memory budget could
  pass as if it were the intended Jsonnet error. Each fixture now requires a
  specific exit status, with the two documented exceptions recorded in an
  allowlist with their reasons.

## [0.2.0] - 2026-07-26

### Added

- `EngineConfig` and `RequestLimits` expose the full policy surface, and
  `ValidatePolicy` checks one without building an engine.
- `WithCompilationCache` and `NewCompilationCacheDir` reuse compiled guest code
  across engines and processes.
- `WithDefaultImporter` and `WithDefaultCapabilities` let an engine carry
  operator-wide defaults that a request may override but not remove.
- `WithObserver` and `NewSlogObserver` report import, capability, and evaluation
  events for auditing.
- The `sonnetbox` command accepts `--policy`, `--print-policy`, per-limit flags,
  and `--error-format=json`.
- Differential conformance coverage against the upstream `google/jsonnet` test
  suite.

### Fixed

- Capability arity is checked at the ABI boundary rather than inside the guest.
- A trace captured before a failure is no longer discarded with the error.
- `NewEngine` reports a canceled context instead of a compilation failure.

## [0.1.2] - 2026-07-25

### Added

- Signed release binaries and signed GHCR container images with SBOM and
  provenance attestations.

## [0.1.1] - 2026-07-25

### Changed

- Documentation and public API reference improvements.

## [0.1.0] - 2026-07-25

Initial release: evaluate untrusted Jsonnet in a fresh WebAssembly sandbox with
`MapImporter`, `WorkspaceImporter`, request capabilities, deterministic fuel
budgeting, and the `sonnetbox` command.

[Unreleased]: https://github.com/thevilledev/sonnetbox/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/thevilledev/sonnetbox/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/thevilledev/sonnetbox/compare/v0.1.2...v0.2.0
[0.1.2]: https://github.com/thevilledev/sonnetbox/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/thevilledev/sonnetbox/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/thevilledev/sonnetbox/releases/tag/v0.1.0
