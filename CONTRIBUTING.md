# Contributing

## Development setup

Use Go 1.25 or newer. The supported development and Wasm build toolchain is
pinned in `.go-version`; golangci-lint is pinned in
`.golangci-lint-version`. The Wasmtime reference host uses the exact Rust
toolchain in `.rust-version`, with its dependency graph locked by
`Cargo.lock`.

Install [prek](https://github.com/j178/prek), then enable the repository's
pre-commit hook:

```sh
prek install
```

The hook rebuilds the embedded Wasm guest when staged Go sources, module
metadata, build settings, or guest artifacts change. If the rebuild updates
the guest, stage `guest/sonnetbox.wasm` and its `.sha256`
file, then commit again.

Before sending a change, run:

```sh
make check
make race
make fuzz-smoke
```

`make check` verifies Go and Rust formatting, module metadata, lint, tests,
coverage, the Cgo-free dependency graph, cross-runtime conformance, and the
checked-in Wasm guest. CI runs the same checks as independent jobs.

To apply the configured Go formatters, run `make fmt`.

For only the Wasmtime host, run:

```sh
make rust-check
```

Shared behavior belongs in `abi/v7/conformance.json` and must be asserted by
both `conformance_test.go` and the Rust conformance test. Runtime-specific
traps and policy backstops stay in the relevant host's tests.

## Updating the embedded guest

Changes to guest code or guest dependencies require rebuilding the checked-in
Wasm reactor:

```sh
make wasm
make wasm-check
```

Commit both `guest/sonnetbox.wasm` and its `.sha256` file.
The build is reproducible with the toolchain pinned in `.go-version`.

## Changes and reviews

Keep changes focused and add tests for observable behavior. Use Conventional
Commit subjects, sign commits, and include a `Signed-off-by` trailer.
Pull requests must pass every required CI job before merge.
